package download

import (
	"testing"
	"time"
)

// rampHarness drives noteLocked purely with injected byte totals and times.
type rampHarness struct {
	rs        *rampState
	total     int64
	at        time.Time
	autoReady bool
}

func newRampHarness(parts int, window int64) *rampHarness {
	start := time.Unix(1000, 0)
	h := &rampHarness{
		rs: &rampState{
			parts:     parts,
			window:    window,
			settleMin: rampSettleCap,
			admitted:  1,
			markTime:  start,
		},
		at:        start,
		autoReady: true,
	}
	h.rs.now = func() time.Time { return h.at }
	return h
}

func TestRampEligible(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		remaining int64
		minPart   int64
		parts     int
		want      bool
	}{
		{name: "one part", remaining: 1 << 30, minPart: 16 << 20, parts: 1},
		{name: "invalid minimum", remaining: 1 << 30, parts: 8},
		{name: "one byte short", remaining: 128<<20 - 1, minPart: 16 << 20, parts: 8},
		{name: "exact runway", remaining: 128 << 20, minPart: 16 << 20, parts: 8, want: true},
		{name: "large remainder", remaining: 1 << 30, minPart: 16 << 20, parts: 8, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rampEligible(tc.remaining, tc.minPart, tc.parts); got != tc.want {
				t.Fatalf("rampEligible(%d, %d, %d) = %t, want %t",
					tc.remaining, tc.minPart, tc.parts, got, tc.want)
			}
		})
	}
}

// window advances exactly one full byte window at the given rate (bytes/sec)
// and returns noteLocked's side effects.
func (h *rampHarness) window(rate float64) (spawnFrom, spawnN, demoteTo int) {
	h.total += h.rs.window
	h.at = h.at.Add(time.Duration(float64(h.rs.window) / rate * float64(time.Second)))
	spawnFrom, spawnN, demoteTo = h.rs.noteLocked(h.total, h.at)
	if h.autoReady {
		for i := range spawnN {
			h.rs.noteWorkerReady(spawnFrom + i)
		}
	}
	return spawnFrom, spawnN, demoteTo
}

func (h *rampHarness) measure(rate float64) (spawnFrom, spawnN, demoteTo int) {
	for range rampMeasureWindows {
		spawnFrom, spawnN, demoteTo = h.window(rate)
	}
	return spawnFrom, spawnN, demoteTo
}

func TestRampDemotesOnSettledFlatThroughput(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)

	if _, n, d := h.window(100); n != 0 || d != 0 {
		t.Fatal("burn-in window must have no side effects")
	}
	from, n, d := h.window(100) // baseline window → probe batch
	if from != 1 || n != 1 || d != 0 {
		t.Fatalf("first admission = (%d,%d,%d), want spawn worker 1", from, n, d)
	}
	if !h.rs.settling {
		t.Fatal("admission must arm settling")
	}
	// Settling windows (wall floor satisfied by the slow rate: each window
	// at rate 100 B/s over 1000 B lasts 10s >> the settle floor).
	if _, n, d := h.window(100); n != 0 || d != 0 {
		t.Fatal("settling window must have no side effects")
	}
	// Stabilized decision: flat throughput (no gain over baseline) → demote.
	_, n, d = h.measure(100)
	if n != 0 || d != 1 {
		t.Fatalf("flat decision = (n=%d, demote=%d), want demote to 1", n, d)
	}
	if !h.rs.done.Load() {
		t.Fatal("demotion must finish the ramp")
	}
	// Once done, further windows are inert.
	if _, n, d := h.window(100); n != 0 || d != 0 {
		t.Fatal("done ramp acted again")
	}
}

func TestRampFreezeBandKeepsFlows(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)
	h.window(100) // burn-in
	h.window(100) // baseline + admit
	h.window(100) // settle
	// 1.10x gain: marginal band — keep the flows, stop admitting.
	if _, n, d := h.measure(110); n != 0 || d != 0 {
		t.Fatal("freeze band must neither spawn nor demote")
	}
	if !h.rs.done.Load() || h.rs.admitted != 2 {
		t.Fatalf("freeze band: done=%t admitted=%d, want done with 2 flows",
			h.rs.done.Load(), h.rs.admitted)
	}
}

func TestRampDoesNotDemoteDuringAdmissionSettling(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)
	h.window(100) // burn-in
	h.window(100) // baseline + admit (batch dialing)
	baseline := h.rs.admissionBaseline

	// Fast link: byte windows complete long before the settle wall floor.
	// Rates are flat — a premature decision would demote — but settling
	// must hold until the settle floor has passed since admission.
	for range 5 {
		h.total += h.rs.window
		h.at = h.at.Add(time.Millisecond) // 1ms per window: way under floor
		if _, n, d := h.rs.noteLocked(h.total, h.at); n != 0 || d != 0 {
			t.Fatal("decision taken while batch still settling")
		}
		if !h.rs.settling {
			t.Fatal("settling released before the wall floor")
		}
	}
	// Past the floor the next window releases settling; subsequent stabilized
	// measurement windows make the decision.
	h.total += h.rs.window
	h.at = h.at.Add(rampSettleCap)
	if _, n, d := h.rs.noteLocked(h.total, h.at); n != 0 || d != 0 {
		t.Fatal("settling-release window must not decide")
	}
	if h.rs.settling {
		t.Fatal("settling not released after the wall floor")
	}
	if h.rs.admissionBaseline != baseline {
		t.Fatal("settling windows must not touch the admission baseline")
	}
}

func TestRampRejectsBatchThatNeverContributes(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)
	h.autoReady = false
	h.window(100) // burn-in
	h.window(100) // baseline + admit worker 1

	// The original flow speeds up enough to clear the admission threshold,
	// but worker 1 has delivered nothing. None of that gain may be credited
	// to the new batch, regardless of elapsed wall time or byte windows.
	for range rampMeasureWindows {
		if _, n, d := h.window(230); n != 0 || d != 0 {
			t.Fatalf("unready batch acted: spawn=%d demote=%d", n, d)
		}
		if !h.rs.settling || h.rs.admitted != 2 || h.rs.done.Load() {
			t.Fatalf("unready state: settling=%t admitted=%d done=%t",
				h.rs.settling, h.rs.admitted, h.rs.done.Load())
		}
	}
	if _, n, d := h.window(230); n != 0 || d != 1 {
		t.Fatalf("expired unready batch = (spawn=%d, demote=%d), want demote to 1", n, d)
	}
	if !h.rs.done.Load() {
		t.Fatal("rejecting an unready batch must finish the ramp")
	}
}

func TestRampWaitsForAdmittedWorkerContribution(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)
	h.autoReady = false
	h.window(100)
	h.window(100)

	for range rampMeasureWindows {
		if _, n, d := h.window(230); n != 0 || d != 0 {
			t.Fatalf("unready batch acted: spawn=%d demote=%d", n, d)
		}
	}

	h.rs.noteWorkerReady(1)
	h.window(230) // release settling; this window is not a decision sample
	if h.rs.settling {
		t.Fatal("settling did not release after the admitted worker contributed")
	}
	if from, n, d := h.measure(230); from != 2 || n != 2 || d != 0 {
		t.Fatalf("ready paying batch = (%d,%d,%d), want admit workers 2..3", from, n, d)
	}
	if h.rs.batchReady {
		t.Fatal("new admission inherited the previous batch's readiness")
	}
	h.rs.noteWorkerReady(1)
	if h.rs.batchReady {
		t.Fatal("worker outside the current batch marked it ready")
	}
	h.rs.noteWorkerReady(2)
	if !h.rs.batchReady {
		t.Fatal("worker in the current batch did not mark it ready")
	}
}

func TestSettlingWindowAdvancesMeasurementMarks(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)
	h.window(100) // burn-in
	h.window(100) // baseline + admit
	markAt, markTime := h.rs.markAt, h.rs.markTime
	h.window(100) // settling
	if h.rs.markAt == markAt || !h.rs.markTime.After(markTime) {
		t.Fatal("settling window must advance measurement marks")
	}
	// The decision window therefore measures ONLY post-settling bytes: a
	// startup-slow settling window followed by a healthy decision window
	// must not be averaged together into a false demotion.
	if _, _, d := h.measure(230); d != 0 { // 2.3x baseline: clear gain
		t.Fatal("healthy post-settling window judged by stale marks")
	}
	if h.rs.admitted != 4 {
		t.Fatalf("admitted = %d, want next batch after clear gain", h.rs.admitted)
	}
}

func TestPartialBatchDemotesToExactPreviousCount(t *testing.T) {
	t.Parallel()
	h := newRampHarness(6, 1000)
	h.window(100)                // burn-in
	h.window(100)                // baseline → admit to 2
	h.window(100)                // settle
	h.measure(230)               // decide: gain → admit to 4
	h.window(230)                // settle
	from, n, _ := h.measure(520) // decide: gain → admit final clamped batch (4→6)
	if from != 4 || n != 2 || h.rs.admitted != 6 {
		t.Fatalf("final batch = from %d n %d admitted %d, want 4..5 admitted to 6", from, n, h.rs.admitted)
	}
	if h.rs.prevAdmitted != 4 {
		t.Fatalf("prevAdmitted = %d, want 4 (recorded at admission, not admitted/2)", h.rs.prevAdmitted)
	}
	h.window(520) // settle
	// Final clamped batch is flat → demote to exactly 4, not 3.
	if _, _, d := h.measure(520); d != 4 {
		t.Fatalf("flat final batch demoted to %d, want prevAdmitted=4", d)
	}
}

func TestFinalAdmissionBatchIsStillEvaluated(t *testing.T) {
	t.Parallel()
	// Parts=2: the first probe IS the final batch; reaching parts must not
	// set done until it passes settling + decision.
	h := newRampHarness(2, 1000)
	h.window(100) // burn-in
	h.window(100) // baseline → admit to 2 == parts
	if h.rs.done.Load() {
		t.Fatal("reaching parts must not finish the ramp before evaluation")
	}
	h.window(100) // settle
	if h.rs.done.Load() {
		t.Fatal("settling must not finish the ramp")
	}
	if _, _, d := h.measure(100); d != 1 {
		t.Fatalf("flat final batch demoted to %d, want 1", d)
	}

	// And a paying final batch is kept.
	h2 := newRampHarness(2, 1000)
	h2.window(100)
	h2.window(100)
	h2.window(100) // settle
	if _, _, d := h2.measure(190); d != 0 {
		t.Fatal("paying final batch must not demote")
	}
	if !h2.rs.done.Load() || h2.rs.admitted != 2 {
		t.Fatal("paying final batch must be kept and finish the ramp")
	}
}

func TestRampDemotionUsesStabilizedRate(t *testing.T) {
	t.Parallel()
	h := newRampHarness(2, 1000)
	h.window(100) // burn-in
	h.window(100) // baseline + final admission
	h.window(100) // settle

	// The first short sample alone looks like a paying 4% gain. It must not
	// decide; paired with the compensating slow sample, aggregate throughput is
	// flat and the batch must retire.
	if _, n, d := h.window(104); n != 0 || d != 0 || h.rs.done.Load() {
		t.Fatal("first noisy decision sample acted before stabilization")
	}
	if _, n, d := h.window(96); n != 0 || d != 1 {
		t.Fatalf("stabilized flat decision = (n=%d, demote=%d), want demote to 1", n, d)
	}
}

func TestDemoteSkipsDrainedChunks(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	ev := recordEvents(s)
	s.addPending(0, 1000, 0)
	s.addPending(1000, 2000, 0)
	c0, c1 := s.next(0), s.next(1)
	s.claim(c1, 1000) // worker 1's chunk fully claimed (drained), not completed

	victims := s.demote(1)
	if len(victims) != 1 || victims[0] != 1 {
		t.Fatalf("victims = %v, want {1}", victims)
	}
	// Drained chunk: nothing to move, no resize, no new pending.
	if len(ev.resizes) != 0 {
		t.Fatalf("drained victim resized: %v", ev.resizes)
	}
	before := s.remainingBytes()
	if before != 1000 {
		t.Fatalf("remainingBytes = %d, want only worker 0's chunk", before)
	}
	_ = c0
}

func TestSnapshotPreservesClaimedNotWritten(t *testing.T) {
	t.Parallel()
	s := newScheduler(1)
	s.addPending(0, 1000, 0)
	c := s.next(0)
	// 600 bytes claimed, only 400 written (WriteAt still in flight), then
	// the owner is demoted: the chunk shrinks to the claim cursor.
	s.claim(c, 600)
	c.written.Store(400)
	victims := s.demote(1)
	if len(victims) != 0 { // sole survivor is never retired
		t.Fatalf("victims = %v", victims)
	}
	// Force the shrink via a second worker's demotion instead.
	s2 := newScheduler(1)
	s2.addPending(0, 1000, 0)
	s2.addPending(5000, 6000, 0)
	k0, k1 := s2.next(0), s2.next(1)
	s2.claim(k1, 600)
	k1.written.Store(400)
	if v := s2.demote(1); len(v) != 1 || v[0] != 1 {
		t.Fatalf("victims = %v, want {1}", v)
	}
	// Durable/resume space: snapshot must still cover [written, done) of
	// the shrunken victim — the claimed-but-unwritten span [400,600) —
	// alongside the moved remainder [600,1000).
	snap := s2.snapshot()
	covered := func(off, end int64) bool {
		for _, cs := range snap {
			if cs.Off <= off && end <= cs.End && cs.Done <= off-cs.Off {
				return true
			}
		}
		return false
	}
	if !covered(400, 600) {
		t.Fatalf("snapshot lost claimed-but-unwritten span: %+v", snap)
	}
	if !covered(600, 1000) {
		t.Fatalf("snapshot lost moved remainder: %+v", snap)
	}
	_ = k0
}

// TestSettleFloorClampBounds pins the derived settle floor: twice the
// election round-trip, never below the fixed floor, never above the cap.
func TestSettleFloorClampBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		elect time.Duration
		want  time.Duration
	}{
		{"zero measurement keeps the floor", 0, rampSettleFloor},
		{"sub-floor measurement keeps the floor", 400 * time.Microsecond, rampSettleFloor},
		{"exactly half the floor doubles to the floor", rampSettleFloor / 2, rampSettleFloor},
		{"mid-range doubles", 10 * time.Millisecond, 20 * time.Millisecond},
		{"exactly half the cap doubles to the cap", rampSettleCap / 2, rampSettleCap},
		{"above half the cap clamps to the cap", 150 * time.Millisecond, rampSettleCap},
		{"pathological measurement clamps to the cap", 30 * time.Second, rampSettleCap},
	}
	for _, tc := range cases {
		if got := settleFloorFor(tc.elect); got != tc.want {
			t.Errorf("%s: settleFloorFor(%v) = %v, want %v", tc.name, tc.elect, got, tc.want)
		}
	}
}
