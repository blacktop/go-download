package download

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestMinPartsValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		opt     Options
		wantErr bool
	}{
		{name: "default", opt: Options{Parts: 8}},
		{name: "floor equals cap", opt: Options{Parts: 8, MinParts: 8}},
		{name: "floor below cap", opt: Options{Parts: 8, MinParts: 4}},
		{name: "negative", opt: Options{Parts: 8, MinParts: -1}, wantErr: true},
		{name: "above cap", opt: Options{Parts: 4, MinParts: 5}, wantErr: true},
		{name: "above default cap", opt: Options{MinParts: 9}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := New(&tc.opt)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%+v) err = %v, wantErr %t", tc.opt, err, tc.wantErr)
			}
			if err == nil && tc.opt.MinParts == 0 && d.opt.MinParts != DefaultMinParts {
				t.Fatalf("default MinParts = %d, want %d", d.opt.MinParts, DefaultMinParts)
			}
		})
	}
}

func TestPrepareSplitsPendingForEagerWorkers(t *testing.T) {
	t.Parallel()
	const minPart = int64(64 << 10)
	for _, tc := range []struct {
		name      string
		remaining int64
		minParts  int
		want      int
	}{
		{name: "floor one", remaining: 1 << 30, minParts: 1, want: 1},
		{name: "ample runway", remaining: 1 << 30, minParts: 8, want: 8},
		{name: "tiny file", remaining: 2*minPart - 1, minParts: 8, want: 1},
		{name: "one split", remaining: 2 * minPart, minParts: 8, want: 2},
		{name: "halving stops at two", remaining: 3 * minPart, minParts: 3, want: 2},
		{name: "uneven halves", remaining: 7 * minPart, minParts: 8, want: 4},
		{name: "exact power of two", remaining: 8 * minPart, minParts: 8, want: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sched := newScheduler(minPart)
			sched.addPending(0, tc.remaining, 0)
			if got := sched.prepare(tc.minParts); got != tc.want {
				t.Fatalf("prepare(%d) over %d bytes = %d, want %d", tc.minParts, tc.remaining, got, tc.want)
			}
			if got := len(sched.pending); got != tc.want {
				t.Fatalf("pending ranges = %d, want %d", got, tc.want)
			}
			if rem := sched.remainingBytes(); rem != tc.remaining {
				t.Fatalf("remaining bytes = %d after prepare, want %d (coverage must be conserved)",
					rem, tc.remaining)
			}
		})
	}
}

// TestPrepareGrantsSurviveEarlyClaims is the oracle behind eager startup: the
// byte-zero worker may claim bytes from the election body before any sibling
// calls next. Because prepare pre-splits pending under the lock, every
// eager worker is still granted a range — the count is not a prediction that
// live progress can invalidate.
func TestPrepareGrantsSurviveEarlyClaims(t *testing.T) {
	t.Parallel()
	const minPart = int64(64 << 10)
	for size := minPart; size <= 16*minPart; size += minPart / 4 {
		for _, minParts := range []int{2, 3, 5, 8} {
			sched := newScheduler(minPart)
			sched.addPending(0, size, 0)
			want := sched.prepare(minParts)
			first := sched.next(0)
			if first == nil || first.off != 0 {
				t.Fatalf("size %d: first grant %+v, want the byte-zero range", size, first)
			}
			if _, n, _ := sched.claim(first, bufSize); n == 0 && first.end > 0 {
				t.Fatalf("size %d: byte-zero owner could not claim", size)
			}
			granted := 1
			for id := 1; id < minParts; id++ {
				if sched.next(id) != nil {
					granted++
				}
			}
			if granted != want {
				t.Fatalf("size %d minParts %d: prepare = %d, scheduler granted %d after an early claim",
					size, minParts, want, granted)
			}
		}
	}
}

func TestPrepareUsesFragmentedResumeTopology(t *testing.T) {
	t.Parallel()
	const minPart = int64(64 << 10)
	sched := newScheduler(minPart)
	for i := range 3 {
		off := int64(i) * 2 * minPart
		sched.addPending(off, off+minPart, 0)
	}
	if got := sched.prepare(3); got != 3 {
		t.Fatalf("prepare = %d, want all 3 pending resume ranges", got)
	}
	for id := range 3 {
		if sched.next(id) == nil {
			t.Fatalf("worker %d was not granted a pending resume range", id)
		}
	}
}

// TestRampProbesUpFromFloorAndDemotesBackToIt: with MinParts 4 and Parts 8 the
// governor admits the 4→8 batch and, when it does not pay, retires back to
// exactly four — never below the floor.
func TestRampProbesUpFromFloorAndDemotesBackToIt(t *testing.T) {
	t.Parallel()
	h := newRampHarness(8, 1000)
	h.rs.floor = 4
	h.rs.admitted = 4

	if _, n, d := h.window(100); n != 0 || d != 0 {
		t.Fatal("burn-in window must have no side effects")
	}
	from, n, d := h.window(100)
	if from != 4 || n != 4 || d != 0 {
		t.Fatalf("first admission = (%d,%d,%d), want spawn workers 4..7", from, n, d)
	}
	if _, n, d := h.window(100); n != 0 || d != 0 {
		t.Fatal("settling window must have no side effects")
	}
	_, n, d = h.measure(100)
	if n != 0 || d != 4 {
		t.Fatalf("flat decision = (n=%d, demote=%d), want demote to the floor 4", n, d)
	}
	if !h.rs.done.Load() {
		t.Fatal("demotion must finish the ramp")
	}
}

// TestThrottleOverridesFloor walks the three explicit 429 states: an
// unproven admission rolls back to prevAdmitted, a floor (MinParts == Parts
// included) sheds to one flow, and one flow has nothing left to shed.
func TestThrottleOverridesFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                        string
		floor, admitted, prev, from int
		wantKeep                    int // 0: demote must not be called
	}{
		{name: "unproven batch rolls back", floor: 4, admitted: 8, prev: 4, from: 5, wantKeep: 4},
		{name: "default ramp rolls back to floor", floor: 1, admitted: 2, prev: 1, from: 1, wantKeep: 1},
		{name: "at floor sheds to one", floor: 4, admitted: 4, from: 3, wantKeep: 1},
		{name: "fixed parallelism sheds to one", floor: 8, admitted: 8, from: 0, wantKeep: 1},
		{name: "stale id from a retired worker", floor: 4, admitted: 4, prev: 4, from: 6},
		{name: "single flow has nothing to shed", floor: 1, admitted: 1, from: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept := 0
			rs := &rampState{parts: 8, floor: tc.floor, admitted: tc.admitted, prevAdmitted: tc.prev,
				now: time.Now, demote: func(keep int) { kept = keep }}
			rs.rejectThrottled(tc.from)
			if kept != tc.wantKeep {
				t.Fatalf("demote keep = %d, want %d", kept, tc.wantKeep)
			}
			if !rs.done.Load() {
				t.Fatal("a 429 must stop expansion")
			}
			if tc.wantKeep != 0 && rs.admitted != tc.wantKeep {
				t.Fatalf("admitted = %d after rollback, want %d", rs.admitted, tc.wantKeep)
			}
		})
	}
}

// TestThrottleStepsDownTwice: after rolling an unproven batch back, a second
// 429 from a survivor sheds the floor itself — the host gets explicit,
// monotone steps rather than a collapse on the first burst.
func TestThrottleStepsDownTwice(t *testing.T) {
	t.Parallel()
	var keeps []int
	rs := &rampState{parts: 8, floor: 4, admitted: 8, prevAdmitted: 4,
		now: time.Now, demote: func(keep int) { keeps = append(keeps, keep) }}
	rs.rejectThrottled(7) // batch member
	rs.rejectThrottled(6) // stale: already retired by the rollback
	rs.rejectThrottled(2) // survivor still throttled: shed the floor
	rs.rejectThrottled(0) // already at one
	if want := []int{4, 1}; !slices.Equal(keeps, want) {
		t.Fatalf("demote sequence = %v, want %v", keeps, want)
	}
}

// TestFixedParallelismShedsOn429 drives the production path: MinParts ==
// Parts opens four eager flows against a host that serves two at a time and
// 429s the rest. The override must retire the rejected eager flows instead of
// holding them in retry loops, and the download must still complete.
func TestFixedParallelismShedsOn429(t *testing.T) {
	t.Parallel()
	data := testData(512 << 10)
	var rejected atomic.Int32
	flows := &flowLog{}
	slots := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			rejected.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		flows.enter()
		defer flows.exit()
		start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Errorf("expected ranged request, got %q", r.Header.Get("Range"))
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		body := data[start : end+1]
		for len(body) > 0 {
			n := min(16<<10, len(body))
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			body = body[n:]
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 4, MinParts: 4, MinPartSize: 64 << 10, MaxRetries: 3,
		Timeout: 30 * time.Second})
	dest := filepath.Join(t.TempDir(), "file.bin")
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if n := rejected.Load(); n == 0 {
		t.Skip("eager flows never collided with the slots; scenario not exercised")
	} else if n > 2 {
		t.Fatalf("server rejected %d requests, want at most the two eager flows once", n)
	}
}

// TestMinPartsOpensAllFlowsImmediately: MinParts == Parts must reproduce
// fixed parallelism. Every sibling request after the election is held at a
// barrier until all three have arrived: eager workers dial before any body
// byte is needed, so the barrier opens; a ramped run cannot admit a second
// flow without throughput, so the barrier would time out.
func TestMinPartsOpensAllFlowsImmediately(t *testing.T) {
	t.Parallel()
	const minPart = 64 << 10
	data := testData(4 * minPart)
	var st stats
	inner := rangeHandler(data, `"v1"`, &st)
	var arrivals atomic.Int32
	release := make(chan struct{})
	var timedOut atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if start, _, ok := parseFullRange(r.Header.Get("Range"), int64(len(data))); ok && start > 0 {
			if arrivals.Add(1) == 3 {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				timedOut.Store(true)
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, &Options{Parts: 4, MinParts: 4, MinPartSize: minPart})
	dest := filepath.Join(t.TempDir(), "file.bin")
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if n := arrivals.Load(); n != 3 || timedOut.Load() {
		t.Fatalf("%d sibling flows arrived (barrier timed out: %t), want 3 before any body byte: "+
			"flows were ramped, not opened eagerly", n, timedOut.Load())
	}
}

// electionHarness is a fresh multipart run whose election response is still
// pending, exactly as runWorkers sees it before the first worker starts.
type electionHarness struct {
	*retireHarness
	data []byte
	st   *stats
}

func newElectionHarness(t *testing.T, size int) *electionHarness {
	t.Helper()
	data := testData(size)
	st := &stats{}
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, st))
	t.Cleanup(srv.Close)
	d := newDL(t, &Options{Parts: 2, MinPartSize: 1 << 10})
	h := newRetireHarness(t, d, srv.URL, int64(len(data)))
	elected, err := d.elect(t.Context(), srv.URL+"/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if elected.resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("election status = %d, want 206", elected.resp.StatusCode)
	}
	elected.resp.Body = &closeOnceBody{ReadCloser: elected.resp.Body}
	h.r.initial, h.r.initialAddr, h.r.initialCancel = elected.resp, elected.remoteAddr, elected.cancel
	t.Cleanup(h.r.closeInitial)
	return &electionHarness{retireHarness: h, data: data, st: st}
}

func (h *electionHarness) assertStaged(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(h.r.partPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, h.data) {
		t.Fatal("staged bytes differ from source")
	}
	if !h.sched.idle() {
		t.Fatal("scheduler left work behind")
	}
}

// TestElectionResponseFollowsByteZeroChunk: with eager MinParts startup a
// worker other than 0 can win the byte-zero chunk. It must consume the
// election response rather than leave it to an absent worker 0 and issue a
// duplicate request for the same bytes.
func TestElectionResponseFollowsByteZeroChunk(t *testing.T) {
	t.Parallel()
	h := newElectionHarness(t, 64<<10)
	if err := <-h.start(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	h.assertStaged(t)
	if ranges := h.st.rangeHeaders(); len(ranges) != 1 {
		t.Fatalf("server saw %d requests %v, want only the election", len(ranges), ranges)
	}
	if h.r.initial != nil {
		t.Fatal("election response was not consumed")
	}
}

// TestElectionResponseSurvivesEarlySplit: an eager sibling can split the
// byte-zero chunk before its owner validates the election response, whose
// Content-Range legitimately spans the whole representation. The owner must
// accept it and stop at the chunk's live end, not reject it as too long.
func TestElectionResponseSurvivesEarlySplit(t *testing.T) {
	t.Parallel()
	h := newElectionHarness(t, 64<<10)
	first := h.sched.next(0)
	second := h.sched.next(1)
	if first == nil || second == nil || first.off != 0 || second.off != int64(len(h.data))/2 {
		t.Fatalf("setup: chunks %+v / %+v, want an even split of the byte-zero chunk", first, second)
	}
	w0 := newWorker(0, h.r, h.sched, h.file)
	w1 := newWorker(1, h.r, h.sched, h.file)
	if err := w0.downloadChunk(t.Context(), first); err != nil {
		t.Fatalf("byte-zero owner rejected the election response after a split: %v", err)
	}
	if got, want := first.written.Load(), first.end-first.off; got != want {
		t.Fatalf("byte-zero chunk wrote %d of %d", got, want)
	}
	// The read must stop at the prepared end: bytes the sibling owns are
	// neither read from the election body nor credited to run progress.
	if got, want := h.r.progress.Load(), first.end-first.off; got != want {
		t.Fatalf("run progress after the byte-zero chunk = %d, want %d (election body over-read)",
			got, want)
	}
	if err := w1.downloadChunk(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	h.assertStaged(t)
	if ranges := h.st.rangeHeaders(); len(ranges) != 2 {
		t.Fatalf("server saw %d requests %v, want election + one tail request", len(ranges), ranges)
	}
}
