package download

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadMeasurementDebugRecord(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	server := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := newDL(t, &Options{
		Parts: 2, MinParts: 2, MinPartSize: 4 << 10, Logger: logger,
	})
	res, err := d.Get(t.Context(), server.URL+"/file.bin", filepath.Join(t.TempDir(), "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(data)) {
		t.Fatalf("result size = %d, want %d", res.Size, len(data))
	}

	output := logs.String()
	for _, field := range []string{
		`msg="download measurement"`,
		"host=127.0.0.1",
		"parts=2",
		"min_parts=2",
		"min_part_size=4096",
		"protocol=HTTP/1.1",
		"retries=0",
		"http_429s=0",
		"connected_addresses=",
		"placement=false",
		"resumed=false",
		"size=65536",
		"integrity=passed",
		"success=true",
	} {
		if !strings.Contains(output, field) {
			t.Errorf("debug log missing %q:\n%s", field, output)
		}
	}
}

func TestMeasurementDisabledBelowDebug(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if measurement := newRunMeasurement(t.Context(), logger); measurement != nil {
		t.Fatal("non-debug logger allocated run measurement")
	}
}
