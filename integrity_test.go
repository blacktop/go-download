package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeBareRange(w http.ResponseWriter, r *http.Request, data []byte, etag string) {
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
	if !ok {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func TestRedirectSensitiveHeadersNotReapplied(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var mu sync.Mutex
	var originHeader http.Header
	var received []http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Clone())
		mu.Unlock()
		writeBareRange(w, r, data, `"v1"`)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		originHeader = r.Header.Clone()
		mu.Unlock()
		http.Redirect(w, r, target.URL+"/file.bin", http.StatusFound)
	}))
	defer source.Close()
	sourceURL := strings.Replace(source.URL, "127.0.0.1", "localhost", 1) + "/start"

	d := newDL(t, &Options{
		Parts:       2,
		MinPartSize: 4 << 10,
		Headers: http.Header{
			"Authorization":       {"Bearer origin-secret"},
			"Cookie":              {"session=origin-secret"},
			"Proxy-Authorization": {"Basic origin-secret"},
			"X-Trace":             {"safe-trace"},
		},
	})
	_, got := mustGet(t, d, sourceURL, filepath.Join(t.TempDir(), "file.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("redirected download differs from source")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
		if value := originHeader.Get(name); value == "" {
			t.Errorf("origin request did not receive configured %s", name)
		}
	}
	if len(received) < 2 {
		t.Fatalf("target saw %d requests, want probe plus worker", len(received))
	}
	for i, h := range received {
		for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if value := h.Get(name); value != "" {
				t.Errorf("target request %d leaked %s: %q", i, name, value)
			}
		}
		if got := h.Get("X-Trace"); got != "safe-trace" {
			t.Errorf("target request %d lost non-sensitive header: %q", i, got)
		}
	}
}

func TestMultipartRequiresValidatorOrChecksum(t *testing.T) {
	t.Parallel()
	t.Run("falls back to one stream", func(t *testing.T) {
		t.Parallel()
		v1 := bytes.Repeat([]byte{0x11}, 128<<10)
		v2 := bytes.Repeat([]byte{0x22}, len(v1)+(16<<10))
		var rangedWorkers atomic.Int32
		var fullRequests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Header.Get("Range") {
			case "bytes=0-0":
				writeBareRange(w, r, v1, "")
			case "":
				fullRequests.Add(1)
				writeBareRange(w, r, v2, "")
			default:
				rangedWorkers.Add(1)
				writeBareRange(w, r, v2, "")
			}
		}))
		defer srv.Close()

		d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10})
		_, got := mustGet(t, d, srv.URL+"/file.bin", filepath.Join(t.TempDir(), "file.bin"))
		if !bytes.Equal(got, v2) {
			t.Fatal("validator-less fallback did not produce one coherent representation")
		}
		if got := rangedWorkers.Load(); got != 0 {
			t.Errorf("validator-less download issued %d worker range requests", got)
		}
		if got := fullRequests.Load(); got != 1 {
			t.Errorf("full requests = %d, want 1", got)
		}
	})

	t.Run("checksum permits multipart", func(t *testing.T) {
		t.Parallel()
		data := testData(128 << 10)
		sum := fmt.Sprintf("%x", sha256.Sum256(data))
		var rangedWorkers atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "bytes=0-0" {
				rangedWorkers.Add(1)
			}
			writeBareRange(w, r, data, "")
		}))
		defer srv.Close()

		d := newDL(t, &Options{
			Parts: 4, MinPartSize: 4 << 10, ExpectedSHA256: sum,
		})
		res, got := mustGet(t, d, srv.URL+"/file.bin", filepath.Join(t.TempDir(), "file.bin"))
		if !bytes.Equal(got, data) || res.SHA256 != sum {
			t.Fatal("checksummed multipart download failed")
		}
		if got := rangedWorkers.Load(); got < 2 {
			t.Errorf("checksum-backed multipart issued only %d worker ranges", got)
		}
	})

	t.Run("fallback rejects truncated declared body", func(t *testing.T) {
		t.Parallel()
		probeData := testData(64 << 10)
		fullData := testData(96 << 10)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") == "bytes=0-0" {
				writeBareRange(w, r, probeData, "")
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(fullData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fullData[:len(fullData)/2])
		}))
		defer srv.Close()

		d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10, MaxRetries: 1})
		dest := filepath.Join(t.TempDir(), "file.bin")
		if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); !errors.Is(err, ErrMaxRetry) {
			t.Fatalf("truncated fallback error = %v, want ErrMaxRetry", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("truncated fallback installed destination: %v", err)
		}
	})

	t.Run("fallback rejects truncated chunked body", func(t *testing.T) {
		t.Parallel()
		probeData := testData(64 << 10)
		fullData := testData(96 << 10)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") == "bytes=0-0" {
				writeBareRange(w, r, probeData, "")
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fullData[:len(fullData)/2])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()

		d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10, MaxRetries: 1})
		dest := filepath.Join(t.TempDir(), "file.bin")
		if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); !errors.Is(err, ErrMaxRetry) {
			t.Fatalf("truncated chunked fallback error = %v, want ErrMaxRetry", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("truncated chunked fallback installed destination: %v", err)
		}
	})
}

func TestMultipartRejectsMismatchedContentRange(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			writeBareRange(w, r, data, `"v1"`)
			return
		}
		start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Errorf("worker sent malformed range %q", r.Header.Get("Range"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)+1))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer srv.Close()

	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	_, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
	if err == nil || !strings.Contains(err.Error(), "wrong Content-Range") {
		t.Fatalf("mismatched Content-Range error = %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("mismatched ranges installed destination: %v", statErr)
	}
}

func writePartialState(t *testing.T, dest, sourceURL string, data []byte) {
	t.Helper()
	part := dest + ".part"
	file := make([]byte, len(data))
	half := len(data) / 2
	copy(file[:half], data[:half])
	if err := os.WriteFile(part, file, 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	st := &stateFile{
		Version:  stateVersion,
		SourceID: sourceIdentity(u),
		Size:     int64(len(data)),
		ETag:     `"shared"`,
		Chunks: []chunkState{{
			Off: 0, End: int64(len(data)), Done: int64(half),
		}},
	}
	if err := st.save(statePath(part)); err != nil {
		t.Fatal(err)
	}
}

func TestResumeBoundToSourceResource(t *testing.T) {
	t.Parallel()
	a := bytes.Repeat([]byte{0xA1}, 128<<10)
	b := bytes.Repeat([]byte{0xB2}, len(a))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeBareRange(w, r, b, `"shared"`)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	writePartialState(t, dest, srv.URL+"/resource-a", a)
	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10})
	res, got := mustGet(t, d, srv.URL+"/resource-b", dest)
	if res.Resumed {
		t.Error("resume state from a different source URL was accepted")
	}
	if !bytes.Equal(got, b) {
		t.Fatal("fresh resource B was mixed with resumed bytes from resource A")
	}
}

func TestResumeAllowsRefreshedSignedRedirect(t *testing.T) {
	t.Parallel()
	data := testData(128 << 10)
	var st stats
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/stable", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/cdn/file.bin?signature=two", http.StatusFound)
	})
	mux.Handle("/cdn/file.bin", rangeHandler(data, `"shared"`, &st))
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	writePartialState(t, dest, srv.URL+"/stable", data)
	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	res, got := mustGet(t, d, srv.URL+"/stable", dest)
	if !res.Resumed {
		t.Error("stable source URL did not resume through a refreshed signed redirect")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed signed-redirect download differs from source")
	}
	half := int64(len(data) / 2)
	starts := st.rangeStarts()
	if len(starts) < 2 || starts[1] < half {
		t.Errorf("resume restarted before saved offset %d: %v", half, starts)
	}
}

func TestFinalNoOverwriteIsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	part := dest + ".part"
	newData := []byte("new verified download")
	oldData := []byte("late destination")
	if err := os.WriteFile(part, newData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, oldData, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	d := newDL(t, nil)
	r := &run{d: d, destPath: dest, partPath: part, total: int64(len(newData))}
	if _, err := r.verifyAndFinalize(file, false); !errors.Is(err, ErrDestExists) {
		t.Fatalf("finalization error = %v, want ErrDestExists", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldData) {
		t.Fatalf("late destination was replaced: got %q", got)
	}
	if staged, err := os.ReadFile(part); err != nil || !bytes.Equal(staged, newData) {
		t.Fatalf("verified staging file was not preserved for recovery: data=%q err=%v", staged, err)
	}
}

func TestDestinationLockHonorsCancellation(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "file.bin")
	unlock, err := acquireDestination(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := acquireDestination(ctx, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting lock error = %v, want context.Canceled", err)
	}
	unlock()

	// A canceled waiter must release its registry reference and leave the
	// destination lock usable by later downloads.
	unlock, err = acquireDestination(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestConcurrentGetsSerializeSameDestination(t *testing.T) {
	t.Parallel()
	data := testData(128 << 10)
	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWorker) }) }
	t.Cleanup(release)
	var probes atomic.Int32
	var workers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			probes.Add(1)
			writeBareRange(w, r, data, `"v1"`)
			return
		}
		workers.Add(1)
		startOnce.Do(func() { close(workerStarted) })
		<-releaseWorker
		writeBareRange(w, r, data, `"v1"`)
	}))
	defer srv.Close()

	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	type outcome struct {
		res *Result
		err error
	}
	results := make(chan outcome, 2)
	go func() {
		res, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
		results <- outcome{res: res, err: err}
	}()
	select {
	case <-workerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first download worker did not start")
	}
	go func() {
		res, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
		results <- outcome{res: res, err: err}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for probes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := probes.Load(); got != 2 {
		t.Fatalf("probe requests = %d, want 2 concurrent calls", got)
	}
	if got := workers.Load(); got != 1 {
		t.Fatalf("worker requests before release = %d, want 1 staging owner", got)
	}
	release()

	first, second := <-results, <-results
	var successes, existsErrors int
	for _, result := range []outcome{first, second} {
		switch {
		case result.err == nil && result.res != nil:
			successes++
		case errors.Is(result.err, ErrDestExists):
			existsErrors++
		default:
			t.Errorf("unexpected concurrent result: res=%v err=%v", result.res, result.err)
		}
	}
	if successes != 1 || existsErrors != 1 {
		t.Fatalf("successes=%d ErrDestExists=%d, want one each", successes, existsErrors)
	}
	if got := workers.Load(); got != 1 {
		t.Errorf("worker requests after completion = %d, want 1", got)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("serialized destination contains corrupt data")
	}
}

type scopeReporter struct {
	NopReporter
	mu         sync.Mutex
	active     bool
	starts     int
	dones      int
	violations []string
}

func (r *scopeReporter) Start(info Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active {
		r.violations = append(r.violations, "Start interleaved with active run "+info.Name)
	}
	r.active = true
	r.starts++
}

func (r *scopeReporter) checkActive(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		r.violations = append(r.violations, event+" occurred outside a run")
	}
}

func (r *scopeReporter) ChunkStart(int, int64, int64, int64) { r.checkActive("ChunkStart") }
func (r *scopeReporter) ChunkProgress(int, int, time.Duration) {
	r.checkActive("ChunkProgress")
}
func (r *scopeReporter) ChunkDone(int) { r.checkActive("ChunkDone") }

func (r *scopeReporter) Done(error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		r.violations = append(r.violations, "Done occurred outside a run")
	}
	r.active = false
	r.dones++
}

func TestReporterScopesConcurrentDownloads(t *testing.T) {
	t.Parallel()
	data := testData(128 << 10)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		5*time.Millisecond, 4, func(*http.Request) bool { return true }))
	defer srv.Close()
	rep := &scopeReporter{}
	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10, Reporter: rep})
	dir := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, name := range []string{"a.bin", "b.bin"} {
		go func() {
			<-start
			_, err := d.Get(t.Context(), srv.URL+"/file.bin", filepath.Join(dir, name))
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.starts != 2 || rep.dones != 2 {
		t.Errorf("reporter starts=%d dones=%d, want 2 each", rep.starts, rep.dones)
	}
	if len(rep.violations) != 0 {
		t.Errorf("reporter stream violations: %v", rep.violations)
	}
}

func TestRetryableStatusDropsPinnedNode(t *testing.T) {
	t.Parallel()
	data := testData(32 << 10)
	var badRequests atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	var goodStats stats
	good := httptest.NewServer(rangeHandler(data, `"v1"`, &goodStats))
	defer good.Close()

	d := newDL(t, &Options{
		Parts: 1,
		Proxy: func(*http.Request) (*url.URL, error) {
			return nil, nil
		},
	})
	backends := map[string]string{
		"192.0.2.1:80": strings.TrimPrefix(bad.URL, "http://"),
		"192.0.2.2:80": strings.TrimPrefix(good.URL, "http://"),
	}
	d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		backend, ok := backends[addr]
		if !ok {
			return nil, &net.AddrError{Err: "unexpected", Addr: addr}
		}
		return (&net.Dialer{}).DialContext(ctx, network, backend)
	}

	p := testPicker("192.0.2.1", "192.0.2.2")
	if err := p.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Force the first two selections to the bad node; its second strike
	// bans it, after which the good node must be selected.
	p.nodes[1].conns = 1
	dest := filepath.Join(t.TempDir(), "part")
	file, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(int64(len(data))); err != nil {
		t.Fatal(err)
	}
	sched := newScheduler(4 << 10)
	sched.addPending(0, int64(len(data)), 0)
	grant := sched.next()
	run := &run{
		d: d, url: "http://cdn.test/file.bin", sourceURL: &url.URL{Scheme: "http", Host: "cdn.test"},
		total: int64(len(data)), etag: `"v1"`, partPath: dest,
	}
	w := newWorker(0, run, sched, file, p)
	defer w.dropNode()
	for attempt := 1; attempt <= 2; attempt++ {
		if err := w.attempt(t.Context(), grant.c); !errors.Is(err, StatusError(http.StatusServiceUnavailable)) {
			t.Fatalf("bad attempt %d error = %v", attempt, err)
		}
		if w.node != nil || w.client != nil {
			t.Fatalf("bad attempt %d retained pinned node/client", attempt)
		}
	}
	if err := w.attempt(t.Context(), grant.c); err != nil {
		t.Fatalf("healthy-node attempt failed: %v", err)
	}
	if got := badRequests.Load(); got != 2 {
		t.Errorf("bad-node requests = %d, want 2 before ban", got)
	}
	if len(goodStats.rangeHeaders()) != 1 {
		t.Errorf("healthy node requests = %v, want one", goodStats.rangeHeaders())
	}
}

func TestAdaptiveTimeoutNeverDropsConfiguredBase(t *testing.T) {
	t.Parallel()
	const base = 2 * time.Minute
	d := newDL(t, &Options{Timeout: base})
	w := newWorker(0, &run{d: d}, nil, nil, nil)
	w.bumpTimeout()
	if w.timeout != base {
		t.Errorf("bumpTimeout reduced configured base: got %v, want %v", w.timeout, base)
	}
	w.timeout = base - time.Second
	w.decayTimeout()
	if w.timeout != base {
		t.Errorf("decayTimeout preserved value below base: got %v, want %v", w.timeout, base)
	}

	const huge = time.Duration(1<<63 - 1)
	hugeDownloader := newDL(t, &Options{Timeout: huge})
	hugeWorker := newWorker(0, &run{d: hugeDownloader}, nil, nil, nil)
	hugeWorker.bumpTimeout()
	if hugeWorker.timeout != huge {
		t.Errorf("bumpTimeout overflowed configured base: got %v, want %v", hugeWorker.timeout, huge)
	}
}
