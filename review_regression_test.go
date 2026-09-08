package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDerivedNameCannotEscapeDirectory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".", "..", "/", "a/..", "a/."} {
		t.Run(name, func(t *testing.T) {
			for _, fromHeader := range []bool{true, false} {
				dir := t.TempDir()
				h := make(http.Header)
				location := "https://example.test/" + name
				if fromHeader {
					location = "https://example.test/file.bin"
					h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
				}
				dest, err := resolveDest(dir, location, h)
				if err == nil {
					t.Errorf("name %q, header=%v resolved to %q", name, fromHeader, dest)
				}
			}
		})
	}
}

func TestDownloadRefusesSymlinkedStaging(t *testing.T) {
	t.Parallel()
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprint(multipart), func(t *testing.T) {
			data := testData(64 << 10)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if multipart {
					writeBareRange(w, r, data, `"v1"`)
					return
				}
				_, _ = w.Write(data)
			}))
			defer srv.Close()
			dir := t.TempDir()
			dest := filepath.Join(dir, "file.bin")
			target := filepath.Join(dir, "victim")
			sentinel := []byte("user data that must survive")
			if err := os.WriteFile(target, sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dest+".part"); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			d := newDL(t, &Options{Parts: 1})
			defer d.CloseIdleConnections()
			if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); err == nil {
				t.Error("accepted symlinked staging")
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, sentinel) {
				t.Errorf("symlink target changed (%d bytes): %v", len(got), err)
			}
		})
	}
}

func TestSidecarSaveDoesNotFollowPredictableTempSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	sentinel := []byte("unrelated data")
	if err := os.WriteFile(target, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file.part.json")
	if err := os.Symlink(target, path+".tmp"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validSidecar().save(path); err != nil {
		t.Fatal(err)
	}
	if loadState(path) == nil {
		t.Fatal("saved state is invalid")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Errorf("temp symlink target changed: %q, %v", got, err)
	}
}

type cancelAfterProgress struct {
	NopReporter
	cancel context.CancelFunc
}

func (r cancelAfterProgress) ChunkProgress(_ int, n int, _ time.Duration) {
	if n > 0 {
		r.cancel()
	}
}

func TestFreshChecksumRunInvalidatesOldSidecar(t *testing.T) {
	t.Parallel()
	a := bytes.Repeat([]byte{0x11}, 4*bufSize)
	b := bytes.Repeat([]byte{0x22}, len(a))
	var phase atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if phase.Load() == 0 {
			writeBareRange(w, req, b, "")
			return
		}
		writeBareRange(w, req, a, `"v1"`)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "file.bin")
	u, err := url.Parse(srv.URL + "/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	seedInterruptedDownload(t, dest, a, sourceIdentity(u))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := newDL(t, &Options{Parts: 1})
	defer d.CloseIdleConnections()
	_, err = d.Do(ctx, &Request{URL: u.String(), Dest: dest,
		ExpectedSHA256: fmt.Sprintf("%x", sha256.Sum256(b)), Reporter: cancelAfterProgress{cancel: cancel}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run: %v", err)
	}
	if _, err := os.Lstat(statePath(dest + ".part")); !os.IsNotExist(err) {
		t.Errorf("stale coverage survived: %v", err)
	}
	phase.Store(1)
	res, got := mustGet(t, d, u.String(), dest)
	if res.Resumed || !bytes.Equal(got, a) {
		t.Fatal("old sidecar installed mixed content after a checksum-only run")
	}
}

func TestHealthyProgressResetsBackoff(t *testing.T) {
	t.Parallel()
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprint(single), func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "stage")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := newDL(t, &Options{Timeout: time.Second})
			r := &run{d: d, rep: NopReporter{}}
			w := newWorker(0, r, newScheduler(1024), f)
			defer w.releaseBuf()
			for range 8 {
				w.bo.next()
			}
			if single {
				var written int64
				_, err = w.singleSink(&written, int64(2*len(w.buf)))(w.buf, time.Millisecond)
			} else {
				w.sched.addPending(0, int64(2*len(w.buf)), 0)
				c := w.sched.next(0)
				_, err = w.chunkSink(c)(w.buf, time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			if delay := w.bo.next(); delay > time.Duration(float64(backoffBase)*(1+backoffJitter)) {
				t.Errorf("healthy progress left retry delay at %v", delay)
			}
		})
	}
}

// A synthetic transport exercises Unicode URLs and http.Client's real redirect
// policy without DNS, TLS or access to an external server.
func TestIDNRedirectPreservesApprovedHeaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target string
		auth         bool
	}{
		{"equivalent", "xn--bcher-kva.example", true},
		{"subdomain", "cdn.xn--bcher-kva.example", true},
		{"unrelated", "other.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := testData(64 << 10)
			var requests atomic.Int32
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			targetURL := &url.URL{Scheme: "http", Host: tc.target, Path: "/file.bin"}
			jar.SetCookies(targetURL, []*http.Cookie{{Name: "live", Value: "old", Path: "/"}})
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/start" {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {targetURL.String()}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				n := requests.Add(1)
				if got := req.Header.Get("Authorization"); (got == "Bearer fixture") != tc.auth {
					t.Errorf("request %d: auth=%q", n, got)
				}
				explicit := false
				var lives []string
				for _, c := range req.Cookies() {
					if c.Name == "explicit" {
						explicit = true
					}
					if c.Name == "live" {
						lives = append(lives, c.Value)
					}
				}
				if explicit != tc.auth {
					t.Errorf("request %d: explicit cookie=%v", n, explicit)
				}
				want := "old"
				if n > 1 {
					want = "new"
				}
				if len(lives) != 1 || lives[0] != want {
					t.Errorf("request %d: jar cookies %v, want [%s]", n, lives, want)
				}
				rec := httptest.NewRecorder()
				if n == 1 {
					rec.Header().Set("Set-Cookie", "live=new; Path=/")
				}
				writeBareRange(rec, req, data, `"v1"`)
				resp := rec.Result()
				resp.Request = req
				return resp, nil
			})
			d := newDL(t, &Options{Transport: rt, Jar: jar, Parts: 1})
			dest := filepath.Join(t.TempDir(), "file.bin")
			source, err := url.Parse("http://bücher.example/start")
			if err != nil {
				t.Fatal(err)
			}
			seedInterruptedDownload(t, dest, data, sourceIdentity(source))
			res, err := d.Do(t.Context(), &Request{URL: source.String(), Dest: dest, Headers: http.Header{"Authorization": {"Bearer fixture"}, "Cookie": {"explicit=fixture"}}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dest)
			if err != nil || !bytes.Equal(got, data) || !res.Resumed || requests.Load() < 2 {
				t.Fatalf("resumed redirect failed: %v, %v", res, err)
			}
		})
	}
}

type traceTestConn struct {
	net.Conn
	addr string
}

func (c traceTestConn) RemoteAddr() net.Addr { return traceTestAddr(c.addr) }

type traceTestAddr string

func (a traceTestAddr) Network() string { return "tcp" }
func (a traceTestAddr) String() string  { return string(a) }

func TestElectionTraceIgnoresLatePreviousHop(t *testing.T) {
	t.Parallel()
	var originTrace *httptrace.ClientTrace
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		if req.URL.Path == "/start" {
			originTrace = trace
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"http://final.example/file"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		trace.GotConn(httptrace.GotConnInfo{Conn: traceTestConn{addr: "192.0.2.2:443"}})
		originTrace.GotConn(httptrace.GotConnInfo{Conn: traceTestConn{addr: "192.0.2.1:443"}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	d := newDL(t, &Options{Transport: rt})
	u, _ := url.Parse("http://origin.example/start")
	e, err := d.elect(t.Context(), u.String(), u, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.cancel(nil)
	defer e.resp.Body.Close()
	if e.remoteAddr != "192.0.2.2:443" {
		t.Fatalf("election attributed to old hop: %q", e.remoteAddr)
	}
}

func TestElectionTraceAsync(t *testing.T) {
	t.Parallel()
	var callbacks sync.WaitGroup
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		callbacks.Go(func() {
			for range 1000 {
				trace.GotConn(httptrace.GotConnInfo{Conn: traceTestConn{addr: "192.0.2.2:443"}})
			}
		})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	defer callbacks.Wait()
	d := newDL(t, &Options{Transport: rt})
	u, _ := url.Parse("http://example.test/file")
	for range 10 {
		e, err := d.elect(t.Context(), u.String(), u, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.cancel(nil)
		e.resp.Body.Close()
	}
}

func TestSidecarSaveCleansTempOnRenameFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validSidecar().save(path); err == nil {
		t.Fatal("expected rename failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sidecar.json" {
		t.Fatalf("temporary state leaked: %v", entries)
	}
}

func TestCompletedSmallChunkResetsBackoff(t *testing.T) {
	t.Parallel()
	data := testData(2048)
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		writeBareRange(rec, req, data, `"v1"`)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})
	d := newDL(t, &Options{Transport: rt, Parts: 1})
	r := &run{d: d, rep: NopReporter{}, url: "http://example.test/file", total: int64(len(data)), etag: `"v1"`}
	sched := newScheduler(1024)
	sched.addPending(0, int64(len(data)), 0)
	f, err := os.CreateTemp(t.TempDir(), "stage")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := newWorker(0, r, sched, f)
	defer w.releaseBuf()
	for range 8 {
		w.bo.next()
	}
	if err := w.downloadChunk(t.Context(), sched.next(0)); err != nil {
		t.Fatal(err)
	}
	if delay := w.bo.next(); delay > time.Duration(float64(backoffBase)*(1+backoffJitter)) {
		t.Errorf("completed small chunk retained %v backoff", delay)
	}
}

func TestStagingOpenDoesNotCreateDanglingSymlinkTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "absent")
	part := filepath.Join(dir, "file.part")
	if err := os.Symlink(target, part); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f, err := openStaging(part, os.O_RDWR|os.O_CREATE)
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Fatal("accepted dangling staging symlink")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("created dangling link target: %v", err)
	}
}

func TestFreshRunFailsClosedOnSidecarRemovalError(t *testing.T) {
	t.Parallel()
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprint(multipart), func(t *testing.T) {
			data := testData(64 << 10)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if multipart {
					writeBareRange(w, r, data, `"v1"`)
					return
				}
				_, _ = w.Write(data)
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "file.bin")
			sentinel := []byte("retained staging")
			if err := os.WriteFile(dest+".part", sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			side := statePath(dest + ".part")
			if err := os.Mkdir(side, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(side, "keep"), sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			d := newDL(t, &Options{Parts: 1})
			defer d.CloseIdleConnections()
			if _, err := d.Get(t.Context(), srv.URL+"/file", dest); err == nil {
				t.Fatal("wrote staging without invalidating old metadata")
			}
			got, err := os.ReadFile(dest + ".part")
			if err != nil || !bytes.Equal(got, sentinel) {
				t.Fatalf("staging changed despite sidecar removal failure: %v", err)
			}
		})
	}
}
