package download

import (
	"os"
	"path/filepath"
	"testing"
)

func validSidecar() *stateFile {
	return &stateFile{
		Version: stateVersion,
		URL:     "http://example.org/f",
		Size:    1000,
		ETag:    `"v1"`,
		Chunks: []chunkState{
			{Off: 0, End: 500, Done: 200},
			{Off: 500, End: 1000, Done: 0},
		},
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f.part.json")
	st := validSidecar()
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	got := loadState(path)
	if got == nil {
		t.Fatal("loadState returned nil for a valid sidecar")
	}
	if got.Size != 1000 || got.ETag != `"v1"` || len(got.Chunks) != 2 {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.remaining() != 800 {
		t.Errorf("remaining = %d, want 800", got.remaining())
	}
}

func TestLoadStateRejectsInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, mutate func(*stateFile)) string {
		st := validSidecar()
		mutate(st)
		path := filepath.Join(dir, name)
		if err := st.save(path); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name   string
		mutate func(*stateFile)
	}{
		{"wrong version", func(st *stateFile) { st.Version = 99 }},
		{"zero size", func(st *stateFile) { st.Size = 0 }},
		{"no validator", func(st *stateFile) { st.ETag, st.LastModified = "", "" }},
		{"overlapping chunks", func(st *stateFile) { st.Chunks[1].Off = 400 }},
		{"chunk past size", func(st *stateFile) { st.Chunks[1].End = 2000 }},
		{"done past length", func(st *stateFile) { st.Chunks[0].Done = 900 }},
		{"negative off", func(st *stateFile) { st.Chunks[0].Off = -1; st.Chunks[0].End = 10 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := write(tc.name+".json", tc.mutate)
			if loadState(path) != nil {
				t.Error("invalid sidecar was accepted")
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		if loadState(filepath.Join(dir, "nope.json")) != nil {
			t.Error("missing sidecar was accepted")
		}
	})
	t.Run("garbage json", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(dir, "garbage.json")
		os.WriteFile(path, []byte("{"), 0o644)
		if loadState(path) != nil {
			t.Error("garbage sidecar was accepted")
		}
	})
}

func TestStateUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	if err := os.WriteFile(part, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}

	st := validSidecar()
	if !st.usable(part, 1000, `"v1"`, "") {
		t.Error("valid state rejected")
	}
	if st.usable(part, 999, `"v1"`, "") {
		t.Error("size mismatch accepted")
	}
	if st.usable(part, 1000, `"v2"`, "") {
		t.Error("etag mismatch accepted")
	}
	if st.usable(filepath.Join(dir, "missing.part"), 1000, `"v1"`, "") {
		t.Error("missing part file accepted")
	}

	weak := validSidecar()
	weak.ETag = `W/"v1"`
	if weak.usable(part, 1000, `W/"v1"`, "") {
		t.Error("weak etag accepted as validator")
	}

	lm := validSidecar()
	lm.ETag = ""
	lm.LastModified = "Mon, 02 Jan 2026 03:04:05 GMT"
	if !lm.usable(part, 1000, "", "Mon, 02 Jan 2026 03:04:05 GMT") {
		t.Error("matching Last-Modified rejected")
	}
	if lm.usable(part, 1000, "", "Tue, 03 Jan 2026 03:04:05 GMT") {
		t.Error("changed Last-Modified accepted")
	}

	// Truncated .part file invalidates resume.
	if err := os.Truncate(part, 500); err != nil {
		t.Fatal(err)
	}
	if st.usable(part, 1000, `"v1"`, "") {
		t.Error("truncated part file accepted")
	}
}
