package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// retireHarness builds a real run/scheduler/worker trio against srv and
// returns a retire func mirroring runWorkers' pool-level demote hook.
type retireHarness struct {
	t       *testing.T
	r       *run
	sched   *scheduler
	file    *os.File
	cancels map[int]context.CancelCauseFunc
	mu      sync.Mutex
}

func newRetireHarness(t *testing.T, d *Downloader, srvURL string, total int64) *retireHarness {
	t.Helper()
	part := filepath.Join(t.TempDir(), "h.part")
	file, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if err := file.Truncate(total); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srvURL + "/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	sched := newScheduler(1 << 10)
	sched.addPending(0, total, 0)
	r := &run{
		d: d, rep: NopReporter{}, url: srvURL + "/file.bin", sourceURL: u,
		destPath: part[:len(part)-5], partPath: part,
		total: total, etag: `"v1"`,
	}
	return &retireHarness{
		t: t, r: r, sched: sched, file: file,
		cancels: make(map[int]context.CancelCauseFunc),
	}
}

// start runs worker id in a goroutine and returns a channel carrying its
// run() result.
func (h *retireHarness) start(ctx context.Context, id int) <-chan error {
	wctx, wcancel := context.WithCancelCause(ctx)
	h.mu.Lock()
	h.cancels[id] = wcancel
	h.mu.Unlock()
	w := newWorker(id, h.r, h.sched, h.file)
	done := make(chan error, 1)
	go func() { done <- w.run(wctx) }()
	return done
}

// retire mirrors runWorkers' pool hook: scheduler demote first, then cancel
// exactly the returned victims with the retirement cause.
func (h *retireHarness) retire(keep int) []int {
	victims := h.sched.demote(keep)
	for _, id := range victims {
		h.mu.Lock()
		wcancel := h.cancels[id]
		h.mu.Unlock()
		if wcancel != nil {
			wcancel(errWorkerRetired)
		}
	}
	return victims
}

func waitPrompt(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not exit promptly after retirement", what)
		return nil
	}
}

// TestDemoteWakesBackoffSleep proves retirement cancellation interrupts an
// exponential-backoff sleep via the channel-coordinated sleep seam — no
// wall-clock assertion involved.
func TestDemoteWakesBackoffSleep(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		w.WriteHeader(http.StatusServiceUnavailable) // every attempt: retryable
	}))
	t.Cleanup(srv.Close)

	sleeping := make(chan struct{}, 16)
	d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10, MaxRetries: 10})
	d.sleepHook = func(ctx context.Context, _ time.Duration) error {
		select {
		case sleeping <- struct{}{}:
		default:
		}
		<-ctx.Done() // block until cancelled: proves the wake, not the clock
		return context.Cause(ctx)
	}

	h := newRetireHarness(t, d, srv.URL, int64(len(data)))
	// A second (keeper) worker must exist or demote(1) has no excess.
	h.sched.next(0) // register keeper 0 and grant it the only chunk
	done := h.start(context.Background(), 1)
	<-sleeping // worker 1 split a chunk, got 503s, and is parked in backoff

	victims := h.retire(1)
	err := waitPrompt(t, done, "backoff-sleeping worker")
	if err != nil {
		t.Fatalf("retired worker surfaced error: %v (victims %v)", err, victims)
	}
}

// TestDemoteCancelsStalledRead proves retirement wakes a worker blocked in a
// body read on a stalled connection.
func TestDemoteCancelsStalledRead(t *testing.T) {
	t.Parallel()
	data := testData(256 << 10)
	served := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Errorf("unranged request %q", r.Header.Get("Range"))
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range",
			"bytes "+itoa(start)+"-"+itoa(end)+"/"+itoa(int64(len(data))))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : start+1024])
		if f, okf := w.(http.Flusher); okf {
			f.Flush()
		}
		once.Do(func() { close(served) })
		<-release // stall forever until the test ends
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10, Timeout: time.Hour})
	h := newRetireHarness(t, d, srv.URL, int64(len(data)))
	keeper := h.sched.next(0) // keeper holds a registration
	done := h.start(context.Background(), 1)
	<-served // worker 1 is mid-read on the stalled body

	h.retire(1)
	if err := waitPrompt(t, done, "stalled-read worker"); err != nil {
		t.Fatalf("retired worker surfaced error: %v", err)
	}
	_ = keeper
}

// TestDemoteCancelsRequestSetup proves retirement wakes a worker blocked
// before any response exists: in dial, and in response-header wait.
func TestDemoteCancelsRequestSetup(t *testing.T) {
	t.Parallel()
	t.Run("blocked dial", func(t *testing.T) {
		t.Parallel()
		d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10})
		dialing := make(chan struct{}, 4)
		d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			select {
			case dialing <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		h := newRetireHarness(t, d, "http://cdn.invalid", 64<<10)
		h.sched.next(0)
		done := h.start(context.Background(), 1)
		<-dialing
		h.retire(1)
		if err := waitPrompt(t, done, "dial-blocked worker"); err != nil {
			t.Fatalf("retired worker surfaced error: %v", err)
		}
	})

	t.Run("blocked response header", func(t *testing.T) {
		t.Parallel()
		got := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(got) })
			<-release // never answer
		}))
		t.Cleanup(func() { close(release); srv.Close() })
		d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10})
		h := newRetireHarness(t, d, srv.URL, 64<<10)
		h.sched.next(0)
		done := h.start(context.Background(), 1)
		<-got
		h.retire(1)
		if err := waitPrompt(t, done, "header-blocked worker"); err != nil {
			t.Fatalf("retired worker surfaced error: %v", err)
		}
	})
}

// TestDemoteDuringThrottleWait proves retirement wakes the flat 429
// politeness pause and the retired worker's shrunken chunk completes cleanly.
func TestDemoteDuringThrottleWait(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	sleeping := make(chan struct{}, 16)
	d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10, MaxRetries: 4})
	d.sleepHook = func(ctx context.Context, _ time.Duration) error {
		select {
		case sleeping <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return context.Cause(ctx)
	}
	h := newRetireHarness(t, d, srv.URL, int64(len(data)))
	h.sched.next(0)
	done := h.start(context.Background(), 1)
	<-sleeping
	h.retire(1)
	if err := waitPrompt(t, done, "throttle-waiting worker"); err != nil {
		t.Fatalf("retired worker surfaced error: %v", err)
	}
}

// TestRetirementDoesNotMaskContentChanged: a genuine permanent error from a
// worker that is being retired must still fail the download; the one-sided
// requirement is that the outcome is NEVER a silent clean exit with an
// unserved chunk and never the internal retirement sentinel.
func TestRetirementDoesNotMaskContentChanged(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	responded := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validator mismatch: full 200 despite If-Range → content changed.
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		select {
		case responded <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10})
	h := newRetireHarness(t, d, srv.URL, int64(len(data)))
	h.sched.next(0)
	done := h.start(context.Background(), 1)
	<-responded
	// Race retirement against the in-flight permanent classification.
	h.retire(1)
	err := waitPrompt(t, done, "content-changed worker")
	if err != nil {
		if errors.Is(err, errWorkerRetired) {
			t.Fatalf("internal retirement sentinel surfaced: %v", err)
		}
		if !errors.Is(err, errContentChanged) {
			t.Fatalf("unexpected error class: %v", err)
		}
		return // permanent error won the race: correct
	}
	// Clean exit is acceptable ONLY if the worker's work was demote-moved:
	// its chunk must not be stranded incomplete in active.
	if h.sched.idle() {
		t.Fatal("clean exit with all work gone: nothing was downloaded")
	}
}

func TestCallerCancellationWinsPermanentResponseRace(t *testing.T) {
	t.Parallel()
	const total = 64 << 10
	workerEntered := make(chan struct{})
	releaseResponse := make(chan struct{})
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("ETag", `"v1"`)
		if req.Header.Get("Range") == "bytes=0-" {
			header.Set("Content-Range", "bytes 0-0/"+strconv.Itoa(total))
			return &http.Response{
				StatusCode:    http.StatusPartialContent,
				Header:        header,
				ContentLength: 1,
				Body:          &singleCloseBody{},
				Request:       req,
			}, nil
		}
		close(workerEntered)
		<-releaseResponse
		// The response is permanently malformed, but the caller cancellation
		// already owns the public outcome.
		header.Set("Content-Range", "bytes 1-1/"+strconv.Itoa(total))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        header,
			ContentLength: 1,
			Body:          &singleCloseBody{},
			Request:       req,
		}, nil
	})

	d := newDL(t, &Options{Transport: rt, Parts: 1, MinPartSize: 4 << 10})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	dest := filepath.Join(t.TempDir(), "file.bin")
	go func() {
		_, err := d.Get(ctx, "http://cancel.test/file.bin", dest)
		done <- err
	}()
	<-workerEntered
	cancel()
	close(releaseResponse)
	if err := waitPrompt(t, done, "caller-cancelled worker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("download error = %v, want caller context cancellation", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// demotionReporter records the reporter stream through a governor demotion.
type demotionReporter struct {
	NopReporter
	mu       sync.Mutex
	started  map[int]int
	dones    map[int]int
	lengths  map[int]int64 // last known length per id
	resizeUp bool
	preStart bool
}

func newDemotionReporter() *demotionReporter {
	return &demotionReporter{
		started: map[int]int{}, dones: map[int]int{}, lengths: map[int]int64{},
	}
}

func (r *demotionReporter) ChunkStart(id int, _, length, _ int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started[id]++
	r.lengths[id] = length
}

func (r *demotionReporter) ChunkResize(id int, length int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started[id] == 0 {
		r.preStart = true
	}
	if length > r.lengths[id] {
		r.resizeUp = true
	}
	r.lengths[id] = length
}

func (r *demotionReporter) ChunkDone(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dones[id]++
}

// TestRampDemotesOnFlatThroughput is the control-plane oracle: on a
// shared-cap (saturated) link the ramp must probe, judge the batch flat, and
// retire it — steady-state concurrent ranged bodies return to exactly one.
func TestRampDemotesOnFlatThroughput(t *testing.T) {
	t.Parallel()
	// 16 MiB at a 4 MB/s shared cap (~4s): stabilized decision windows
	// (512 KiB each) average enough limiter quanta that the flat verdict is
	// unambiguous, and leave ample post-demotion runway under -race.
	data := testData(16 << 20)
	var st stats
	var flows flowLog
	srv := httptest.NewServer(sharedCapHandler(data, `"v1"`, &st,
		newSharedLimiter(4<<20), &flows))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 4, MinPartSize: 256 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	res, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size = %d, want %d", len(got), len(data))
	}

	// Server-observed flow oracle: after the probe raises concurrency above
	// one, retirement restores exactly one flow for a sustained span.
	// ChunkResize cannot timestamp this reliably because it also reports
	// ordinary tail steals, including steals near EOF.
	// The oracle is vacuous unless a second flow was actually admitted: a
	// zero firstAtLeast(2) would make longestSpan measure the entire
	// single-flow run and trivially clear the threshold.
	probed := flows.firstAtLeast(2)
	if probed.IsZero() {
		t.Fatal("ramp never admitted a second flow: nothing was probed or retired")
	}
	single := flows.longestSpan(func(c int) bool { return c == 1 }, probed)
	if single < 400*time.Millisecond {
		t.Fatalf("longest one-flow span after probing = %v, want at least 400ms", single)
	}
}

// TestDemoteThenInterruptThenResume: interrupting a download after demotion
// leaves a gap-free sidecar, and the resume completes byte-identical.
func TestDemoteThenInterruptThenResume(t *testing.T) {
	t.Parallel()
	data := testData(4 << 20)
	var st stats
	srv := httptest.NewServer(sharedCapHandler(data, `"v1"`, &st,
		newSharedLimiter(2<<20), nil)) // ~2s total: time to interrupt mid-flight
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "file.bin")
	ctx, cancel := context.WithTimeout(t.Context(), 900*time.Millisecond)
	defer cancel()
	d := newDL(t, &Options{Parts: 4, MinPartSize: 32 << 10})
	if _, err := d.Get(ctx, srv.URL+"/file.bin", dest); err == nil {
		t.Fatal("expected interruption")
	}

	// Sidecar must exist, be loadable, and cover [0,total) without gaps or
	// overlaps when unioned with durable bytes.
	side := loadState(dest + ".part.json")
	if side == nil {
		t.Fatal("no valid sidecar after interruption")
	}
	chunks := append([]chunkState(nil), side.Chunks...)
	for i, c := range chunks {
		if c.Off < 0 || c.End > int64(len(data)) || c.Off+c.Done > c.End {
			t.Fatalf("sidecar chunk %d malformed: %+v", i, c)
		}
	}

	res, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed {
		t.Fatal("second run did not resume")
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed bytes differ from source")
	}
}

// TestReporterContractUnderDemotion pins event cardinality and ordering
// through a demotion: starts once per id, dones once per started id, resizes
// only after start and never growing.
func TestReporterContractUnderDemotion(t *testing.T) {
	t.Parallel()
	data := testData(6 << 20)
	var st stats
	srv := httptest.NewServer(sharedCapHandler(data, `"v1"`, &st,
		newSharedLimiter(6<<20), nil))
	t.Cleanup(srv.Close)

	rep := newDemotionReporter()
	d := newDL(t, &Options{Parts: 4, MinPartSize: 64 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := d.Do(t.Context(), &Request{
		URL: srv.URL + "/file.bin", Dest: dest, Reporter: rep,
	}); err != nil {
		t.Fatal(err)
	}

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.preStart {
		t.Error("ChunkResize before ChunkStart")
	}
	if rep.resizeUp {
		t.Error("ChunkResize grew a chunk")
	}
	for id, n := range rep.started {
		if n != 1 {
			t.Errorf("ChunkStart fired %d times for id %d", n, id)
		}
		if rep.dones[id] != 1 {
			t.Errorf("ChunkDone fired %d times for started id %d", rep.dones[id], id)
		}
	}
	for id := range rep.dones {
		if rep.started[id] == 0 {
			t.Errorf("ChunkDone for never-started id %d", id)
		}
	}
}

// TestRetirementDoesNotMaskWriteFailure: a destination write failure from a
// worker that is being retired must still fail the download. The part file
// is opened read-only so every write deterministically fails; retirement
// racing that failure must never produce a clean exit with the chunk marked
// complete, and never surface the internal retirement sentinel.
func TestRetirementDoesNotMaskWriteFailure(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	responded := make(chan struct{}, 4)
	var st stats
	inner := rangeHandler(data, `"v1"`, &st)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		select {
		case responded <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10})
	h := newRetireHarness(t, d, srv.URL, int64(len(data)))
	// Reopen the part file read-only: pwrite fails on the first buffer.
	ro, err := os.Open(h.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	h.file = ro

	h.sched.next(0) // register keeper 0 and grant it the only chunk
	done := h.start(context.Background(), 1)
	<-responded
	// Race retirement against the in-flight write failure.
	h.retire(1)
	err = waitPrompt(t, done, "write-failure worker")
	if err != nil {
		if errors.Is(err, errWorkerRetired) {
			t.Fatalf("internal retirement sentinel surfaced: %v", err)
		}
		return // the write failure won the race: correct
	}
	// Clean exit is acceptable ONLY if retirement won before any write was
	// attempted: the chunk must not have been marked complete.
	if h.sched.idle() {
		t.Fatal("clean exit with all work gone: a failed write was marked done")
	}
}

// TestConstrainedRampReachesAndKeepsFourFlows is the control-plane oracle
// for the per-flow-constrained benchmark scenario: with every connection
// individually throttled, the ramp must reach Parts=4 flows and RETAIN them
// after admission settling — throughput alone cannot distinguish four flows
// from a lucky two, and a demotion regression would silently halve
// parallelism. The useful initial response is the first accounted flow.
func TestConstrainedRampReachesAndKeepsFourFlows(t *testing.T) {
	t.Parallel()
	data := testData(8 << 20)
	flows := &flowLog{}
	var st stats
	// 20ms per 64KiB (~3.2 MB/s per connection): slow enough that the
	// per-flow cap stays sleep-dominated even on a starved 2-vCPU CI
	// runner under the race detector — if CPU dominates, extra flows
	// genuinely stop paying and the ramp correctly refuses to reach 4.
	inner := throttledRangeHandler(data, `"v1"`, &st,
		20*time.Millisecond, 64, func(*http.Request) bool { return true })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flows.enter()
		defer flows.exit()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 4, MinPartSize: 256 << 10})
	dest := filepath.Join(t.TempDir(), "file.bin")
	start := time.Now()
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)

	if peak := flows.maxConcBetween(start, start.Add(wall)); peak != 4 {
		t.Fatalf("peak concurrent ranged flows = %d, want the ramp to reach 4", peak)
	}
	// Retention: four flows must hold for a sustained fraction of the
	// transfer (expected ~55%; 25% keeps the oracle robust under load).
	// Premature retirement (demote to 2 after settling) or a stalled ramp
	// cannot produce a span this long.
	if sustained := flows.longestAt(4); sustained < wall/4 {
		t.Fatalf("longest 4-flow span %v of %v total: flows were not retained", sustained, wall)
	}
}

// TestPoolDemoteCancelsParkedWorker drives the PRODUCTION dispatch path —
// ramp decision → pool retire hook → per-worker cancel — with the victim
// parked awaiting response headers that never come. With a huge stall
// timeout, only the pool's cancellation can wake it; if the hook merely
// demotes without cancelling, Get hangs on wg.Wait until this test's
// deadline. The retire-harness siblings above prove worker cancellation
// semantics; this proves the pool actually dispatches it.
func TestPoolDemoteCancelsParkedWorker(t *testing.T) {
	t.Parallel()
	data := testData(4 << 20)
	lim := newSharedLimiter(2 << 20) // paced so ramp windows elapse
	// Connection 1 carries worker 0's useful initial stream. The SECOND dialed
	// connection is deterministically the admitted worker 1 (worker 0 is still
	// streaming its first response when the ramp spawns worker 1); its request
	// blocks before headers, forever. Later connections are served normally.
	parked := make(chan struct{}, 4)
	release := make(chan struct{})
	var mu sync.Mutex
	var connections int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(testConnIDKey{}).(int)
		if id == 2 {
			select {
			case parked <- struct{}{}:
			default:
			}
			<-release // never answer: only worker-context cancellation wakes the client
			return
		}
		start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Errorf("unranged request %q", r.Header.Get("Range"))
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		body := data[start : end+1]
		for len(body) > 0 {
			n := min(16<<10, len(body))
			lim.acquire(n)
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			body = body[n:]
		}
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		mu.Lock()
		connections++
		id := connections
		mu.Unlock()
		return context.WithValue(ctx, testConnIDKey{}, id)
	}
	srv.Start()
	t.Cleanup(func() { close(release); srv.Close() })

	d := newDL(t, &Options{Parts: 2, MinPartSize: 64 << 10, Timeout: time.Hour})

	dest := filepath.Join(t.TempDir(), "file.bin")
	done := make(chan error, 1)
	go func() {
		_, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Get did not complete: demoted parked worker was never cancelled by the pool")
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-parked:
	default:
		t.Fatal("no worker ever parked awaiting headers: the oracle did not exercise dispatch")
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ")
	}
}

// TestGenuineUnderRetirementPrecedence deterministically pins the
// retirement-vs-error precedence contract that the race tests above can
// only reach probabilistically: permanent failures (content change, write
// errors, integrity) always win against retirement; retryable transport
// noise and cancellation never do.
func TestGenuineUnderRetirementPrecedence(t *testing.T) {
	t.Parallel()
	wins := []error{
		&permanentError{errContentChanged},
		&permanentError{errors.New("pwrite: bad file descriptor")},
		fmt.Errorf("attempt 3: %w", &permanentError{errors.New("checksum mismatch")}),
	}
	for _, err := range wins {
		if !genuineUnderRetirement(err) {
			t.Errorf("permanent error must win against retirement: %v", err)
		}
	}
	absorbed := []error{
		errors.New("unexpected HTTP status: 503"),
		context.Canceled,
		fmt.Errorf("read: %w", errWorkerRetired),
		errStall,
	}
	for _, err := range absorbed {
		if genuineUnderRetirement(err) {
			t.Errorf("retryable/cancellation error must not bypass the retirement net: %v", err)
		}
	}
}
