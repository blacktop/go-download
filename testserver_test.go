package download

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	// served counts body bytes actually written (throttled handler only).
	served int64
}

func (s *stats) addServed(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served += int64(n)
}

func (s *stats) servedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
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
			st.addServed(n)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			body = body[n:]
			if isSlow {
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
