package download

import (
	"sync"
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

	c1 := s.next(0)
	if c1 == nil || len(ev.grants) != 1 || ev.grants[0] != (grantEvent{c1.id, 0, 1000, 0}) {
		t.Fatalf("first grant events = %+v, want whole file", ev.grants)
	}
	if len(ev.resizes) != 0 {
		t.Fatalf("pending grant must not resize anything: %v", ev.resizes)
	}

	// Second worker splits the in-flight chunk in half.
	c2 := s.next(1)
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
	c1 := s.next(0)

	// Owner downloads 600 bytes; remaining is [600,1000).
	if off, n, stop := s.claim(c1, 600); off != 0 || n != 600 || stop {
		t.Fatalf("claim = (%d,%d,%t)", off, n, stop)
	}
	c2 := s.next(1)
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
	c1 := s.next(0)
	if _, _, stop := s.claim(c1, 500); stop {
		t.Fatal("unexpected stop")
	}
	// Remaining 500 < 2*300: no split allowed.
	if c := s.next(1); c != nil {
		t.Fatalf("expected nil grant, got %+v", c)
	}
}

func TestSchedulerClaimClampsAfterSteal(t *testing.T) {
	t.Parallel()
	s := newScheduler(100)
	s.addPending(0, 1000, 0)
	c1 := s.next(0)
	c2 := s.next(1) // steals [500,1000)
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

	c := s.next(0)
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
	c := s.next(0)
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

func TestNextRefusesAboveLimit(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	ev := recordEvents(s)
	for i := range 4 {
		s.addPending(int64(i*100), int64(i*100+100), 0)
	}
	c0, c1 := s.next(0), s.next(1)
	if c0 == nil || c1 == nil {
		t.Fatal("setup grants failed")
	}
	if victims := s.demote(2); len(victims) != 0 {
		t.Fatalf("demote(2) with 2 live workers returned victims %v", victims)
	}

	// Registration above the limit is refused and deregistered.
	if c := s.next(2); c != nil {
		t.Fatalf("worker 2 granted %+v above limit", c)
	}
	if c := s.next(3); c != nil {
		t.Fatal("worker 3 granted above limit")
	}
	// Refusal deregistered them: existing workers keep working.
	s.complete(c0)
	if c := s.next(0); c == nil {
		t.Fatal("worker 0 refused below limit")
	}
	// Count semantics, not id semantics: after the current workers drain,
	// a previously refused high id is granted again.
	s.complete(c1)
	if c := s.next(1); c == nil {
		t.Fatal("worker 1 refused below limit")
	}
	// Drain workers 0 and 1 completely, then add fresh work: a previously
	// refused high id must be granted once the live count is under limit.
	for _, id := range []int{0, 1} {
		for {
			c := s.next(id)
			if c == nil {
				break
			}
			s.claim(c, 1000)
			s.complete(c)
		}
	}
	s.addPending(4000, 5000, 0)
	if c := s.next(3); c == nil {
		t.Fatal("worker 3 refused with zero other live workers")
	}
	_ = ev
}

func TestUnlimitedLimitCanBeLowered(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	s.demote(3)
	if s.limit != 3 {
		t.Fatalf("limit = %d after demote(3) from unlimited, want 3", s.limit)
	}
	s.demote(5)
	if s.limit != 3 {
		t.Fatalf("limit = %d after demote(5), must never raise", s.limit)
	}
	s.demote(0)
	if s.limit != 1 {
		t.Fatalf("limit = %d after demote(0), want floor 1", s.limit)
	}
}

func TestDemoteNeverRetiresLastHighID(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	ev := recordEvents(s)
	s.addPending(0, 1000, 0)
	c := s.next(7) // only survivor has a high id
	if c == nil {
		t.Fatal("grant failed")
	}
	s.claim(c, 100)
	if victims := s.demote(1); len(victims) != 0 {
		t.Fatalf("demote(1) retired the sole survivor: %v", victims)
	}
	if len(ev.resizes) != 0 {
		t.Fatalf("sole survivor's chunk was shrunk: %v", ev.resizes)
	}
	if _, end, todo := s.cursor(c); end != 1000 || !todo {
		t.Fatalf("sole survivor chunk altered: end=%d todo=%t", end, todo)
	}
	// The survivor finishes its chunk normally and drains.
	s.claim(c, 900)
	s.complete(c)
	if c2 := s.next(7); c2 != nil {
		t.Fatalf("sole survivor granted unexpected work %+v", c2)
	}
	if !s.idle() {
		t.Fatal("work remained after sole survivor finished")
	}
}

func TestDemoteChoosesExactExcessVictims(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	ev := recordEvents(s)
	chunks := make(map[int]*chunk)
	for i := range 4 {
		s.addPending(int64(i*1000), int64(i*1000+1000), 0)
	}
	for i := range 4 {
		c := s.next(i)
		if c == nil {
			t.Fatal("setup grant failed")
		}
		s.claim(c, 100) // every owner has a 100-byte cursor
		chunks[i] = c
	}
	before := s.remainingBytes()

	victims := s.demote(2)
	if len(victims) != 2 {
		t.Fatalf("victims = %v, want exactly 2", victims)
	}
	seen := map[int]bool{}
	for _, v := range victims {
		seen[v] = true
	}
	if !seen[2] || !seen[3] {
		t.Fatalf("victims = %v, want the highest ids {2,3}", victims)
	}
	if got := s.remainingBytes(); got != before {
		t.Fatalf("remainingBytes changed: %d -> %d", before, got)
	}
	// Victims' chunks shrunk to their cursors; keepers untouched.
	for _, id := range []int{2, 3} {
		if _, _, todo := s.cursor(chunks[id]); todo {
			t.Errorf("victim %d's chunk still has work", id)
		}
	}
	for _, id := range []int{0, 1} {
		if _, end, todo := s.cursor(chunks[id]); !todo || end != int64(id*1000+1000) {
			t.Errorf("keeper %d's chunk was altered", id)
		}
	}
	if len(ev.resizes) != 2 {
		t.Errorf("resizes = %v, want exactly the two victims", ev.resizes)
	}
	// The two moved remainders are pending, granted to keepers with fresh
	// ids and one ChunkStart each.
	grantsBefore := len(ev.grants)
	s.complete(chunks[0])
	got0 := s.next(0)
	s.complete(chunks[1])
	got1 := s.next(1)
	if got0 == nil || got1 == nil {
		t.Fatal("keepers were not granted the moved remainders")
	}
	if len(ev.grants) != grantsBefore+2 {
		t.Errorf("moved remainders emitted %d grants, want 2", len(ev.grants)-grantsBefore)
	}
	// Victims exit via the retiring branch.
	if c := s.next(2); c != nil {
		t.Fatal("retiring worker 2 was granted work")
	}
	if c := s.next(3); c != nil {
		t.Fatal("retiring worker 3 was granted work")
	}
}

func TestDemoteRaceTwoSurvivors(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	// Low-id keepers already exited; only high ids remain.
	s.addPending(0, 1000, 0)
	s.addPending(1000, 2000, 0)
	c4, c5 := s.next(4), s.next(5)
	if c4 == nil || c5 == nil {
		t.Fatal("setup grants failed")
	}
	// demote(2) with exactly two non-retiring live workers: no victims, and
	// both survivors keep working regardless of their ids.
	if victims := s.demote(2); len(victims) != 0 {
		t.Fatalf("demote(2) returned victims %v", victims)
	}
	var wg sync.WaitGroup
	for id, c := range map[int]*chunk{4: c4, 5: c5} {
		wg.Go(func() {
			s.claim(c, 1000)
			s.complete(c)
			for {
				n := s.next(id)
				if n == nil {
					return
				}
				s.claim(n, 2000)
				s.complete(n)
			}
		})
	}
	wg.Wait()
	if !s.idle() {
		t.Fatal("work stranded after concurrent drain at the limit")
	}

	// And demote(1) on two high-id survivors retires exactly one.
	s2 := newScheduler(1)
	s2.addPending(0, 1000, 0)
	s2.addPending(1000, 2000, 0)
	k4, k5 := s2.next(4), s2.next(5)
	if k4 == nil || k5 == nil {
		t.Fatal("setup grants failed")
	}
	victims := s2.demote(1)
	if len(victims) != 1 || victims[0] != 5 {
		t.Fatalf("demote(1) victims = %v, want {5}", victims)
	}
	// Worker 5 finishes its shrunk chunk and exits; worker 4 completes its
	// own chunk first (as a real worker does), then drains everything.
	s2.claim(k5, 2000)
	s2.complete(k5)
	if c := s2.next(5); c != nil {
		t.Fatal("retired worker granted work")
	}
	s2.claim(k4, 2000)
	s2.complete(k4)
	for {
		c := s2.next(4)
		if c == nil {
			break
		}
		s2.claim(c, 2000)
		s2.complete(c)
	}
	if !s2.idle() {
		t.Fatal("work stranded after single-survivor drain")
	}
}
