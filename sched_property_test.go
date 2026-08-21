package download

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// The property suite drives the REAL scheduler through generated operation
// traces with simulated (non-goroutine, deterministic) workers, checking the
// plan's invariants after every operation:
//
//	P1 assignment space: pending+active [off+done,end) intervals are
//	   disjoint and account exactly for every unclaimed byte
//	P2 durable space: flushed bytes plus snapshot intervals partition
//	   [0,total) without gaps even while written trails done
//	P3 liveness: work never strands — while bytes remain, some worker can
//	   still take them
//	P4 reporter: grants exactly once per id; resizes only for granted ids,
//	   never growing; every granted id eventually completes
//	P5 bounded drain: an error-free run finishes within a step bound
//
// The model tracks claims with a per-byte ledger, deliberately NOT reusing
// the production interval or victim-selection logic.

type simOp struct {
	kind   string // "next", "claim", "flush", "complete", "demote", "newWorker"
	worker int
	arg    int
}

func (o simOp) String() string { return fmt.Sprintf("%s(w%d,%d)", o.kind, o.worker, o.arg) }

type simWorker struct {
	id      int
	chunk   *chunk
	exited  bool
	unflush int64 // claimed bytes not yet marked written
}

// simFailer abstracts the failure sink so the simulator runs under both
// *testing.T and the trace-minimization probe.
type simFailer interface{ Fatalf(string, ...any) }

type sim struct {
	t       simFailer
	s       *scheduler
	total   int64
	claimed []bool // per-byte: handed out by claim
	flushed []bool // per-byte: written.Store'd (durable model)
	workers map[int]*simWorker
	granted map[int]int   // chunk id → grant count
	length  map[int]int64 // chunk id → last known length (grant/resize)
	steps   int
}

func newSim(t simFailer, total int64, minSize int64, resumeGaps [][2]int64) *sim {
	sm := &sim{
		t: t, s: newScheduler(minSize), total: total,
		claimed: make([]bool, total), flushed: make([]bool, total),
		workers: map[int]*simWorker{},
		granted: map[int]int{}, length: map[int]int64{},
	}
	sm.s.onGrant = func(id int, off, length, written int64) {
		sm.granted[id]++
		if sm.granted[id] > 1 {
			t.Fatalf("P4: chunk %d granted %d times", id, sm.granted[id])
		}
		sm.length[id] = length
	}
	sm.s.onResize = func(id int, length int64) {
		if sm.granted[id] == 0 {
			t.Fatalf("P4: resize for never-granted chunk %d", id)
		}
		if length > sm.length[id] {
			t.Fatalf("P4: resize grew chunk %d: %d -> %d", id, sm.length[id], length)
		}
		sm.length[id] = length
	}
	if len(resumeGaps) == 0 {
		sm.s.addPending(0, total, 0)
	} else {
		// Bytes outside the gaps are already durable (resume model).
		for i := range sm.flushed {
			sm.flushed[i] = true
			sm.claimed[i] = true
		}
		for _, g := range resumeGaps {
			sm.s.addPending(g[0], g[1], 0)
			for i := g[0]; i < g[1]; i++ {
				sm.flushed[i] = false
				sm.claimed[i] = false
			}
		}
	}
	return sm
}

func (sm *sim) worker(id int) *simWorker {
	w, ok := sm.workers[id]
	if !ok {
		w = &simWorker{id: id}
		sm.workers[id] = w
	}
	return w
}

// apply executes one operation against the real scheduler, mirroring the
// production worker protocol.
func (sm *sim) apply(op simOp) {
	sm.steps++
	if op.kind == "demote" {
		// No sim worker is materialized for a demote op: production has no
		// phantom idle worker to rescue stranded work.
		victims := sm.s.demote(op.arg)
		for _, v := range victims {
			if vw, ok := sm.workers[v]; ok && vw.exited {
				sm.t.Fatalf("demote selected exited worker %d", v)
			}
		}
		sm.check()
		return
	}
	w := sm.worker(op.worker)
	switch op.kind {
	case "next":
		if w.exited || w.chunk != nil {
			return // protocol: only idle, live workers call next
		}
		c := sm.s.next(w.id)
		if c == nil {
			w.exited = true
			return
		}
		w.chunk = c
	case "claim":
		if w.chunk == nil {
			return
		}
		off, n, stop := sm.s.claim(w.chunk, max(op.arg, 1))
		for i := off; i < off+int64(n); i++ {
			if sm.claimed[i] {
				sm.t.Fatalf("P1: byte %d claimed twice", i)
			}
			sm.claimed[i] = true
		}
		w.unflush += int64(n)
		_ = stop
	case "flush":
		if w.chunk == nil || w.unflush == 0 {
			return
		}
		// Writes land in claim order within a chunk.
		c := w.chunk
		start := c.off + c.written.Load()
		for i := start; i < start+w.unflush; i++ {
			sm.flushed[i] = true
		}
		c.written.Add(w.unflush)
		w.unflush = 0
	case "complete":
		if w.chunk == nil {
			return
		}
		if _, _, todo := sm.s.cursor(w.chunk); todo || w.unflush != 0 {
			return // protocol: complete only drained, fully written chunks
		}
		sm.s.complete(w.chunk)
		w.chunk = nil
	case "newWorker":
		sm.worker(op.worker) // noncontiguous ids come from the generator
	}
	sm.check()
}

// check asserts P1 and P2 after every operation.
func (sm *sim) check() {
	sm.s.mu.Lock()
	type iv struct{ a, b int64 }
	var unclaimed []iv
	for _, c := range sm.s.pending {
		unclaimed = append(unclaimed, iv{c.off + c.done, c.end})
	}
	for _, c := range sm.s.active {
		unclaimed = append(unclaimed, iv{c.off + c.done, c.end})
	}
	sm.s.mu.Unlock()

	// P1: unclaimed intervals disjoint and exactly the un-claimed bytes.
	cover := make([]int, sm.total)
	for _, v := range unclaimed {
		for i := v.a; i < v.b; i++ {
			cover[i]++
		}
	}
	for i := int64(0); i < sm.total; i++ {
		switch {
		case cover[i] > 1:
			sm.t.Fatalf("P1: byte %d covered by %d intervals", i, cover[i])
		case cover[i] == 1 && sm.claimed[i]:
			sm.t.Fatalf("P1: claimed byte %d still offered as work", i)
		case cover[i] == 0 && !sm.claimed[i]:
			sm.t.Fatalf("P1: unclaimed byte %d lost from assignment space", i)
		}
	}

	// P2: durable bytes + snapshot intervals partition [0,total).
	snap := sm.s.snapshot()
	need := make([]bool, sm.total)
	for _, cs := range snap {
		for i := cs.Off + cs.Done; i < cs.End; i++ {
			need[i] = true
		}
		for i := cs.Off; i < cs.Off+cs.Done; i++ {
			if !sm.flushed[i] {
				sm.t.Fatalf("P2: snapshot marks byte %d done but it is not flushed", i)
			}
		}
	}
	for i := int64(0); i < sm.total; i++ {
		if !need[i] && !sm.flushed[i] {
			sm.t.Fatalf("P2: byte %d neither durable nor covered by the snapshot", i)
		}
	}
}

// drain finishes the run: every non-exited worker follows the production
// protocol to completion. Bounded (P5) and must strand nothing (P3).
func (sm *sim) drain() {
	const bound = 100000
	for range bound {
		progressed := false
		for _, w := range sm.workers {
			if w.exited {
				continue
			}
			progressed = true
			if w.chunk == nil {
				sm.apply(simOp{"next", w.id, 0})
				continue
			}
			sm.apply(simOp{"claim", w.id, 1 << 20})
			sm.apply(simOp{"flush", w.id, 0})
			sm.apply(simOp{"complete", w.id, 0})
		}
		if !progressed {
			break
		}
	}
	if !sm.s.idle() {
		sm.t.Fatalf("P3/P5: work stranded after drain (remaining=%d)", sm.s.remainingBytes())
	}
	for i := int64(0); i < sm.total; i++ {
		if !sm.flushed[i] {
			sm.t.Fatalf("byte %d never downloaded", i)
		}
	}
	// P4 closure: every granted chunk either completed or is gone.
	if len(sm.s.active) != 0 || len(sm.s.pending) != 0 {
		sm.t.Fatalf("chunks left behind after drain")
	}
}

// genTrace builds a deterministic op sequence for a seed. The starting
// worker id is randomized so traces cover pools whose surviving ids are all
// above the demote keep count (the state that distinguishes exact-excess
// victim selection from id-threshold selection).
func genTrace(rng *rand.Rand, parts int, ops int) []simOp {
	ids := []int{rng.IntN(8)}
	nextID := ids[0] + 1
	trace := make([]simOp, 0, ops)
	for range ops {
		id := ids[rng.IntN(len(ids))]
		switch rng.IntN(10) {
		case 0, 1, 2:
			trace = append(trace, simOp{"next", id, 0})
		case 3, 4:
			trace = append(trace, simOp{"claim", id, 1 + rng.IntN(700)})
		case 5:
			trace = append(trace, simOp{"flush", id, 0})
		case 6:
			trace = append(trace, simOp{"complete", id, 0})
		case 7:
			trace = append(trace, simOp{"demote", id, 1 + rng.IntN(parts)})
		case 8:
			if len(ids) < parts+3 {
				// Noncontiguous ids: real pools have gaps after exits.
				nextID += 1 + rng.IntN(3)
				ids = append(ids, nextID)
				trace = append(trace, simOp{"newWorker", nextID, 0})
			}
		case 9:
			trace = append(trace, simOp{"claim", id, 1 + rng.IntN(64)},
				simOp{"flush", id, 0})
		}
	}
	return trace
}

func runTrace(t simFailer, total, minSize int64, gaps [][2]int64, trace []simOp) {
	sm := newSim(t, total, minSize, gaps)
	for _, op := range trace {
		sm.apply(op)
	}
	sm.drain()
}

func TestSchedulerPropertySuite(t *testing.T) {
	t.Parallel()
	configs := []struct {
		name    string
		total   int64
		minSize int64
		parts   int
		gaps    [][2]int64
	}{
		{name: "fresh-p2", total: 2048, minSize: 1, parts: 2},
		{name: "fresh-p6", total: 4096, minSize: 8, parts: 6},
		{name: "fresh-p1", total: 1024, minSize: 1, parts: 1},
		{name: "resumed-gaps", total: 4096, minSize: 4, parts: 6,
			gaps: [][2]int64{{100, 700}, {1024, 1030}, {2048, 4000}}},
	}
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()
			for seed := range 100 {
				rng := rand.New(rand.NewPCG(uint64(seed), 42))
				trace := genTrace(rng, cfg.parts, 400)
				func() {
					defer func() {
						if t.Failed() {
							t.Logf("seed=%d minimized trace:\n%v",
								seed, minimizeTrace(cfg.total, cfg.minSize, cfg.gaps, trace))
						}
					}()
					runTrace(t, cfg.total, cfg.minSize, cfg.gaps, trace)
				}()
				if t.Failed() {
					return
				}
			}
		})
	}
}

// minimizeTrace greedily removes operations while the failure reproduces,
// yielding a small reproduction for the report.
func minimizeTrace(total, minSize int64, gaps [][2]int64, trace []simOp) []simOp {
	const probeBudget = 500
	probes := 0
	fails := func(ops []simOp) (failed bool) {
		probes++
		defer func() {
			if recover() != nil {
				failed = true
			}
		}()
		runTrace(&probeT{}, total, minSize, gaps, ops)
		return false
	}
	out := append([]simOp(nil), trace...)
	for i := 0; i < len(out) && probes < probeBudget; {
		candidate := append(append([]simOp(nil), out[:i]...), out[i+1:]...)
		if fails(candidate) {
			out = candidate
		} else {
			i++
		}
	}
	return out
}

// probeT panics on failure so minimization can detect reproduction without a
// *testing.T.
type probeT struct{}

func (p *probeT) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

// TestDemoteAllHighIDs is the explicit minimal edge case: every surviving
// worker id is >= keep. Exact-excess selection retires none of them when
// there is no excess; id-threshold selection would retire them all and
// strand the pending work.
func TestDemoteAllHighIDs(t *testing.T) {
	t.Parallel()
	// keep=2 with two high-id survivors: no excess, none may retire.
	runTrace(t, 2048, 1, nil, []simOp{
		{"next", 4, 0}, {"claim", 4, 300}, {"flush", 4, 0},
		{"next", 5, 0}, {"claim", 5, 300}, {"flush", 5, 0},
		{"demote", 0, 2},
	})
	// keep=1 with two high-id survivors: exactly ONE retires; id-threshold
	// selection would retire both and strand the remainder.
	runTrace(t, 2048, 1, nil, []simOp{
		{"next", 4, 0}, {"claim", 4, 300}, {"flush", 4, 0},
		{"next", 5, 0}, {"claim", 5, 300}, {"flush", 5, 0},
		{"demote", 0, 1},
	})
}
