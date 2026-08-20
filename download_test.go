package download

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func newDL(t *testing.T, opt *Options) *Downloader {
	t.Helper()
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustGet(t *testing.T, d *Downloader, url, dest string) (*Result, []byte) {
	t.Helper()
	res, err := d.Get(t.Context(), url, dest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	return res, got
}

func assertClean(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file left behind: %v", err)
	}
	if _, err := os.Stat(dest + ".part.json"); !os.IsNotExist(err) {
		t.Errorf("sidecar left behind: %v", err)
	}
}

func TestGetMultipart(t *testing.T) {
	t.Parallel()
	data := testData(1 << 20)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 4, MinPartSize: 4 << 10})
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)

	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if res.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", res.Size, len(data))
	}
	if res.ETag != `"v1"` {
		t.Errorf("ETag = %q", res.ETag)
	}
	if res.Resumed {
		t.Error("fresh download reported Resumed")
	}
	if starts := st.rangeStarts(); len(starts) < 3 {
		t.Errorf("expected parallel range requests, saw %d: %v", len(starts), starts)
	}
	assertClean(t, dest)
}

func TestGetSingleStream(t *testing.T) {
	t.Parallel()
	data := testData(200 << 10)
	var st stats
	srv := httptest.NewServer(plainHandler(data, &st))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 4, MinPartSize: 1 << 10})
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)

	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if res.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", res.Size, len(data))
	}
	assertClean(t, dest)
}

func TestGetUnknownLength(t *testing.T) {
	t.Parallel()
	data := testData(150 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: chunked transfer, unknown size.
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, nil)
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if res.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", res.Size, len(data))
	}
}

func TestDestExists(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDL(t, nil)
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); !errors.Is(err, ErrDestExists) {
		t.Fatalf("expected ErrDestExists, got %v", err)
	}

	d = newDL(t, &Options{Overwrite: true, MinPartSize: 4 << 10})
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("overwrite download differs from source")
	}
}

func TestRedirectAndDerivedName(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	mux := http.NewServeMux()
	mux.Handle("/real/final-name.bin", rangeHandler(data, `"v1"`, &st))
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/real/final-name.bin", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	d := newDL(t, &Options{MinPartSize: 4 << 10})
	res, got := mustGet(t, d, srv.URL+"/start", dir)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if filepath.Base(res.Path) != "final-name.bin" {
		t.Errorf("derived name = %q, want final-name.bin", filepath.Base(res.Path))
	}
}

func TestStallDetectionAndRetry(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var firstReq atomic.Bool
	firstReq.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if firstReq.CompareAndSwap(true, false) {
			// Send half, then stall until the client gives up.
			w.Write(data[:len(data)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Timeout: 200 * time.Millisecond, MaxRetries: 3})
	start := time.Now()
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source (stall retry corrupted output)")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("stall detector did not abort the hung read promptly")
	}
}

func TestFlakyServerRetryResumesMidChunk(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.enter(r)
		defer st.exit()
		start, end, ok := parseFullRange(r.Header.Get("Range"), int64(len(data)))
		if !ok {
			t.Errorf("expected ranged request, got %q", r.Header.Get("Range"))
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range",
			"bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		// Serve at most 8 KiB of any range, then kill the connection.
		body := data[start : end+1]
		n := min(8<<10, len(body))
		w.Write(body[:n])
		if n < len(body) {
			panic(http.ErrAbortHandler)
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 2, MinPartSize: 8 << 10, MaxRetries: 10,
		Timeout: 2 * time.Second})
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	// Byte equality above proves retries resumed without duplicating or
	// losing data; the request count proves retries actually happened.
	if starts := st.rangeStarts(); len(starts) < 5 {
		t.Errorf("expected many retried range requests, saw %d", len(starts))
	}
}

func TestWorkStealing(t *testing.T) {
	t.Parallel()
	data := testData(256 << 10)
	var st stats
	// The connection that requests offset 0 crawls; everyone else is fast.
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		50*time.Millisecond, 1, func(r *http.Request) bool {
			s, _ := parseRangeStart(r.Header.Get("Range"))
			return s == 0
		}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10, Timeout: 30 * time.Second})
	_, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	// The fast worker must have stolen tail ranges repeatedly.
	if starts := st.rangeStarts(); len(starts) < 4 {
		t.Errorf("expected repeated tail-stealing splits, saw range starts %v", starts)
	}
}

func TestCancelSavesResumeState(t *testing.T) {
	t.Parallel()
	data := testData(512 << 10)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		20*time.Millisecond, 4, func(*http.Request) bool { return true }))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10})
	_, err := d.Get(ctx, srv.URL+"/file.bin", dest)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if _, err := os.Stat(dest + ".part"); err != nil {
		t.Errorf("expected .part staging file to remain: %v", err)
	}
	stf := loadState(dest + ".part.json")
	if stf == nil {
		t.Fatal("expected a valid resume sidecar")
	}
	if stf.remaining() == 0 || stf.remaining() == int64(len(data)) {
		t.Errorf("sidecar remaining = %d, want partial progress", stf.remaining())
	}
}

func TestResume(t *testing.T) {
	t.Parallel()
	data := testData(512 << 10)
	var st stats
	var slow atomic.Bool
	slow.Store(true)
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		20*time.Millisecond, 4, func(*http.Request) bool { return slow.Load() }))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10})
	_, err := d.Get(ctx, srv.URL+"/file.bin", dest)
	cancel()
	if err == nil {
		t.Fatal("expected cancellation error on phase 1")
	}
	before := len(st.rangeStarts())

	slow.Store(false)
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed bytes differ from source")
	}
	if !res.Resumed {
		t.Error("expected Resumed=true")
	}
	// Phase 2 must not re-fetch from byte 0 (beyond the probe request).
	phase2 := st.rangeStarts()[before:]
	fullRefetches := 0
	for i, s := range phase2 {
		if i == 0 {
			continue // election probe is always bytes=0-
		}
		if s == 0 {
			fullRefetches++
		}
	}
	if fullRefetches > 0 {
		t.Errorf("resume re-fetched from offset 0: %v", phase2)
	}
	assertClean(t, dest)
}

func TestResumeRejectedWhenContentChanges(t *testing.T) {
	t.Parallel()
	dataV1 := testData(256 << 10)
	dataV2 := append(testData(256<<10), 0xFF) // different content and size
	var st stats
	var v2 atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, etag := dataV1, `"v1"`
		if v2.Load() {
			data, etag = dataV2, `"v2"`
		}
		throttle := time.Duration(0)
		if !v2.Load() {
			throttle = 20 * time.Millisecond
		}
		throttledRangeHandler(data, etag, &st, throttle, 4,
			func(*http.Request) bool { return !v2.Load() }).ServeHTTP(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	ctx, cancel := context.WithTimeout(t.Context(), 1200*time.Millisecond)
	d := newDL(t, &Options{Parts: 2, MinPartSize: 4 << 10})
	_, err := d.Get(ctx, srv.URL+"/file.bin", dest)
	cancel()
	if err == nil {
		t.Fatal("expected cancellation error on phase 1")
	}

	v2.Store(true)
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if res.Resumed {
		t.Error("resume must be rejected when the ETag changed")
	}
	if !bytes.Equal(got, dataV2) {
		t.Fatal("fresh download after content change differs from new content")
	}
}

func TestCorruptSidecarStartsFresh(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest+".part.json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newDL(t, &Options{MinPartSize: 4 << 10})
	res, got := mustGet(t, d, srv.URL+"/file.bin", dest)
	if res.Resumed {
		t.Error("corrupt sidecar must not resume")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
}

func TestSHA256Verification(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)

	t.Run("match", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "file.bin")
		d := newDL(t, &Options{ExpectedSHA256: hexSum, MinPartSize: 4 << 10})
		res, _ := mustGet(t, d, srv.URL+"/file.bin", dest)
		if res.SHA256 != hexSum {
			t.Errorf("Result.SHA256 = %q, want %q", res.SHA256, hexSum)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "file.bin")
		bad := "0000000000000000000000000000000000000000000000000000000000000000"
		d := newDL(t, &Options{ExpectedSHA256: bad, MinPartSize: 4 << 10})
		_, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
		var ce *ChecksumError
		if !errors.As(err, &ce) {
			t.Fatalf("expected ChecksumError, got %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Error("mismatched file must not be renamed into place")
		}
	})
}

func TestSHA1Verification(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	sum1 := sha1.Sum(data)
	hexSum1 := hex.EncodeToString(sum1[:])
	sum256 := sha256.Sum256(data)
	hexSum256 := hex.EncodeToString(sum256[:])
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)

	t.Run("match", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "file.bin")
		d := newDL(t, &Options{ExpectedSHA1: hexSum1, MinPartSize: 4 << 10})
		res, _ := mustGet(t, d, srv.URL+"/file.bin", dest)
		if res.SHA1 != hexSum1 {
			t.Errorf("Result.SHA1 = %q, want %q", res.SHA1, hexSum1)
		}
	})

	t.Run("both algorithms", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "file.bin")
		d := newDL(t, &Options{ExpectedSHA1: hexSum1, ExpectedSHA256: hexSum256, MinPartSize: 4 << 10})
		res, _ := mustGet(t, d, srv.URL+"/file.bin", dest)
		if res.SHA1 != hexSum1 || res.SHA256 != hexSum256 {
			t.Errorf("Result checksums = (%q, %q), want (%q, %q)", res.SHA1, res.SHA256, hexSum1, hexSum256)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "file.bin")
		bad := "0000000000000000000000000000000000000000"
		d := newDL(t, &Options{ExpectedSHA1: bad, MinPartSize: 4 << 10})
		_, err := d.Get(t.Context(), srv.URL+"/file.bin", dest)
		var ce *ChecksumError
		if !errors.As(err, &ce) {
			t.Fatalf("expected ChecksumError, got %v", err)
		}
		if ce.Algo != "sha1" {
			t.Errorf("ChecksumError.Algo = %q, want sha1", ce.Algo)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Error("mismatched file must not be renamed into place")
		}
	})
}

func TestContentRangeParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in                string
		start, end, total int64
		ok                bool
	}{
		{in: "bytes 0-999/1000", start: 0, end: 999, total: 1000, ok: true},
		{in: "bytes 500-999/1000", start: 500, end: 999, total: 1000, ok: true},
		{in: "bytes 0-0/1", start: 0, end: 0, total: 1, ok: true},
		{in: "bytes 0-999/*", start: 0, end: 999, total: -1, ok: true},
		{in: "bytes */1000", ok: false},
		{in: "garbage", ok: false},
		{in: "", ok: false},
	}
	for _, tc := range tests {
		s, e, tot, err := parseContentRange(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("%q: err = %v, want ok=%t", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && (s != tc.start || e != tc.end || tot != tc.total) {
			t.Errorf("%q: got (%d,%d,%d), want (%d,%d,%d)", tc.in, s, e, tot, tc.start, tc.end, tc.total)
		}
	}
}

func TestOptionsValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(&Options{Parts: -1}); err == nil {
		t.Error("negative Parts must error")
	}
	if _, err := New(&Options{MinPartSize: -5}); err == nil {
		t.Error("negative MinPartSize must error")
	}
	if _, err := New(&Options{ExpectedSHA256: "xyz"}); err == nil {
		t.Error("bad sha256 must error")
	}
	if _, err := New(&Options{ExpectedSHA1: "xyz"}); err == nil {
		t.Error("bad sha1 must error")
	}
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.opt.Parts != defaultParts || d.opt.MinPartSize != defaultMinPartSize {
		t.Error("nil Options must select defaults")
	}
}
