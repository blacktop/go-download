package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testConnIDKey struct{}

type initialReporter struct {
	NopReporter
	connected atomic.Int32
}

func (r *initialReporter) Connected(int, string) { r.connected.Add(1) }

func TestInitialRangeResponseIsWorkerZero(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	inner := rangeHandler(data, `"v1"`, &st)
	var mu sync.Mutex
	var ranges []string
	var connections int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		mu.Lock()
		connections++
		mu.Unlock()
		return ctx
	}
	srv.Start()
	t.Cleanup(srv.Close)

	rep := &initialReporter{}
	d := newDL(t, &Options{Parts: 1, MinPartSize: 4 << 10, Reporter: rep})
	_, got := mustGet(t, d, srv.URL+"/file.bin", filepath.Join(t.TempDir(), "file.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 1 || ranges[0] != "bytes=0-" {
		t.Fatalf("requests = %v, want one useful initial range", ranges)
	}
	if connections != 1 {
		t.Fatalf("TCP connections = %d, want 1", connections)
	}
	if rep.connected.Load() != 1 {
		t.Fatalf("Connected calls = %d, want 1", rep.connected.Load())
	}
}

func TestInitialPlainResponseIsSingleStream(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(plainHandler(data, &st))
	t.Cleanup(srv.Close)

	d := newDL(t, nil)
	_, got := mustGet(t, d, srv.URL+"/file.bin", filepath.Join(t.TempDir(), "file.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if ranges := st.rangeHeaders(); len(ranges) != 1 || ranges[0] != "bytes=0-" {
		t.Fatalf("requests = %v, want one useful initial request", ranges)
	}
}

type countedBody struct {
	r      io.Reader
	reads  atomic.Int32
	closes atomic.Int32
}

func (b *countedBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.r.Read(p)
}

func (b *countedBody) Close() error {
	if n := b.closes.Add(1); n != 1 {
		panic(fmt.Sprintf("response body closed %d times", n))
	}
	return nil
}

func fullRangeResponse(req *http.Request, data []byte, body io.ReadCloser) *http.Response {
	h := make(http.Header)
	h.Set("ETag", `"v1"`)
	h.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
	return &http.Response{
		StatusCode:    http.StatusPartialContent,
		Header:        h,
		ContentLength: int64(len(data)),
		Body:          body,
		Request:       req,
	}
}

func TestCustomTransportInitialBodyConsumedAndClosedOnce(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	body := &countedBody{r: bytes.NewReader(data)}
	var requests atomic.Int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if got := req.Header.Get("Range"); got != "bytes=0-" {
			t.Fatalf("Range = %q, want bytes=0-", got)
		}
		return fullRangeResponse(req, data, body), nil
	})

	d := newDL(t, &Options{Transport: rt, Parts: 1, MinPartSize: 4 << 10})
	_, got := mustGet(t, d, "http://custom.test/file.bin",
		filepath.Join(t.TempDir(), "file.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if requests.Load() != 1 || body.reads.Load() == 0 || body.closes.Load() != 1 {
		t.Fatalf("requests=%d reads=%d closes=%d, want 1, >0, 1",
			requests.Load(), body.reads.Load(), body.closes.Load())
	}
}

type blockingBody struct {
	closed chan struct{}
	once   sync.Once
	closes atomic.Int32
}

func (b *blockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("closed while blocked")
}

func (b *blockingBody) Close() error {
	b.closes.Add(1)
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestInitialBodyStallUsesWorkerTimeout(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	body := &blockingBody{closed: make(chan struct{})}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fullRangeResponse(req, data, body), nil
	})
	d := newDL(t, &Options{
		Transport: rt, Parts: 1, MinPartSize: 4 << 10,
		Timeout: 20 * time.Millisecond, MaxRetries: 1,
	})
	start := time.Now()
	_, err := d.Get(t.Context(), "http://custom.test/file.bin",
		filepath.Join(t.TempDir(), "file.bin"))
	if !errors.Is(err, ErrMaxRetry) {
		t.Fatalf("stalled initial response error = %v, want ErrMaxRetry", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stalled initial response took %v", elapsed)
	}
	if body.closes.Load() != 1 {
		t.Fatalf("underlying body closes = %d, want 1", body.closes.Load())
	}
}

func TestDestinationRejectionClosesInitialWithoutReading(t *testing.T) {
	t.Parallel()
	data := testData(8 << 10)
	body := &countedBody{r: bytes.NewReader(data)}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fullRangeResponse(req, data, body), nil
	})
	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDL(t, &Options{Transport: rt})
	if _, err := d.Get(t.Context(), "http://custom.test/file.bin", dest); !errors.Is(err, ErrDestExists) {
		t.Fatalf("Get error = %v, want ErrDestExists", err)
	}
	if body.reads.Load() != 0 || body.closes.Load() != 1 {
		t.Fatalf("rejected body reads=%d closes=%d, want 0, 1",
			body.reads.Load(), body.closes.Load())
	}
}

func TestResumeClosesInitialBeforeMissingRange(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	const missingStart, missingEnd = 16 << 10, 32 << 10
	initialBody := &countedBody{r: bytes.NewReader(data)}
	var mu sync.Mutex
	var ranges []string
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rangeHeader := req.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rangeHeader)
		mu.Unlock()
		if rangeHeader == "bytes=0-" {
			return fullRangeResponse(req, data, initialBody), nil
		}
		start, end, ok := parseFullRange(rangeHeader, int64(len(data)))
		if !ok {
			return nil, fmt.Errorf("bad Range %q", rangeHeader)
		}
		h := make(http.Header)
		h.Set("ETag", `"v1"`)
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        h,
			ContentLength: end - start + 1,
			Body:          io.NopCloser(bytes.NewReader(data[start : end+1])),
			Request:       req,
		}, nil
	})

	dest := filepath.Join(t.TempDir(), "file.bin")
	part := dest + ".part"
	staged := append([]byte(nil), data...)
	clear(staged[missingStart:missingEnd])
	if err := os.WriteFile(part, staged, 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("http://custom.test/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	state := &stateFile{
		Version: stateVersion, SourceID: sourceIdentity(u), Size: int64(len(data)), ETag: `"v1"`,
		Chunks: []chunkState{{Off: missingStart, End: missingEnd}},
	}
	if err := state.save(statePath(part)); err != nil {
		t.Fatal(err)
	}

	d := newDL(t, &Options{Transport: rt, Parts: 1, MinPartSize: 4 << 10})
	res, got := mustGet(t, d, u.String(), dest)
	if !res.Resumed || !bytes.Equal(got, data) {
		t.Fatal("resume did not reconstruct the original bytes")
	}
	if initialBody.reads.Load() != 0 || initialBody.closes.Load() != 1 {
		t.Fatalf("initial body reads=%d closes=%d, want 0, 1",
			initialBody.reads.Load(), initialBody.closes.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	wantRange := "bytes=" + strconv.Itoa(missingStart) + "-" + strconv.Itoa(missingEnd-1)
	if len(ranges) != 2 || ranges[0] != "bytes=0-" || ranges[1] != wantRange {
		t.Fatalf("ranges = %v, want [bytes=0- %s]", ranges, wantRange)
	}
}

func TestCappedInitialWithoutValidatorFallsBackToFullGET(t *testing.T) {
	t.Parallel()
	probeData := testData(64 << 10)
	fullData := testData(96 << 10)
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rangeHeader)
		mu.Unlock()
		if rangeHeader == "bytes=0-" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-1023/%d", len(probeData)))
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(probeData[:1024])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(fullData)))
		_, _ = w.Write(fullData)
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10})
	_, got := mustGet(t, d, srv.URL+"/file.bin", filepath.Join(t.TempDir(), "file.bin"))
	if !bytes.Equal(got, fullData) {
		t.Fatal("fallback did not use one coherent full response")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 2 || ranges[0] != "bytes=0-" || ranges[1] != "" {
		t.Fatalf("ranges = %v, want initial range then full GET", ranges)
	}
}

// signalCloseBody signals closedCh exactly once when first closed.
type signalCloseBody struct {
	r        io.Reader
	once     sync.Once
	closedCh chan struct{}
}

func (b *signalCloseBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *signalCloseBody) Close() error {
	b.once.Do(func() { close(b.closedCh) })
	return nil
}

// TestContendedDestinationClosesInitialBeforeWaiting: a Get that loses the
// destination lock must not park its open, unread initial stream for the
// holder's whole tenure — it closes the initial response BEFORE waiting and
// re-requests after acquiring. The test itself owns the destination lock,
// so the ordering needs no scheduling assumption: the close must be
// observed while the test still holds the lock.
func TestContendedDestinationClosesInitialBeforeWaiting(t *testing.T) {
	t.Parallel()
	data := testData(32 << 10)
	initialClosed := make(chan struct{})
	var requests atomic.Int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 { // the contended initial
			return fullRangeResponse(req, data,
				&signalCloseBody{r: bytes.NewReader(data), closedCh: initialClosed}), nil
		}
		// The fresh request issued after the lock is finally acquired.
		return fullRangeResponse(req, data, io.NopCloser(bytes.NewReader(data))), nil
	})

	dest := filepath.Join(t.TempDir(), "file.bin")
	unlock, err := acquireDestination(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}

	d := newDL(t, &Options{Transport: rt, Parts: 1, MinPartSize: 4 << 10})
	errCh := make(chan error, 1)
	go func() {
		_, err := d.Get(t.Context(), "http://custom.test/file.bin", dest)
		errCh <- err
	}()

	// The loser must dispose of its initial response while the lock is still
	// held here — before, not after, the contended wait resolves.
	select {
	case <-initialClosed:
	case err := <-errCh:
		t.Fatalf("Get finished (err=%v) without closing its initial first", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Get never closed its initial response while waiting for the lock")
	}
	select {
	case err := <-errCh:
		t.Fatalf("Get finished (err=%v) while the destination lock was still held", err)
	default:
	}

	unlock()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("destination bytes differ after the contended download")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want the closed initial plus one fresh request", got)
	}
}

// ctxOnlyBody unblocks Read ONLY via request-context cancellation: Close is
// recorded but deliberately does not wake a blocked Read. This is the
// custom-transport oracle for genuine election-request cancellation.
type ctxOnlyBody struct {
	ctx    context.Context
	closes atomic.Int32
}

func (b *ctxOnlyBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, context.Cause(b.ctx)
}

func (b *ctxOnlyBody) Close() error {
	b.closes.Add(1)
	return nil
}

// TestInitialStallCancelsElectionRequest: a stalled initial body on a custom
// transport must be woken by cancelling the election REQUEST — there is no
// contract that Body.Close unblocks Read. The retry then completes normally.
func TestInitialStallCancelsElectionRequest(t *testing.T) {
	t.Parallel()
	data := testData(16 << 10)
	stalled := &ctxOnlyBody{}
	var requests atomic.Int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			stalled.ctx = req.Context()
			return fullRangeResponse(req, data, stalled), nil
		}
		rangeHeader := req.Header.Get("Range")
		start, end, ok := parseFullRange(rangeHeader, int64(len(data)))
		if !ok {
			return nil, fmt.Errorf("bad Range %q", rangeHeader)
		}
		h := make(http.Header)
		h.Set("ETag", `"v1"`)
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        h,
			ContentLength: end - start + 1,
			Body:          io.NopCloser(bytes.NewReader(data[start : end+1])),
			Request:       req,
		}, nil
	})

	d := newDL(t, &Options{
		Transport: rt, Parts: 1, MinPartSize: 4 << 10,
		Timeout: 20 * time.Millisecond, MaxRetries: 3,
	})
	start := time.Now()
	dest := filepath.Join(t.TempDir(), "file.bin")
	res, got := mustGet(t, d, "http://custom.test/file.bin", dest)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stall recovery took %v: the election request was not cancelled", elapsed)
	}
	if res.Size != int64(len(data)) || !bytes.Equal(got, data) {
		t.Fatal("retry after cancelled stall produced wrong bytes")
	}
}
