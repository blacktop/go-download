package download

import (
	"testing"
)

// schedEvents captures the under-lock grant/resize notifications.
type schedEvents struct {
	grants  []grantEvent
	resizes []resizeEvent
}

type grantEvent struct {
	id                   int
	off, length, written int64
}

type resizeEvent struct {
	id     int
	length int64
}

func recordEvents(s *scheduler) *schedEvents {
	ev := &schedEvents{}
	s.onGrant = func(id int, off, length, written int64) {
		ev.grants = append(ev.grants, grantEvent{id, off, length, written})
	}
	s.onResize = func(id int, length int64) {
		ev.resizes = append(ev.resizes, resizeEvent{id, length})
	}
	return ev
}

func TestSchedulerPendingThenSplit(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	ev := recordEvents(s)
	s.addPending(0, 1000, 0)

	c1 := s.next()
	if c1 == nil || len(ev.grants) != 1 || ev.grants[0] != (grantEvent{c1.id, 0, 1000, 0}) {
		t.Fatalf("first grant events = %+v, want whole file", ev.grants)
	}
	if len(ev.resizes) != 0 {
		t.Fatalf("pending grant must not resize anything: %v", ev.resizes)
	}

	// Second worker splits the in-flight chunk in half.
	c2 := s.next()
	if c2 == nil {
		t.Fatal("second grant = nil, want a split")
	}
	if len(ev.grants) != 2 || ev.grants[1] != (grantEvent{c2.id, 500, 500, 0}) {
		t.Errorf("split grant events = %+v, want [500,+500)", ev.grants)
	}
	if len(ev.resizes) != 1 || ev.resizes[0] != (resizeEvent{c1.id, 500}) {
		t.Errorf("resizes = %v, want id %d shrunk to 500", ev.resizes, c1.id)
	}
}

func TestSchedulerSplitRespectsCursor(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	ev := recordEvents(s)
	s.addPending(0, 1000, 0)
	c1 := s.next()

	// Owner downloads 600 bytes; remaining is [600,1000).
	if off, n, stop := s.claim(c1, 600); off != 0 || n != 600 || stop {
		t.Fatalf("claim = (%d,%d,%t)", off, n, stop)
	}
	c2 := s.next()
	if c2 == nil {
		t.Fatal("expected split of remaining 400 bytes")
	}
	if got := ev.grants[1]; got != (grantEvent{c2.id, 800, 200, 0}) {
		t.Errorf("split = %+v, want [800,+200)", got)
	}
}

func TestSchedulerMinSizeGate(t *testing.T) {
	t.Parallel()
	s := newScheduler(300)
	s.addPending(0, 1000, 0)
	c1 := s.next()
	if _, _, stop := s.claim(c1, 500); stop {
		t.Fatal("unexpected stop")
	}
	// Remaining 500 < 2*300: no split allowed.
	if c := s.next(); c != nil {
		t.Fatalf("expected nil grant, got %+v", c)
	}
}

func TestSchedulerClaimClampsAfterSteal(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	s.addPending(0, 1000, 0)
	c1 := s.next()
	c2 := s.next() // steals [500,1000)
	if c2 == nil {
		t.Fatal("expected split")
	}

	// Victim has a big buffered read in flight; the claim clamps at 500.
	off, n, stop := s.claim(c1, 900)
	if off != 0 || n != 500 || !stop {
		t.Fatalf("claim = (%d,%d,%t), want (0,500,true)", off, n, stop)
	}
	// Thief claims its full half.
	off, n, stop = s.claim(c2, 500)
	if off != 500 || n != 500 || !stop {
		t.Fatalf("thief claim = (%d,%d,%t), want (500,500,true)", off, n, stop)
	}
}

func TestSchedulerResumePendingOrder(t *testing.T) {
	t.Parallel()
	s := newScheduler(10)
	ev := recordEvents(s)
	s.addPending(0, 100, 40)
	s.addPending(500, 600, 0)

	c := s.next()
	if got := ev.grants[0]; got != (grantEvent{c.id, 0, 100, 40}) {
		t.Fatalf("grant = %+v, want resumed chunk with written=40", got)
	}
	if off, end, todo := s.cursor(c); off != 40 || end != 100 || !todo {
		t.Fatalf("cursor = (%d,%d,%t)", off, end, todo)
	}
}

func TestSchedulerIdleAndSnapshot(t *testing.T) {
	t.Parallel()
	s := newScheduler(10)
	s.addPending(0, 100, 0)
	if s.idle() {
		t.Fatal("scheduler with pending work reported idle")
	}
	c := s.next()
	snap := s.snapshot()
	if len(snap) != 1 || snap[0].Off != 0 || snap[0].End != 100 || snap[0].Done != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, n, stop := s.claim(c, 100); n != 100 || !stop {
		t.Fatal("claim did not complete chunk")
	}
	c.written.Store(100)
	s.complete(c)
	if !s.idle() {
		t.Fatal("scheduler with no work reported busy")
	}
	if snap := s.snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot after completion = %+v", snap)
	}
}
