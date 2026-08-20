package download

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSidecar() *stateFile {
	return &stateFile{
		Version:  stateVersion,
		SourceID: strings.Repeat("a", 64),
		Size:     1000,
		ETag:     `"v1"`,
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
	if got.Size != 1000 || got.SourceID != st.SourceID || got.ETag != `"v1"` || len(got.Chunks) != 2 {
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
		{"missing source identity", func(st *stateFile) { st.SourceID = "" }},
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
	file, err := os.Open(part)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	st := validSidecar()
	if !st.usable(file, st.SourceID, 1000, `"v1"`, "") {
		t.Error("valid state rejected")
	}
	if st.usable(file, strings.Repeat("b", 64), 1000, `"v1"`, "") {
		t.Error("source identity mismatch accepted")
	}
	if st.usable(file, st.SourceID, 999, `"v1"`, "") {
		t.Error("size mismatch accepted")
	}
	if st.usable(file, st.SourceID, 1000, `"v2"`, "") {
		t.Error("etag mismatch accepted")
	}

	weak := validSidecar()
	weak.ETag = `W/"v1"`
	if weak.usable(file, weak.SourceID, 1000, `W/"v1"`, "") {
		t.Error("weak etag accepted as validator")
	}
	// A weak ETag falls back to Last-Modified, mirroring run.validator.
	weak.LastModified = "Mon, 02 Jan 2026 03:04:05 GMT"
	if !weak.usable(file, weak.SourceID, 1000, `W/"v1"`, "Mon, 02 Jan 2026 03:04:05 GMT") {
		t.Error("weak etag with matching Last-Modified rejected")
	}
	if weak.usable(file, weak.SourceID, 1000, `W/"v1"`, "Tue, 03 Jan 2026 03:04:05 GMT") {
		t.Error("weak etag with changed Last-Modified accepted")
	}

	lm := validSidecar()
	lm.ETag = ""
	lm.LastModified = "Mon, 02 Jan 2026 03:04:05 GMT"
	if !lm.usable(file, lm.SourceID, 1000, "", "Mon, 02 Jan 2026 03:04:05 GMT") {
		t.Error("matching Last-Modified rejected")
	}
	if lm.usable(file, lm.SourceID, 1000, "", "Tue, 03 Jan 2026 03:04:05 GMT") {
		t.Error("changed Last-Modified accepted")
	}

	// Truncated .part file invalidates resume.
	if err := os.Truncate(part, 500); err != nil {
		t.Fatal(err)
	}
	if st.usable(file, st.SourceID, 1000, `"v1"`, "") {
		t.Error("truncated part file accepted")
	}
}

func TestStateUsableBindsLockedFile(t *testing.T) {
	t.Parallel()
	// usable validates size against the descriptor it is handed, not the
	// pathname: a descriptor for the original full-size staging inode
	// passes while a descriptor for a replacement inode fails, regardless
	// of what any path points at. (Two separate files stand in for the
	// original and replacement inodes; renaming an open file to arrange
	// them at one path is not possible on Windows.)
	dir := t.TempDir()
	originalPath := filepath.Join(dir, "f.part")
	if err := os.WriteFile(originalPath, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(dir, "replacement.part")
	if err := os.WriteFile(replacementPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := os.Open(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	replacement, err := os.Open(replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()

	st := validSidecar()
	if !st.usable(original, st.SourceID, 1000, `"v1"`, "") {
		t.Fatal("test setup: sidecar must describe the original descriptor")
	}
	if st.usable(replacement, st.SourceID, 1000, `"v1"`, "") {
		t.Error("sidecar for replaced staging file accepted against current descriptor")
	}
}

func TestSourceIdentity(t *testing.T) {
	t.Parallel()
	a, err := url.Parse("https://example.org/file?token=secret#one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := url.Parse("https://example.org/file?token=secret#two")
	if err != nil {
		t.Fatal(err)
	}
	c, err := url.Parse("https://example.org/file?token=other")
	if err != nil {
		t.Fatal(err)
	}
	if sourceIdentity(a) != sourceIdentity(b) {
		t.Error("fragments, which are not sent to the server, must not change source identity")
	}
	if sourceIdentity(a) == sourceIdentity(c) {
		t.Error("different query resources received the same source identity")
	}
	if strings.Contains(sourceIdentity(a), "secret") {
		t.Error("source identity exposed URL credentials")
	}
}
