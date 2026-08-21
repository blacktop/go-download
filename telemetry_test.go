package download

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// decisionCapture collects the run's debug logs as JSONL and returns the
// parsed ramp decision records. When DL_DECISION_EVIDENCE names a
// directory, the raw JSONL is also retained there for the offline evidence
// campaign; the JSON handler — not the human CLI rendering — is the parser
// contract.
type decisionCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newDecisionCapture(t *testing.T, name string) (*slog.Logger, *decisionCapture) {
	t.Helper()
	c := &decisionCapture{}
	logger := slog.New(slog.NewJSONHandler(&syncWriter{c: c},
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		dir := os.Getenv("DL_DECISION_EVIDENCE")
		if dir == "" || t.Failed() {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		var evidence bytes.Buffer
		for line := range bytes.Lines(c.buf.Bytes()) {
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Errorf("evidence log line is not JSON: %v", err)
				return
			}
			if rec["msg"] == "ramp decision" {
				evidence.Write(line)
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Errorf("evidence dir: %v", err)
			return
		}
		// Append so repeated runs (-count=N) accumulate samples; a run
		// boundary is visible as a seq reset within the file.
		path := filepath.Join(dir, name+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Errorf("evidence write: %v", err)
			return
		}
		defer f.Close()
		if _, err := f.Write(evidence.Bytes()); err != nil {
			t.Errorf("evidence write: %v", err)
		}
	})
	return logger, c
}

type syncWriter struct{ c *decisionCapture }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}

type eventWriter struct{ events *[]string }

func (w eventWriter) Write(p []byte) (int, error) {
	*w.events = append(*w.events, "emit")
	return len(p), nil
}

// decisions parses the captured JSONL and returns the ramp decision records
// (raw line + decoded fields), in emission order.
func (c *decisionCapture) decisions(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var recs []map[string]any
	for line := range bytes.Lines(c.buf.Bytes()) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %q", err, line)
		}
		if m["msg"] == "ramp decision" {
			recs = append(recs, m)
		}
	}
	return recs
}

// TestRampDecisionRecordsParseableSharedCap runs the shared-cap fixture and
// proves the decision records are machine-parseable with the stable schema,
// carry no sensitive values, and end in the regime's expected demotion.
func TestRampDecisionRecordsParseableSharedCap(t *testing.T) {
	t.Parallel()
	data := testData(4 << 20)
	var st stats
	srv := httptest.NewServer(sharedCapHandler(data, `"v1"`, &st,
		newSharedLimiter(4<<20), nil))
	t.Cleanup(srv.Close)

	logger, cap := newDecisionCapture(t, "sharedcap-parts2")
	d := newDL(t, &Options{Parts: 2, MinPartSize: 64 << 10, Logger: logger})
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); err != nil {
		t.Fatal(err)
	}

	recs := cap.decisions(t)
	if len(recs) < 2 {
		t.Fatalf("decision records = %d, want at least admit + verdict", len(recs))
	}
	required := []string{"schema", "run", "seq", "action", "reason",
		"judged_transition", "judged_prior", "judged_admitted", "judged_batch_size",
		"judged_clamped", "judged_final", "created_transition", "created_prior",
		"created_admitted", "created_batch_size", "created_clamped", "created_final",
		"ceiling", "baseline_bytes", "baseline_us", "baseline_bps",
		"measured_bytes", "measured_us", "rate_bps", "q", "q_valid",
		"window_bytes", "measure_windows",
		"settle_floor_ms", "unclaimed_bytes", "h_est_s", "ready_min_us",
		"ready_med_us", "ready_max_us", "ready_workers", "batch_workers"}
	actions := map[string]bool{"admit": true, "freeze": true, "demote": true, "keep-final": true}
	var lastSeq float64
	for i, rec := range recs {
		for _, k := range required {
			if _, ok := rec[k]; !ok {
				t.Fatalf("record %d missing %q: %v", i, k, rec)
			}
		}
		if rec["schema"].(float64) != rampDecisionSchema {
			t.Fatalf("schema = %v", rec["schema"])
		}
		if !actions[rec["action"].(string)] {
			t.Fatalf("unknown action %q", rec["action"])
		}
		if seq := rec["seq"].(float64); seq <= lastSeq {
			t.Fatalf("seq not monotonic: %v after %v", seq, lastSeq)
		} else {
			lastSeq = seq
		}
		// No sensitive values: records must not carry the URL, host, or
		// destination path.
		raw, _ := json.Marshal(rec)
		if bytes.Contains(raw, []byte(srv.URL)) || bytes.Contains(raw, []byte(dest)) {
			t.Fatalf("record leaks endpoint or path: %s", raw)
		}
	}
	if first := recs[0]; first["action"] != "admit" || first["reason"] != "first-batch-probe" ||
		first["judged_transition"] != "" || first["created_transition"] != "1->2" ||
		first["q_valid"] != false {
		t.Fatalf("first record = %v", first)
	} else if first["ready_min_us"].(float64) != -1 || first["ready_workers"].(float64) != 0 {
		// The initial worker is not an admitted batch; nothing may be
		// measured against a zero admission time.
		t.Fatalf("first admission carries readiness data: %v", first)
	}
	last := recs[len(recs)-1]
	if last["action"] == "admit" {
		t.Fatalf("shared-cap run ended on an admit record: %v", last)
	}
}

// TestResumedRunwayDecisionRecords interrupts a paced download, resumes it,
// and proves the resumed run emits its own correlated decision records with
// the smaller remaining-work snapshot — the resumed-runway evidence vehicle.
func TestResumedRunwayDecisionRecords(t *testing.T) {
	t.Parallel()
	data := testData(2 << 20)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		8*time.Millisecond, 8, func(*http.Request) bool { return true }))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "file.bin")
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	d := newDL(t, &Options{Parts: 4, MinPartSize: 16 << 10})
	if _, err := d.Get(ctx, srv.URL+"/file.bin", dest); err == nil {
		t.Fatal("expected cancellation to interrupt the first pass")
	}

	logger, cap := newDecisionCapture(t, "resumed-runway-parts4")
	d2 := newDL(t, &Options{Parts: 4, MinPartSize: 16 << 10, Logger: logger})
	if _, err := d2.Get(t.Context(), srv.URL+"/file.bin", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed bytes differ from source")
	}

	recs := cap.decisions(t)
	if len(recs) == 0 {
		t.Fatal("resumed run emitted no decision records")
	}
	for i, rec := range recs {
		if u := rec["unclaimed_bytes"].(float64); u < 0 || u >= float64(len(data)) {
			t.Fatalf("record %d unclaimed_bytes = %v, want a partial-runway snapshot", i, u)
		}
	}
}

func TestRampDecisionSchemaSeparatesJudgedAndCreated(t *testing.T) {
	t.Parallel()
	cap := &decisionCapture{}
	logger := slog.New(slog.NewJSONHandler(&syncWriter{c: cap},
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	rs := &rampState{enabled: true, log: logger, runID: 7}
	rs.emit(&rampDecision{
		seq: 1, action: "admit", reason: "clear-gain",
		judged:   rampBatch{prior: 1, admitted: 2},
		created:  rampBatch{prior: 2, admitted: 4},
		baseline: rampSample{bytes: 100, elapsed: time.Second},
		measured: rampSample{bytes: 230, elapsed: time.Second},
		q:        2.3, qValid: true,
	}, 2300)

	recs := cap.decisions(t)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec["judged_transition"] != "1->2" || rec["created_transition"] != "2->4" {
		t.Fatalf("transition identities = judged %v created %v",
			rec["judged_transition"], rec["created_transition"])
	}
	if rec["baseline_bytes"].(float64) != 100 || rec["baseline_us"].(float64) != 1_000_000 ||
		rec["measured_bytes"].(float64) != 230 || rec["measured_us"].(float64) != 1_000_000 {
		t.Fatalf("raw samples = baseline %v/%vus measured %v/%vus",
			rec["baseline_bytes"], rec["baseline_us"], rec["measured_bytes"], rec["measured_us"])
	}
	if got := rec["h_est_s"].(float64); got != 10 {
		t.Fatalf("h_est_s = %v, want 10 using current 230 B/s rate", got)
	}
}

func TestRampDecisionEmissionFollowsSideEffects(t *testing.T) {
	t.Parallel()
	var events []string
	start := time.Unix(1000, 0)
	now := start
	rs := &rampState{
		enabled: true,
		log: slog.New(slog.NewJSONHandler(eventWriter{events: &events},
			&slog.HandlerOptions{Level: slog.LevelDebug})),
		now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
		parts: 2, window: 1, admitted: 1, markTime: start,
		spawn: func(int) { events = append(events, "spawn") },
		unclaimed: func() int64 {
			events = append(events, "snapshot")
			return 10
		},
	}
	rs.note(1) // burn-in
	rs.note(2) // snapshot, admit, then emit
	if got, want := strings.Join(events, ","), "snapshot,spawn,emit"; got != want {
		t.Fatalf("decision event order = %q, want %q", got, want)
	}
}

// TestEmitOrderedSerializesOutOfTurnRecords pins the seq-ordered emission
// contract: a later-sequenced record arriving at emission first must wait
// for its predecessor, so the JSONL stream can never interleave decisions
// out of order even when a fast transfer crosses the next decision window
// during the previous decision's side effects.
func TestEmitOrderedSerializesOutOfTurnRecords(t *testing.T) {
	t.Parallel()
	c := &decisionCapture{}
	rs := &rampState{
		log:     slog.New(slog.NewJSONHandler(&syncWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		enabled: true,
	}

	second := make(chan struct{})
	go func() {
		rs.emitOrdered(&rampDecision{seq: 2}, -1)
		close(second)
	}()

	// The out-of-turn record must not emit while its predecessor is missing.
	select {
	case <-second:
		t.Fatal("seq 2 emitted before seq 1")
	case <-time.After(50 * time.Millisecond):
	}
	if len(c.decisions(t)) != 0 {
		t.Fatal("records written while waiting for the predecessor")
	}

	rs.emitOrdered(&rampDecision{seq: 1}, -1)
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("seq 2 never emitted after its predecessor")
	}
	recs := c.decisions(t)
	if len(recs) != 2 || recs[0]["seq"].(float64) != 1 || recs[1]["seq"].(float64) != 2 {
		t.Fatalf("emission order = %v", recs)
	}
}

// TestUnclaimedNetworkFloorExcludesBufferedBytes pins the runway snapshot:
// bytes already received this run (progress) never count as remaining
// network work even while the scheduler cursor still shows them unclaimed,
// and the floor never goes negative or resurrects near completion.
func TestUnclaimedNetworkFloorExcludesBufferedBytes(t *testing.T) {
	t.Parallel()
	sched := newScheduler(1)
	sched.addPending(0, 1000, 0)
	var progress atomic.Int64
	floor := unclaimedNetworkFloor(sched, &progress)

	if got := floor(); got != 1000 {
		t.Fatalf("initial floor = %d, want 1000", got)
	}
	// A 300-byte buffer has been received but the sink has not advanced the
	// claim cursor yet: the scheduler still reports 1000 unclaimed.
	progress.Store(300)
	if sched.remainingBytes() != 1000 {
		t.Fatalf("fixture assumption broken: remainingBytes = %d", sched.remainingBytes())
	}
	if got := floor(); got != 700 {
		t.Fatalf("floor with buffered bytes = %d, want 700", got)
	}
	// Near completion the floor must reach zero, not report phantom runway.
	progress.Store(1000)
	if got := floor(); got != 0 {
		t.Fatalf("floor at completion = %d, want 0", got)
	}
	progress.Store(1300) // discard-overcounted progress stays clamped
	if got := floor(); got != 0 {
		t.Fatalf("floor past completion = %d, want 0", got)
	}
}

// TestUnclaimedNetworkFloorOnResume pins the resumed-run semantics: the
// floor is anchored to the remaining assignment at run start, not the full
// object size.
func TestUnclaimedNetworkFloorOnResume(t *testing.T) {
	t.Parallel()
	sched := newScheduler(1)
	sched.addPending(600, 1000, 0) // 400 bytes remain of a 1000-byte object
	var progress atomic.Int64
	floor := unclaimedNetworkFloor(sched, &progress)
	if got := floor(); got != 400 {
		t.Fatalf("resume floor = %d, want 400", got)
	}
	progress.Store(150)
	if got := floor(); got != 250 {
		t.Fatalf("resume floor after 150 received = %d, want 250", got)
	}
}
