package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedBody is an io.ReadCloser with programmable reads for drainProbe
// hardening cases.
type scriptedBody struct {
	reads    []scriptedRead
	i        int
	closes   atomic.Int32
	closeErr error
	block    chan struct{} // non-nil: first read blocks until Close
}

type scriptedRead struct {
	n   int
	err error
}

func (b *scriptedBody) Read(p []byte) (int, error) {
	if b.block != nil {
		<-b.block
		return 0, errors.New("closed while blocked")
	}
	if b.i >= len(b.reads) {
		return 0, io.EOF
	}
	r := b.reads[b.i]
	b.i++
	for j := 0; j < r.n && j < len(p); j++ {
		p[j] = 0xAB
	}
	return r.n, r.err
}

func (b *scriptedBody) Close() error {
	if b.closes.Add(1) == 1 && b.block != nil {
		close(b.block)
	}
	return b.closeErr
}

func TestDrainProbeHardening(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body *scriptedBody
		want bool
	}{
		{name: "exact one byte then EOF (same read)",
			body: &scriptedBody{reads: []scriptedRead{{1, io.EOF}}}, want: true},
		{name: "one byte then clean EOF (separate read)",
			body: &scriptedBody{reads: []scriptedRead{{1, nil}, {0, io.EOF}}}, want: true},
		{name: "oversized body forfeits",
			body: &scriptedBody{reads: []scriptedRead{{2, nil}}}, want: false},
		{name: "chunked never-ending forfeits",
			body: &scriptedBody{reads: []scriptedRead{{1, nil}, {1, nil}}}, want: false},
		{name: "empty body forfeits",
			body: &scriptedBody{reads: []scriptedRead{{0, io.EOF}}}, want: false},
		{name: "read error forfeits",
			body: &scriptedBody{reads: []scriptedRead{{1, errors.New("reset")}}}, want: false},
		{name: "close error forfeits",
			body: &scriptedBody{reads: []scriptedRead{{1, io.EOF}}, closeErr: errors.New("nope")},
			want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := drainProbe(tc.body, 200*time.Millisecond)
			if got != tc.want {
				t.Fatalf("drainProbe = %t, want %t", got, tc.want)
			}
			if tc.body.closes.Load() < 1 {
				t.Fatal("body never closed")
			}
		})
	}
}

func TestDrainProbeStalledBodyIsBounded(t *testing.T) {
	t.Parallel()
	body := &scriptedBody{block: make(chan struct{})}
	start := time.Now()
	if drainProbe(body, 100*time.Millisecond) {
		t.Fatal("stalled body must forfeit reuse")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain took %v; the budget must bound it", elapsed)
	}
	if body.closes.Load() < 1 {
		t.Fatal("stalled body was not closed by the budget timer")
	}
}

// TestCustomTransportProbeBodyUntouched: with a user RoundTripper the drain
// must not run — the probe body is never read and is closed exactly once by
// the existing synchronous path (singleCloseBody panics on a second close).
func TestCustomTransportProbeBodyUntouched(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Set("ETag", `"v1"`)
		if req.Header.Get("Range") == "bytes=0-0" {
			h.Set("Content-Range", "bytes 0-0/"+strconv.Itoa(len(data)))
			return &http.Response{
				StatusCode:    http.StatusPartialContent,
				Header:        h,
				ContentLength: 1,
				Body:          &panicOnReadBody{},
				Request:       req,
			}, nil
		}
		start, end, _ := parseFullRange(req.Header.Get("Range"), int64(len(data)))
		body := data[start : end+1]
		h.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+
			strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(data)))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        h,
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(bytes.NewReader(body)),
			Request:       req,
		}, nil
	})

	d := newDL(t, &Options{Transport: rt, MinPartSize: 4 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := d.Get(t.Context(), "http://custom.test/file.bin", dest); err != nil {
		t.Fatal(err)
	}
}

// panicOnReadBody proves the probe body is not read; its embedded
// singleCloseBody panics on a second Close.
type panicOnReadBody struct{ singleCloseBody }

func (*panicOnReadBody) Read([]byte) (int, error) { panic("probe body read on custom transport") }

type probeConnIDKey struct{}

func TestProbeConnectionReusedByPinnedWorkerZero(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	inner := rangeHandler(data, `"v1"`, &st)
	type observedRequest struct {
		rangeHeader string
		connID      int
	}
	var mu sync.Mutex
	var requests []observedRequest
	var connections int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(probeConnIDKey{}).(int)
		mu.Lock()
		requests = append(requests, observedRequest{r.Header.Get("Range"), id})
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		mu.Lock()
		connections++
		id := connections
		mu.Unlock()
		return context.WithValue(ctx, probeConnIDKey{}, id)
	}
	srv.Start()
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 2 {
		t.Fatalf("requests = %v, want election and worker range", requests)
	}
	if requests[0].rangeHeader != "bytes=0-0" {
		t.Fatalf("first request Range = %q, want election probe", requests[0].rangeHeader)
	}
	if requests[0].connID == 0 || requests[0].connID != requests[1].connID {
		t.Fatalf("probe connection %d, first worker connection %d; want exact reuse",
			requests[0].connID, requests[1].connID)
	}
	if connections != 1 {
		t.Fatalf("TCP connections = %d, want 1", connections)
	}
}

func TestSharedLeaseDialMarkerFailsWithoutDial(t *testing.T) {
	t.Parallel()
	d := newDL(t, nil)
	var calls atomic.Int32
	d.dial = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("real dial called")
	}
	ctx := context.WithValue(t.Context(), sharedLeaseContextKey{}, struct{}{})
	if _, err := d.dialContext(ctx, "tcp", "example.test:80"); !errors.Is(err, errSharedLeaseMiss) {
		t.Fatalf("dial error = %v, want errSharedLeaseMiss", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("real dial calls = %d, want 0", got)
	}
}

type sharedLeaseReporter struct {
	NopReporter
	connected atomic.Int32
	progress  atomic.Int32
	retries   atomic.Int32
}

func (r *sharedLeaseReporter) Connected(int, string) {
	r.connected.Add(1)
}

func (r *sharedLeaseReporter) ChunkProgress(int, int, time.Duration) {
	r.progress.Add(1)
}

func (r *sharedLeaseReporter) ChunkRetry(int, int, error) {
	r.retries.Add(1)
}

func TestSharedLeaseMissResponseCannotWrite(t *testing.T) {
	t.Parallel()
	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	rep := &sharedLeaseReporter{}
	r := &run{d: d, rep: rep, url: "http://example.test/file.bin", total: 4096, etag: `"v1"`}
	sched := newScheduler(1)
	sched.addPending(0, r.total, 0)
	c := sched.next(0)
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "part"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if err := file.Truncate(r.total); err != nil {
		t.Fatal(err)
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	w := newWorker(0, r, sched, file, nil)
	t.Cleanup(w.releaseBuf)
	w.client = d.newClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		if trace == nil || trace.GotConn == nil {
			t.Fatal("shared request missing GotConn trace")
		}
		trace.GotConn(httptrace.GotConnInfo{Conn: clientConn, Reused: false})
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        http.Header{"Content-Range": {"bytes 0-4095/4096"}},
			ContentLength: r.total,
			Body:          &panicOnReadBody{},
			Request:       req,
		}, nil
	}))
	w.sharedClient = true
	if err := w.attempt(t.Context(), c); !errors.Is(err, errSharedLeaseMiss) {
		t.Fatalf("attempt error = %v, want errSharedLeaseMiss", err)
	}
	if w.sharedClient || w.client != nil {
		t.Fatal("shared client remained attached after its one attempt")
	}
	if off, _, _ := sched.cursor(c); off != 0 {
		t.Fatalf("claim cursor = %d, want 0", off)
	}
	if rep.connected.Load() != 0 || rep.progress.Load() != 0 {
		t.Fatalf("rejected lease events: connected=%d progress=%d",
			rep.connected.Load(), rep.progress.Load())
	}
}

func TestSharedLeaseMissFallsBackToPinnedWithoutRetry(t *testing.T) {
	t.Parallel()
	data := testData(32 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	rep := &sharedLeaseReporter{}
	r := &run{d: d, rep: rep, url: srv.URL + "/file.bin", total: int64(len(data)), etag: `"v1"`}
	sched := newScheduler(1)
	sched.addPending(0, r.total, 0)
	c := sched.next(0)
	part := filepath.Join(t.TempDir(), "part")
	file, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if err := file.Truncate(r.total); err != nil {
		t.Fatal(err)
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	picker := newPicker(u.Hostname(), portOf(u), d.log)
	w := newWorker(0, r, sched, file, picker)
	t.Cleanup(w.releaseBuf)
	t.Cleanup(w.dropNode)
	w.client = d.newClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		trace.GotConn(httptrace.GotConnInfo{Conn: clientConn, Reused: false})
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        http.Header{"Content-Range": {"bytes 0-32767/32768"}},
			ContentLength: int64(len(data)),
			Body:          &panicOnReadBody{},
			Request:       req,
		}, nil
	}))
	w.sharedClient = true
	if err := w.downloadChunk(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if got := rep.retries.Load(); got != 0 {
		t.Fatalf("ChunkRetry calls = %d, want 0", got)
	}
	if got := rep.connected.Load(); got != 1 {
		t.Fatalf("Connected calls = %d, want only the accepted pinned attempt", got)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("pinned fallback did not write the expected bytes")
	}
}

// TestElectionConnectionCloseForfeitsReuse: a Connection: close election
// response must not be drained for reuse — the download still succeeds and
// the first worker range arrives on a fresh connection.
func TestElectionConnectionCloseForfeitsReuse(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	inner := rangeHandler(data, `"v1"`, &st)
	var mu sync.Mutex
	var connIDs []int
	var connections int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Connection", "close")
		}
		id, _ := r.Context().Value(probeConnIDKey{}).(int)
		mu.Lock()
		connIDs = append(connIDs, id)
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		mu.Lock()
		connections++
		id := connections
		mu.Unlock()
		return context.WithValue(ctx, probeConnIDKey{}, id)
	}
	srv.Start()
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connIDs) < 2 {
		t.Fatalf("observed %d requests, want election and worker range", len(connIDs))
	}
	if connIDs[0] == connIDs[1] {
		t.Fatal("worker reused the Connection: close election connection")
	}
}

// TestUnpinnedProbeConnectionReused: with pinning disabled (proxied run) the
// drained election connection sits in the base pool and the first worker
// range reuses it — proven by client-side connection identity at the proxy.
func TestUnpinnedProbeConnectionReused(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	origin := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(origin.Close)

	var mu sync.Mutex
	var connIDs []int
	var connections int
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(probeConnIDKey{}).(int)
		mu.Lock()
		connIDs = append(connIDs, id)
		mu.Unlock()
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	proxy.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		mu.Lock()
		connections++
		id := connections
		mu.Unlock()
		return context.WithValue(ctx, probeConnIDKey{}, id)
	}
	proxy.Start()
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	d := newDL(t, &Options{
		Parts: 1, MinPartSize: 4 << 10,
		Proxy: func(*http.Request) (*url.URL, error) { return proxyURL, nil },
	})
	if d.pinningEnabled(origin.URL + "/file.bin") {
		t.Fatal("proxied run must be unpinned")
	}
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := d.Get(t.Context(), origin.URL+"/file.bin", dest); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connIDs) < 2 {
		t.Fatalf("observed %d proxied requests, want election and worker range", len(connIDs))
	}
	if connIDs[0] != connIDs[1] {
		t.Fatalf("probe conn %d, first worker conn %d; want unpinned base-pool reuse",
			connIDs[0], connIDs[1])
	}
}

// TestSharedLeaseStalePoolFailsClosedWithoutDial: when the pooled probe
// connection has died (or been reaped) before worker 0's shared attempt, the
// transport needs a dial — including via its transparent stale-reuse retry —
// and the reuse-only marker must fail that dial closed. The worker then
// falls back to a pinned client with no real dial charged to the shared
// attempt, no retry event, and a byte-identical result.
func TestSharedLeaseStalePoolFailsClosedWithoutDial(t *testing.T) {
	t.Parallel()
	data := testData(32 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10})
	var realDials atomic.Int32
	origDial := d.dial
	d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		realDials.Add(1)
		return origDial(ctx, network, addr)
	}

	// Pool one connection the way the drained election probe does, then kill
	// it server-side so the shared attempt cannot find a live reusable conn.
	seed, err := d.newClient(d.base).Get(srv.URL + "/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, seed.Body); err != nil {
		t.Fatal(err)
	}
	seed.Body.Close()
	srv.CloseClientConnections()
	time.Sleep(20 * time.Millisecond) // let the transport reap the dead conn
	realDials.Store(0)                // exclude the seed connection

	rep := &sharedLeaseReporter{}
	r := &run{d: d, rep: rep, url: srv.URL + "/file.bin", total: int64(len(data)), etag: `"v1"`}
	sched := newScheduler(1)
	sched.addPending(0, r.total, 0)
	c := sched.next(0)
	part := filepath.Join(t.TempDir(), "part")
	file, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if err := file.Truncate(r.total); err != nil {
		t.Fatal(err)
	}

	picker := newPicker(u.Hostname(), portOf(u), d.log)
	w := newWorker(0, r, sched, file, picker)
	t.Cleanup(w.releaseBuf)
	t.Cleanup(w.dropNode)
	w.client = d.newClient(d.base)
	w.sharedClient = true
	if err := w.downloadChunk(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if got := realDials.Load(); got != 1 {
		t.Fatalf("real dials after seeding = %d, want only the pinned fallback dial", got)
	}
	if got := rep.retries.Load(); got != 0 {
		t.Fatalf("ChunkRetry calls = %d, want 0 (lease miss must not charge the budget)", got)
	}
	if w.sharedClient {
		t.Fatal("shared client remained attached after the miss")
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("pinned fallback did not write the expected bytes")
	}
}

// TestSharedDetachKeepsBasePool: detaching a shared client must never drain
// the base transport's pool — a pooled connection survives the detach and is
// reused by the next base-client request.
func TestSharedDetachKeepsBasePool(t *testing.T) {
	t.Parallel()
	data := testData(8 << 10)
	var st stats
	inner := rangeHandler(data, `"v1"`, &st)
	var mu sync.Mutex
	var connIDs []int
	var connections int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(probeConnIDKey{}).(int)
		mu.Lock()
		connIDs = append(connIDs, id)
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		mu.Lock()
		connections++
		id := connections
		mu.Unlock()
		return context.WithValue(ctx, probeConnIDKey{}, id)
	}
	srv.Start()
	t.Cleanup(srv.Close)

	d := newDL(t, nil)
	client := d.newClient(d.base)
	resp, err := client.Get(srv.URL + "/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	w := &worker{client: d.newClient(d.base), sharedClient: true}
	w.dropNode()
	if w.sharedClient || w.client != nil {
		t.Fatal("dropNode did not detach the shared client")
	}

	resp, err = client.Get(srv.URL + "/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(connIDs) != 2 || connIDs[0] != connIDs[1] {
		t.Fatalf("conn ids = %v (total conns %d); want both requests on one pooled connection",
			connIDs, connections)
	}
}
