package download

import (
	"testing"
)

func TestSchedulerPendingThenSplit(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	s.addPending(0, 1000, 0)

	g1 := s.next()
	if g1 == nil || g1.off != 0 || g1.length != 1000 || g1.victim != nil {
		t.Fatalf("first grant = %+v, want whole file", g1)
	}

	// Second worker splits the in-flight chunk in half.
	g2 := s.next()
	if g2 == nil || g2.victim == nil {
		t.Fatalf("second grant = %+v, want a split", g2)
	}
	if g2.off != 500 || g2.length != 500 {
		t.Errorf("split grant = [%d,+%d), want [500,+500)", g2.off, g2.length)
	}
	if g2.victim.id != g1.c.id || g2.victim.length != 500 {
		t.Errorf("victim = %+v, want id %d shrunk to 500", g2.victim, g1.c.id)
	}
}

func TestSchedulerSplitRespectsCursor(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	s.addPending(0, 1000, 0)
	g1 := s.next()

	// Owner downloads 600 bytes; remaining is [600,1000).
	if off, n, stop := s.claim(g1.c, 600); off != 0 || n != 600 || stop {
		t.Fatalf("claim = (%d,%d,%t)", off, n, stop)
	}
	g2 := s.next()
	if g2 == nil {
		t.Fatal("expected split of remaining 400 bytes")
	}
	if g2.off != 800 || g2.length != 200 {
		t.Errorf("split = [%d,+%d), want [800,+200)", g2.off, g2.length)
	}
}

func TestSchedulerMinSizeGate(t *testing.T) {
	t.Parallel()
	s := newScheduler(300)
	s.addPending(0, 1000, 0)
	g1 := s.next()
	if _, _, stop := s.claim(g1.c, 500); stop {
		t.Fatal("unexpected stop")
	}
	// Remaining 500 < 2*300: no split allowed.
	if g := s.next(); g != nil {
		t.Fatalf("expected nil grant, got %+v", g)
	}
}

func TestSchedulerClaimClampsAfterSteal(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	s.addPending(0, 1000, 0)
	g1 := s.next()
	g2 := s.next() // steals [500,1000)
	if g2 == nil {
		t.Fatal("expected split")
	}

	// Victim has a big buffered read in flight; the claim clamps at 500.
	off, n, stop := s.claim(g1.c, 900)
	if off != 0 || n != 500 || !stop {
		t.Fatalf("claim = (%d,%d,%t), want (0,500,true)", off, n, stop)
	}
	// Thief claims its full half.
	off, n, stop = s.claim(g2.c, 500)
	if off != 500 || n != 500 || !stop {
		t.Fatalf("thief claim = (%d,%d,%t), want (500,500,true)", off, n, stop)
	}
}

func TestSchedulerResumePendingOrder(t *testing.T) {
	t.Parallel()
	s := newScheduler(10)
	s.addPending(0, 100, 40)
	s.addPending(500, 600, 0)

	g := s.next()
	if g.off != 0 || g.written != 40 || g.length != 100 {
		t.Fatalf("grant = %+v, want resumed chunk with written=40", g)
	}
	if off, end, todo := s.cursor(g.c); off != 40 || end != 100 || !todo {
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
	g := s.next()
	snap := s.snapshot()
	if len(snap) != 1 || snap[0].Off != 0 || snap[0].End != 100 || snap[0].Done != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, n, stop := s.claim(g.c, 100); n != 100 || !stop {
		t.Fatal("claim did not complete chunk")
	}
	g.c.written.Store(100)
	s.complete(g.c)
	if !s.idle() {
		t.Fatal("scheduler with no work reported busy")
	}
	if snap := s.snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot after completion = %+v", snap)
	}
}
