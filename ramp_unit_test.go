package download

import (
	"log/slog"
	"testing"
	"time"
)

// rampHarness drives noteLocked purely with injected byte totals and times.
type rampHarness struct {
	rs    *rampState
	total int64
	at    time.Time
}

func newRampHarness(parts int, window int64) *rampHarness {
	start := time.Unix(1000, 0)
	h := &rampHarness{
		rs: &rampState{
			enabled:   true,
			log:       slog.New(slog.DiscardHandler),
			parts:     parts,
			window:    window,
			settleMin: rampSettleCap,
			admitted:  1,
			markTime:  start,
		},
		at: start,
	}
	h.rs.now = func() time.Time { return h.at }
	return h
}

// takeRecord drains the pending decision record the way note() would.
func (h *rampHarness) takeRecord() *rampDecision {
	rec := h.rs.pending
	h.rs.pending = nil
	return rec
}

// window advances exactly one full byte window at the given rate (bytes/sec)
// and returns noteLocked's side effects.
func (h *rampHarness) window(rate float64) (spawnFrom, spawnN, demoteTo int) {
	h.total += h.rs.window
	h.at = h.at.Add(time.Duration(float64(h.rs.window) / rate * float64(time.Second)))
	return h.rs.noteLocked(h.total, h.at)
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

// TestDecisionRecordEveryAction is the deterministic record oracle: each
// governor action produces exactly one pending record with the schema's
// decision-specific fields filled.
func TestDecisionRecordEveryAction(t *testing.T) {
	t.Parallel()

	t.Run("first admission", func(t *testing.T) {
		t.Parallel()
		h := newRampHarness(4, 1000)
		h.window(100) // burn-in: no record
		if rec := h.takeRecord(); rec != nil {
			t.Fatalf("burn-in produced a record: %+v", rec)
		}
		h.window(100) // baseline → admit
		rec := h.takeRecord()
		if rec == nil {
			t.Fatal("first admission produced no record")
		}
		if rec.action != "admit" || rec.reason != "first-batch-probe" ||
			rec.created.prior != 1 || rec.created.admitted != 2 || rec.qValid ||
			rec.judged.size() != 0 || rec.seq != 1 ||
			rec.created.final || rec.created.clamped {
			t.Fatalf("first admission record = %+v", rec)
		}
	})

	t.Run("demote", func(t *testing.T) {
		t.Parallel()
		h := newRampHarness(4, 1000)
		h.window(100)
		h.window(100) // admit 1->2
		h.takeRecord()
		h.window(100) // settle
		h.measure(100)
		rec := h.takeRecord()
		if rec == nil {
			t.Fatal("demotion produced no record")
		}
		if rec.action != "demote" || rec.reason != "batch-not-paying" ||
			rec.judged.prior != 1 || rec.judged.admitted != 2 || !rec.qValid {
			t.Fatalf("demote record = %+v", rec)
		}
		if rec.q < 0.9 || rec.q > 1.02 {
			t.Fatalf("flat demotion q = %v", rec.q)
		}
	})

	t.Run("freeze", func(t *testing.T) {
		t.Parallel()
		h := newRampHarness(4, 1000)
		h.window(100)
		h.window(100)
		h.takeRecord()
		h.window(100) // settle
		h.measure(110)
		rec := h.takeRecord()
		if rec == nil || rec.action != "freeze" || rec.reason != "marginal-gain" ||
			rec.judged.prior != 1 || rec.judged.admitted != 2 || !rec.qValid {
			t.Fatalf("freeze record = %+v", rec)
		}
		if rec.q < 1.05 || rec.q >= rampImprovement {
			t.Fatalf("freeze q = %v", rec.q)
		}
	})

	t.Run("expand then keep-final with readiness", func(t *testing.T) {
		t.Parallel()
		h := newRampHarness(4, 1000)
		h.window(100)
		h.window(100) // admit 1->2
		h.takeRecord()
		spawnedAt := h.rs.admittedAt
		h.window(230)                      // settle
		h.rs.noteWorkerReady(1, spawnedAt) // batch worker 1 became productive
		h.measure(230)                     // clear gain → admit 2->4
		rec := h.takeRecord()
		if rec == nil || rec.action != "admit" || rec.reason != "clear-gain" ||
			rec.judged.prior != 1 || rec.judged.admitted != 2 ||
			rec.created.prior != 2 || rec.created.admitted != 4 || !rec.qValid {
			t.Fatalf("expand record = %+v", rec)
		}
		if rec.q < 2.0 || rec.q > 2.6 {
			t.Fatalf("expand q = %v", rec.q)
		}
		if len(rec.readyLat) != 1 {
			t.Fatalf("expand readiness = %+v (judged batch 1->2 had one worker ready)", rec)
		}
		if rec.readyLat[0] <= 0 {
			t.Fatalf("readiness latency = %v, want positive", rec.readyLat[0])
		}

		// Workers 2 and 3 of the final batch become productive; only they
		// may appear in the final record.
		spawnedAt = h.rs.admittedAt
		h.rs.noteWorkerReady(2, spawnedAt)
		h.rs.noteWorkerReady(3, spawnedAt)
		h.rs.noteWorkerReady(1, spawnedAt) // stale id outside the current batch: ignored
		h.window(520)                      // settle
		h.measure(520)
		rec = h.takeRecord()
		if rec == nil || rec.action != "keep-final" || rec.reason != "final-batch-paying" ||
			rec.judged.prior != 2 || rec.judged.admitted != 4 || !rec.judged.final {
			t.Fatalf("keep-final record = %+v", rec)
		}
		if len(rec.readyLat) != 2 {
			t.Fatalf("keep-final readiness = %+v, want exactly the final batch's two workers", rec)
		}
		if !h.rs.done.Load() {
			t.Fatal("keep-final must finish the ramp")
		}
	})
}

func TestDecisionTelemetryDisabledIsInert(t *testing.T) {
	t.Parallel()
	h := newRampHarness(4, 1000)
	h.rs.enabled = false
	h.window(100)
	h.window(100)
	if h.rs.pending != nil || h.rs.seq != 0 || h.rs.ready != nil {
		t.Fatalf("disabled telemetry retained state: pending=%+v seq=%d ready=%v",
			h.rs.pending, h.rs.seq, h.rs.ready)
	}
}

func TestWorkerReadinessUsesActualSpawnTime(t *testing.T) {
	t.Parallel()
	h := newRampHarness(2, 1000)
	h.window(100)
	h.window(100) // admit worker 1
	h.takeRecord()

	// The admission decision happened much earlier than the worker spawn.
	// Only the interval from actual goroutine submission may be recorded.
	h.at = h.at.Add(2 * time.Second)
	spawnedAt := h.at.Add(-37 * time.Millisecond)
	h.rs.noteWorkerReady(1, spawnedAt)
	if got := h.rs.ready[1]; got != 37*time.Millisecond {
		t.Fatalf("readiness = %v, want 37ms from actual spawn", got)
	}
}
