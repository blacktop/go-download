package download

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- test fixture for published MD5 integrity
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// startStopReporter records Start/Done pairing per instance.
type startStopReporter struct {
	NopReporter
	mu     sync.Mutex
	starts []string
	dones  int
}

func (r *startStopReporter) Start(info Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, info.Name)
}

func (r *startStopReporter) Done(error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dones++
}

func TestDoPerRequestReporterAndChecksums(t *testing.T) {
	t.Parallel()
	dataA := testData(64 << 10)
	dataB := append(testData(32<<10), 0x42)
	sumA := sha256.Sum256(dataA)
	sumB := sha256.Sum256(dataB)
	md5A := md5.Sum(dataA) // #nosec G401 -- test fixture for published MD5 integrity
	md5B := md5.Sum(dataB) // #nosec G401 -- test fixture for published MD5 integrity
	var stA, stB stats
	srvA := httptest.NewServer(rangeHandler(dataA, `"a"`, &stA))
	t.Cleanup(srvA.Close)
	srvB := httptest.NewServer(rangeHandler(dataB, `"b"`, &stB))
	t.Cleanup(srvB.Close)

	// On one long-lived engine, A inherits the option-level MD5 while B
	// replaces it per request. The concurrent runs prove reporters and resolved
	// checksums cannot leak across downloads.
	d := newDL(t, &Options{
		MinPartSize: 4 << 10,
		ExpectedMD5: strings.ToUpper(hex.EncodeToString(md5A[:])),
	})
	dir := t.TempDir()
	repA, repB := &startStopReporter{}, &startStopReporter{}

	var wg sync.WaitGroup
	var resA, resB *Result
	var errA, errB error
	wg.Go(func() {
		resA, errA = d.Do(t.Context(), &Request{
			URL: srvA.URL + "/a.bin", Dest: filepath.Join(dir, "a.bin"),
			Reporter: repA, ExpectedSHA256: hex.EncodeToString(sumA[:]),
		})
	})
	wg.Go(func() {
		resB, errB = d.Do(t.Context(), &Request{
			URL: srvB.URL + "/b.bin", Dest: filepath.Join(dir, "b.bin"),
			Reporter: repB, ExpectedSHA256: hex.EncodeToString(sumB[:]),
			ExpectedMD5: hex.EncodeToString(md5B[:]),
		})
	})
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("Do errors: %v / %v", errA, errB)
	}
	if resA.SHA256 != hex.EncodeToString(sumA[:]) || resB.SHA256 != hex.EncodeToString(sumB[:]) {
		t.Error("per-request checksums not verified")
	}
	if resA.MD5 != hex.EncodeToString(md5A[:]) || resB.MD5 != hex.EncodeToString(md5B[:]) {
		t.Errorf("resolved MD5 checksums crossed runs: %q / %q", resA.MD5, resB.MD5)
	}
	gotA, _ := os.ReadFile(resA.Path)
	gotB, _ := os.ReadFile(resB.Path)
	if !bytes.Equal(gotA, dataA) || !bytes.Equal(gotB, dataB) {
		t.Fatal("downloaded bytes differ from sources")
	}
	for name, rep := range map[string]*startStopReporter{"a": repA, "b": repB} {
		if len(rep.starts) != 1 || rep.dones != 1 {
			t.Errorf("reporter %s saw %d starts / %d dones, want 1/1", name, len(rep.starts), rep.dones)
		}
	}
	if repA.starts[0] != "a.bin" || repB.starts[0] != "b.bin" {
		t.Errorf("reporters crossed streams: %v / %v", repA.starts, repB.starts)
	}

	d.CloseIdleConnections() // smoke: must be safe on a live engine
}

func TestDoSnapshotsAndMergesRequestHeaders(t *testing.T) {
	t.Parallel()
	data := testData(128 << 10)
	started := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	var mu sync.Mutex
	var received []http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Do(func() {
			close(started)
			<-release
		})
		mu.Lock()
		received = append(received, r.Header.Clone())
		mu.Unlock()
		if r.Header.Get("Range") == "bytes=0-" {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", 4095, len(data)))
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[:4096])
			return
		}
		writeBareRange(w, r, data, `"v1"`)
	}))
	t.Cleanup(srv.Close)

	optionHeaders := http.Header{
		"X-Layer":       {"option-one", "option-two"},
		"X-Option-Only": {"option-original"},
	}
	d := newDL(t, &Options{
		Parts: 2, MinParts: 2, MinPartSize: 4 << 10, Headers: optionHeaders,
	})
	// New owns the option header map and its value slices.
	optionHeaders["X-Layer"][0] = "option-mutated"
	optionHeaders.Set("X-Option-Only", "option-mutated")

	requestHeaders := http.Header{
		"x-layer":        {"request-one", "request-two"},
		"X-Request-Only": {"request-original"},
	}
	req := &Request{
		URL: srv.URL + "/file.bin", Dest: filepath.Join(t.TempDir(), "file.bin"),
		Headers: requestHeaders,
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.Do(t.Context(), req)
		done <- err
	}()

	<-started
	// Do has crossed its ownership boundary. Later changes to either the map or
	// an existing value slice must not change worker requests.
	requestHeaders["x-layer"][0] = "request-mutated"
	requestHeaders.Set("X-Request-Only", "request-mutated")
	requestHeaders.Set("X-Late", "late")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 2 {
		t.Fatalf("server saw %d requests, want election plus worker requests", len(received))
	}
	for i, header := range received {
		if got := header.Values("X-Layer"); !slices.Equal(got, []string{"request-one", "request-two"}) {
			t.Errorf("request %d merged X-Layer = %q", i, got)
		}
		if got := header.Get("X-Option-Only"); got != "option-original" {
			t.Errorf("request %d option snapshot = %q", i, got)
		}
		if got := header.Get("X-Request-Only"); got != "request-original" {
			t.Errorf("request %d request snapshot = %q", i, got)
		}
		if got := header.Get("X-Late"); got != "" {
			t.Errorf("request %d observed late header %q", i, got)
		}
	}
}

func TestDoInvalidPerRequestChecksum(t *testing.T) {
	t.Parallel()
	d := newDL(t, nil)
	if _, err := d.Do(t.Context(), &Request{URL: "http://x/", ExpectedSHA256: "zz"}); err == nil {
		t.Error("bad per-request sha256 must error")
	}
	if _, err := d.Do(t.Context(), &Request{URL: "http://x/", ExpectedMD5: "zz"}); err == nil {
		t.Error("bad per-request md5 must error")
	}
	if _, err := d.Do(t.Context(), nil); err == nil {
		t.Error("nil request must error")
	}
}

func TestRejectContentTypes(t *testing.T) {
	t.Parallel()
	html := []byte("<html>dead link</html>")
	var st stats
	srv := httptest.NewServer(withContentType("text/html; charset=UTF-8", rangeHandler(html, `"v1"`, &st)))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "file.ipsw")
	d := newDL(t, &Options{RejectContentTypes: []string{"text/html"}})
	_, err := d.Get(t.Context(), srv.URL+"/file.ipsw", dest)
	var cte *ContentTypeError
	if !errors.As(err, &cte) {
		t.Fatalf("expected ContentTypeError, got %v", err)
	}
	if cte.ContentType != "text/html; charset=UTF-8" {
		t.Errorf("ContentTypeError.ContentType = %q", cte.ContentType)
	}
	// Nothing may exist on disk, and the server saw only the initial request.
	for _, p := range []string{dest, dest + ".part", dest + ".part.json"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after rejected content type", p)
		}
	}
	if n := len(st.rangeHeaders()); n != 1 {
		t.Errorf("server saw %d requests, want only the initial request", n)
	}

	// Non-matching type proceeds.
	d2 := newDL(t, &Options{RejectContentTypes: []string{"application/x-msdownload"}})
	if _, err := d2.Get(t.Context(), srv.URL+"/file.ipsw", dest); err != nil {
		t.Fatalf("non-matching reject list must download: %v", err)
	}
}

func TestRejectedMediaTypeMatching(t *testing.T) {
	t.Parallel()
	reject := []string{"text/html", "application/json"}
	tests := []struct {
		ct   string
		want bool
	}{
		{ct: "text/html", want: true},
		{ct: "TEXT/HTML", want: true},
		{ct: "text/html; charset=UTF-8", want: true},
		// Malformed parameters must not smuggle the media type past the check.
		{ct: "text/html; charset=\x7fbroken", want: true},
		{ct: "text/html;;;", want: true},
		{ct: " text/html ; charset", want: true},
		{ct: "application/json;charset", want: true},
		{ct: "text/plain", want: false},
		{ct: "application/octet-stream; name=a.html", want: false},
		{ct: "", want: false},
	}
	for _, tc := range tests {
		if got := rejected(tc.ct, reject); got != tc.want {
			t.Errorf("rejected(%q) = %t, want %t", tc.ct, got, tc.want)
		}
	}
	if rejected("text/html", nil) {
		t.Error("empty reject list must never match")
	}
}

func TestDiscard(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest+".part", make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".part.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Discard(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dest + ".part", dest + ".part.json"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived Discard", p)
		}
	}
	// Discarding nothing is not an error.
	if err := Discard(t.Context(), dest); err != nil {
		t.Errorf("Discard of missing staging files: %v", err)
	}
}

func TestChunkEventContract(t *testing.T) {
	t.Parallel()
	data := testData(256 << 10)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		time.Millisecond, 1, func(r *http.Request) bool {
			start, ok := parseRangeStart(r.Header.Get("Range"))
			return ok && start == 0
		}))
	t.Cleanup(srv.Close)

	rep := &eventReporter{}
	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10})
	if _, err := d.Do(t.Context(), &Request{
		URL: srv.URL + "/file.bin", Dest: dest, Reporter: rep,
	}); err != nil {
		t.Fatal(err)
	}

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.started) < 2 {
		t.Fatalf("expected several chunks, got %d", len(rep.started))
	}
	for id, n := range rep.started {
		if n != 1 {
			t.Errorf("ChunkStart fired %d times for id %d, want exactly once", n, id)
		}
	}
	for id := range rep.resized {
		if rep.started[id] == 0 {
			t.Errorf("ChunkResize for id %d before its ChunkStart", id)
		}
	}
	if len(rep.resized) == 0 {
		t.Error("dynamic splitting produced no ChunkResize events")
	}
}

// legacyReporter implements only the original seven Reporter methods —
// no NopReporter embed, no ChunkResizer/ChunkRestarter — proving the
// interface stayed source-compatible for existing implementations.
type legacyReporter struct{}

func (legacyReporter) Start(Info)                            {}
func (legacyReporter) ChunkStart(int, int64, int64, int64)   {}
func (legacyReporter) Connected(int, string)                 {}
func (legacyReporter) ChunkProgress(int, int, time.Duration) {}
func (legacyReporter) ChunkRetry(int, int, error)            {}
func (legacyReporter) ChunkDone(int)                         {}
func (legacyReporter) Done(error)                            {}

var _ Reporter = legacyReporter{}

func TestLegacyReporterCompatible(t *testing.T) {
	t.Parallel()
	data := testData(128 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)

	// A multipart download with splits must not panic or misbehave when
	// the reporter lacks the optional extensions.
	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10})
	if _, err := d.Do(t.Context(), &Request{
		URL: srv.URL + "/file.bin", Dest: dest, Reporter: legacyReporter{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
}

// eventReporter counts ChunkStart/ChunkResize per id.
type eventReporter struct {
	NopReporter
	mu      sync.Mutex
	started map[int]int
	resized map[int]int
}

func (r *eventReporter) ChunkStart(id int, _, _, _ int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started == nil {
		r.started = make(map[int]int)
	}
	r.started[id]++
}

func (r *eventReporter) ChunkResize(id int, _ int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resized == nil {
		r.resized = make(map[int]int)
	}
	r.resized[id]++
}

func (r *eventReporter) ChunkProgress(int, int, time.Duration) {}
