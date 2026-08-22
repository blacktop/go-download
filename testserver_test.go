package download

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testModTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testData returns n deterministic pseudo-random bytes.
func testData(n int) []byte {
	r := rand.NewChaCha8([32]byte{1, 2, 3})
	b := make([]byte, n)
	r.Read(b)
	return b
}

// stats records what the test server observed.
type stats struct {
	mu      sync.Mutex
	ranges  []string
	conc    int
	maxConc int
}

func (s *stats) enter(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ranges = append(s.ranges, r.Header.Get("Range"))
	s.conc++
	s.maxConc = max(s.maxConc, s.conc)
}

func (s *stats) exit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conc--
}

func (s *stats) rangeHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

// rangeStarts returns the start offsets of all observed Range headers.
func (s *stats) rangeStarts() []int64 {
	var starts []int64
	for _, h := range s.rangeHeaders() {
		if start, ok := parseRangeStart(h); ok {
			starts = append(starts, start)
		}
	}
	return starts
}

func parseRangeStart(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, false
	}
	numStr, _, _ := strings.Cut(rest, "-")
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// rangeHandler serves data with full Range/If-Range support via
// http.ServeContent, recording observed requests in st.
func rangeHandler(data []byte, etag string, st *stats) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "file.bin", testModTime, bytes.NewReader(data))
	})
}

// plainHandler ignores Range entirely: always 200 with the full body.
func plainHandler(data []byte, st *stats) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	})
}

// throttledRangeHandler serves data manually — 206 for ranged requests, 200
// for plain ones — sleeping delay per written kb when slow(r) is true. Both
// paths pace identically so single-stream and ranged clients see the same
// per-connection throughput cap.
func throttledRangeHandler(data []byte, etag string, st *stats,
	delay time.Duration, kb int, slow func(r *http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		body := data
		if start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data))); ok {
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			body = data[start : end+1]
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
		}
		step := kb << 10
		isSlow := slow != nil && slow(r)
		for len(body) > 0 {
			n := min(step, len(body))
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			body = body[n:]
			// The final bytes have already left the handler. Do not keep this
			// request counted as an active flow during a trailing pacing sleep:
			// the client can legitimately start its next request immediately.
			if isSlow && len(body) > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
				}
			}
		}
	})
}

// parseFullRange parses "bytes=s-e" or "bytes=s-" against size.
func parseFullRange(h string, size int64) (start, end int64, ok bool) {
	rest, found := strings.CutPrefix(h, "bytes=")
	if !found {
		return 0, 0, false
	}
	startStr, endStr, found := strings.Cut(rest, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start >= size {
		return 0, 0, false
	}
	if endStr == "" {
		return start, size - 1, true
	}
	end, err = strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, min(end, size-1), true
}

// sharedLimiter caps the AGGREGATE byte rate across every connection that
// acquires from it — modeling a saturated access link, where adding flows
// cannot add throughput (contrast: throttledRangeHandler caps each flow).
type sharedLimiter struct {
	mu      sync.Mutex
	next    time.Time
	perByte time.Duration
	active  int
}

func newSharedLimiter(bytesPerSec int64) *sharedLimiter {
	return &sharedLimiter{perByte: time.Duration(int64(time.Second) / bytesPerSec)}
}

// enter starts a transfer epoch. Idle time between downloads must not become
// credit that the next download can spend as an unpaced burst.
func (l *sharedLimiter) enter() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == 0 {
		l.next = time.Now()
	}
	l.active++
}

func (l *sharedLimiter) exit() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
}

// acquire reserves n bytes on the shared schedule and waits for their slot.
// A late wake keeps its original reservation so extra waiters cannot turn
// scheduler delay into an apparent throughput gain.
func (l *sharedLimiter) acquire(n int) {
	l.mu.Lock()
	now := time.Now()
	if l.next.IsZero() {
		l.next = now
	}
	wait := max(l.next.Sub(now), 0)
	l.next = l.next.Add(time.Duration(n) * l.perByte)
	l.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

func TestSharedLimiterSchedule(t *testing.T) {
	lim := newSharedLimiter(1 << 20)
	lim.enter()
	late := time.Now().Add(-time.Second)
	lim.next = late
	lim.acquire(1)
	if want := late.Add(lim.perByte); lim.next != want {
		t.Fatalf("late acquire moved schedule to %v, want %v", lim.next, want)
	}
	lim.exit()

	before := time.Now()
	lim.enter()
	after := time.Now()
	defer lim.exit()
	if lim.next.Before(before) || lim.next.After(after) {
		t.Fatalf("new transfer schedule = %v, want within [%v, %v]", lim.next, before, after)
	}
}

// flowLog records ranged-body concurrency transitions with timestamps so a
// test can read the steady-state flow count near the end of a transfer.
type flowLog struct {
	mu     sync.Mutex
	conc   int
	events []flowEvent
}

type flowEvent struct {
	at   time.Time
	conc int
}

func (f *flowLog) enter() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conc++
	f.events = append(f.events, flowEvent{time.Now(), f.conc})
}

func (f *flowLog) exit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conc--
	f.events = append(f.events, flowEvent{time.Now(), f.conc})
}

// maxConcBetween returns the maximum concurrency in force during [a, b]:
// the last event at or before a seeds the running value.
func (f *flowLog) maxConcBetween(a, b time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, peak := 0, 0
	for _, e := range f.events {
		if e.at.Before(a) {
			cur = e.conc
			continue
		}
		if e.at.After(b) {
			break
		}
		peak = max(peak, cur, e.conc)
		cur = e.conc
	}
	return max(peak, cur)
}

// sharedCapHandler serves ranged (206) and plain (200) requests, pacing all
// body bytes through one shared limiter. The useful initial range is flow 1.
func sharedCapHandler(data []byte, etag string, st *stats, lim *sharedLimiter, flows *flowLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		lim.enter()
		defer lim.exit()
		body := data
		if start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data))); ok {
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			body = data[start : end+1]
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
		}
		if flows != nil {
			flows.enter()
			defer flows.exit()
		}
		for len(body) > 0 {
			n := min(16<<10, len(body))
			lim.acquire(n)
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			body = body[n:]
		}
	})
}

// longestSpan returns the longest continuous wall-time span, ignoring
// events before after, during which the in-force concurrency satisfied
// match. A span still open at the last event is closed there.
func (f *flowLog) longestSpan(match func(conc int) bool, after time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var longest time.Duration
	var since time.Time
	active := false
	for _, e := range f.events {
		if e.at.Before(after) {
			continue
		}
		switch {
		case match(e.conc) && !active:
			active, since = true, e.at
		case !match(e.conc) && active:
			active = false
			longest = max(longest, e.at.Sub(since))
		}
	}
	if active && len(f.events) > 0 {
		longest = max(longest, f.events[len(f.events)-1].at.Sub(since))
	}
	return longest
}

// firstAtLeast returns when concurrency first reached n (zero time: never).
func (f *flowLog) firstAtLeast(n int) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.conc >= n {
			return e.at
		}
	}
	return time.Time{}
}

// longestAt returns the longest continuous wall-time span during which the
// in-force concurrency was at least n.
func (f *flowLog) longestAt(n int) time.Duration {
	return f.longestSpan(func(c int) bool { return c >= n }, time.Time{})
}
