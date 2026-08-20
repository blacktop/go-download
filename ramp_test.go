package download

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestRampSurvivesSingleSlotThrottle pins the 429 retry-budget contract: a
// server that admits only one in-flight range at a time (429 to everyone
// else) must serialize the download, not kill it. The throttled worker's
// waits cost no retry budget while its sibling is making progress, so even
// a tiny MaxRetries survives.
func TestRampSurvivesSingleSlotThrottle(t *testing.T) {
	t.Parallel()
	data := testData(128 << 10)
	var rejected atomic.Int32
	slot := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			writeBareRange(w, r, data, `"v1"`)
			return
		}
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			rejected.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Errorf("expected ranged request, got %q", r.Header.Get("Range"))
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		// Hold the slot well past MaxRetries seconds so a 429ing sibling
		// whose politeness waits were charged would exhaust its budget.
		body := data[start : end+1]
		for len(body) > 0 {
			n := min(4<<10, len(body))
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			body = body[n:]
			time.Sleep(120 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10, MaxRetries: 2,
		Timeout: 30 * time.Second})
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if rejected.Load() == 0 {
		t.Skip("second worker never collided with the slot; scenario not exercised")
	}
}

// TestResumeRampUsesRemainingWork pins the resume window sizing: a nearly
// complete multi-part resume must still admit a second worker (windows are
// sized from the remaining bytes, not the original file size).
func TestResumeRampUsesRemainingWork(t *testing.T) {
	t.Parallel()
	total := 1 << 20
	data := testData(total)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		5*time.Millisecond, 1, func(r *http.Request) bool {
			return r.Header.Get("Range") != "bytes=0-0"
		}))
	t.Cleanup(srv.Close)

	// Prefab a resume state: everything done except two 64 KiB chunks.
	dest := filepath.Join(t.TempDir(), "file.bin")
	part := dest + ".part"
	staged := make([]byte, total)
	copy(staged, data)
	chunks := []chunkState{
		{Off: 256 << 10, End: 320 << 10, Done: 0},
		{Off: 512 << 10, End: 576 << 10, Done: 0},
	}
	for _, c := range chunks {
		for i := c.Off; i < c.End; i++ {
			staged[i] = 0
		}
	}
	if err := os.WriteFile(part, staged, 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srv.URL + "/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	side := &stateFile{
		Version:  stateVersion,
		SourceID: sourceIdentity(u),
		Size:     int64(total),
		ETag:     `"v1"`,
		Chunks:   chunks,
	}
	if err := side.save(statePath(part)); err != nil {
		t.Fatal(err)
	}

	// MinPartSize large enough that a full-size-based window (the old bug)
	// could never admit a second worker within the 128 KiB remainder.
	d := newDL(t, &Options{Parts: 2, MinPartSize: 32 << 10})
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !res.Resumed {
		t.Fatal("prefab resume state was not used")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed bytes differ from source")
	}
	st.mu.Lock()
	maxConc := st.maxConc
	st.mu.Unlock()
	if maxConc < 2 {
		t.Errorf("near-complete resume served with max concurrency %d; "+
			"want the pending chunks fetched in parallel", maxConc)
	}
}
