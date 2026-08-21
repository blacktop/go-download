package download

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// decisionCapture collects the run's debug logs as JSONL and returns the
// parsed ramp decision records.
type decisionCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newDecisionCapture() (*slog.Logger, *decisionCapture) {
	c := &decisionCapture{}
	logger := slog.New(slog.NewJSONHandler(c,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, c
}

func (c *decisionCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
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

	logger, cap := newDecisionCapture()
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
		"settle_floor_ms", "ready_min_us",
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

func TestRampDecisionSchemaSeparatesJudgedAndCreated(t *testing.T) {
	t.Parallel()
	cap := &decisionCapture{}
	logger := slog.New(slog.NewJSONHandler(cap,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	rs := &rampState{enabled: true, log: logger, runID: 7}
	rs.emit(&rampDecision{
		seq: 1, action: "admit", reason: "clear-gain",
		judged:   rampBatch{prior: 1, admitted: 2},
		created:  rampBatch{prior: 2, admitted: 4},
		baseline: rampSample{bytes: 100, elapsed: time.Second},
		measured: rampSample{bytes: 230, elapsed: time.Second},
		q:        2.3, qValid: true,
	})

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
	}
	rs.note(1) // burn-in
	rs.note(2) // admit, then emit
	if got, want := strings.Join(events, ","), "spawn,emit"; got != want {
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
		log:     slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})),
		enabled: true,
	}

	second := make(chan struct{})
	go func() {
		rs.emitOrdered(&rampDecision{seq: 2})
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

	rs.emitOrdered(&rampDecision{seq: 1})
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
