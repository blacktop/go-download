package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRampDecisionLogHandlerFiltersOtherDebugRecords(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	logger := slog.New(rampDecisionLogHandler{Handler: slog.NewJSONHandler(&out,
		&slog.HandlerOptions{Level: slog.LevelDebug})})
	logger.DebugContext(context.Background(), "election", "path", "/private/secret")
	logger.DebugContext(context.Background(), "ramp decision", "schema", 2)

	got := out.String()
	if strings.Contains(got, "secret") || strings.Contains(got, "election") {
		t.Fatalf("evidence log contains unrelated debug record: %s", got)
	}
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, `"msg":"ramp decision"`) {
		t.Fatalf("evidence log = %q, want one ramp decision JSON record", got)
	}
}

// TestFanoutPreservesVerboseDiagnostics pins the combined-mode contract:
// enabling the JSON evidence sink must never swallow the human logger's
// diagnostics — verbose records still reach the human handler, and only
// ramp decisions reach the evidence file.
func TestFanoutPreservesVerboseDiagnostics(t *testing.T) {
	t.Parallel()
	var human, evidence bytes.Buffer
	fan := fanoutLogHandler{handlers: []slog.Handler{
		slog.NewTextHandler(&human, &slog.HandlerOptions{Level: slog.LevelDebug}),
		rampDecisionLogHandler{Handler: slog.NewJSONHandler(&evidence,
			&slog.HandlerOptions{Level: slog.LevelDebug})},
	}}
	logger := slog.New(fan)

	logger.DebugContext(context.Background(), "election", "status", 206)
	logger.DebugContext(context.Background(), "retrying chunk", "attempt", 1)
	logger.DebugContext(context.Background(), "ramp decision", "schema", 2)

	if h := human.String(); !strings.Contains(h, "election") ||
		!strings.Contains(h, "retrying chunk") || !strings.Contains(h, "ramp decision") {
		t.Fatalf("human diagnostics lost records: %q", h)
	}
	if e := evidence.String(); strings.Contains(e, "election") ||
		strings.Contains(e, "retrying") || strings.Count(e, "\n") != 1 {
		t.Fatalf("evidence sink = %q, want only the ramp decision", e)
	}
}

// TestFanoutRespectsPerHandlerLevels: without --verbose the human handler
// stays at info level and must not receive debug records even though the
// evidence sink runs at debug.
func TestFanoutRespectsPerHandlerLevels(t *testing.T) {
	t.Parallel()
	var human, evidence bytes.Buffer
	fan := fanoutLogHandler{handlers: []slog.Handler{
		slog.NewTextHandler(&human, &slog.HandlerOptions{Level: slog.LevelInfo}),
		rampDecisionLogHandler{Handler: slog.NewJSONHandler(&evidence,
			&slog.HandlerOptions{Level: slog.LevelDebug})},
	}}
	logger := slog.New(fan)

	logger.DebugContext(context.Background(), "ramp decision", "schema", 2)
	logger.InfoContext(context.Background(), "downloaded")

	if h := human.String(); strings.Contains(h, "ramp decision") || !strings.Contains(h, "downloaded") {
		t.Fatalf("human handler level not respected: %q", h)
	}
	if e := evidence.String(); !strings.Contains(e, "ramp decision") {
		t.Fatalf("evidence sink missed the debug ramp record: %q", e)
	}
}
