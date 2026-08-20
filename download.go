package download

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultParts       = 8
	defaultMinPartSize = int64(16 << 20)
	defaultTimeout     = 15 * time.Second
	defaultMaxRetries  = 10

	// bufSize is the read-buffer size. TLS caps records at 16 KiB; a large
	// user-space buffer amortizes the syscall and copy overhead.
	bufSize = 512 << 10

	flushEvery = time.Second
)

// Options configures a Downloader. The zero value (or nil) means defaults.
type Options struct {
	// Parts is the maximum number of parallel connections. Default 8.
	// 1 disables parallelism (but keeps resume).
	Parts int
	// MinPartSize stops dynamic splitting: a remaining range is never
	// split below 2x this size. Default 16 MiB.
	MinPartSize int64
	// Timeout is the base per-read stall timeout. A connection that fails
	// to fill a read buffer within it is aborted and retried; the timeout
	// adapts upward (to 90s) on flaky links. Default 15s.
	Timeout time.Duration
	// MaxRetries is the per-chunk retry budget. Default 10.
	MaxRetries int
	// Headers are added to every request (User-Agent, auth, ...).
	Headers http.Header
	// Jar supplies cookies to every request (session auth). Nil means no
	// cookie handling.
	Jar http.CookieJar
	// Transport overrides the internal HTTP/1.1 transport. Setting it
	// disables CDN node pinning (this is the HTTP/3 escape hatch: plug in
	// a quic-go RoundTripper here).
	Transport http.RoundTripper
	// TLSConfig is used by the internal transport. Ignored when Transport
	// is set.
	TLSConfig *tls.Config
	// Proxy selects a proxy per request for the internal transport (nil
	// means http.ProxyFromEnvironment). Ignored when Transport is set.
	// Requests that go through a proxy disable CDN node pinning.
	Proxy func(*http.Request) (*url.URL, error)
	// ExpectedSHA256 is the hex-encoded checksum to verify before the
	// final rename. Empty disables verification.
	ExpectedSHA256 string
	// ExpectedSHA1 is the hex-encoded SHA-1 checksum to verify before the
	// final rename (Apple's firmware APIs still publish SHA-1). Empty
	// disables verification. May be combined with ExpectedSHA256; the
	// file is read once.
	ExpectedSHA1 string
	// Overwrite allows replacing an existing destination file.
	Overwrite bool
	// Reporter receives progress events. Nil means silent.
	Reporter Reporter
	// Logger receives debug-level internals. Nil means discard.
	Logger *slog.Logger
}

// Result describes a completed download.
type Result struct {
	// Path is the final destination path.
	Path string
	// Size is the downloaded size in bytes.
	Size int64
	// ETag and LastModified are the server validators, when present.
	ETag         string
	LastModified string
	// ContentType is the Content-Type header of the initial probe
	// response, when present. Useful for detecting servers that answer a
	// dead link with a 200 HTML error page instead of the real file.
	ContentType string
	// Resumed reports whether a previous partial download was continued.
	Resumed bool
	// Elapsed is the wall-clock duration of this Get call.
	Elapsed time.Duration
	// SHA256 is the hex checksum, set only when ExpectedSHA256 was verified.
	SHA256 string
	// SHA1 is the hex checksum, set only when ExpectedSHA1 was verified.
	SHA1 string
}

// Downloader downloads files. It is safe for concurrent use.
type Downloader struct {
	opt  Options
	base *http.Transport // nil when opt.Transport is set
	rep  Reporter
	log  *slog.Logger

	// dial is the TCP dialer shared by the base and pinned transports;
	// tests override it to fake CDN nodes.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// resolveHook overrides DNS resolution in tests.
	resolveHook func(ctx context.Context, host string) ([]netip.Addr, error)
}

// New returns a Downloader. A nil opt selects all defaults.
func New(opt *Options) (*Downloader, error) {
	var o Options
	if opt != nil {
		o = *opt
	}
	if o.Parts == 0 {
		o.Parts = defaultParts
	}
	if o.Parts < 1 {
		return nil, fmt.Errorf("invalid Parts %d: must be >= 1", o.Parts)
	}
	if o.MinPartSize == 0 {
		o.MinPartSize = defaultMinPartSize
	}
	if o.MinPartSize < 1 {
		return nil, fmt.Errorf("invalid MinPartSize %d: must be >= 1", o.MinPartSize)
	}
	if o.Timeout == 0 {
		o.Timeout = defaultTimeout
	}
	if o.Timeout < 0 {
		return nil, fmt.Errorf("invalid Timeout %v: must be > 0", o.Timeout)
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = defaultMaxRetries
	}
	if o.MaxRetries < 0 {
		return nil, fmt.Errorf("invalid MaxRetries %d: must be >= 1", o.MaxRetries)
	}
	if o.ExpectedSHA256 != "" {
		if _, err := hex.DecodeString(o.ExpectedSHA256); err != nil || len(o.ExpectedSHA256) != 64 {
			return nil, fmt.Errorf("invalid ExpectedSHA256 %q: want 64 hex chars", o.ExpectedSHA256)
		}
		o.ExpectedSHA256 = strings.ToLower(o.ExpectedSHA256)
	}
	if o.ExpectedSHA1 != "" {
		if _, err := hex.DecodeString(o.ExpectedSHA1); err != nil || len(o.ExpectedSHA1) != 40 {
			return nil, fmt.Errorf("invalid ExpectedSHA1 %q: want 40 hex chars", o.ExpectedSHA1)
		}
		o.ExpectedSHA1 = strings.ToLower(o.ExpectedSHA1)
	}
	if o.Reporter == nil {
		o.Reporter = NopReporter{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	d := &Downloader{opt: o, rep: o.Reporter, log: o.Logger}
	d.dial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	if o.Transport == nil {
		d.base = newTransport(o, d.dialContext)
	}
	return d, nil
}

// dialContext routes through d.dial so pinned transports and tests share one
// dialer seam.
func (d *Downloader) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.dial(ctx, network, addr)
}

// newTransport builds the internal transport: HTTP/1.1 only, because HTTP/2
// would multiplex every parallel range request onto a single TCP connection
// and defeat the purpose of parallel parts.
func newTransport(
	o Options, dial func(context.Context, string, string) (net.Conn, error),
) *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	proxy := o.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           dial,
		Protocols:             &protocols,
		TLSClientConfig:       o.TLSConfig,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ReadBufferSize:        bufSize,
		MaxIdleConnsPerHost:   o.Parts + 1,
		IdleConnTimeout:       90 * time.Second,
	}
}

func (d *Downloader) roundTripper() http.RoundTripper {
	if d.opt.Transport != nil {
		return d.opt.Transport
	}
	return d.base
}

// newClient wraps a transport in an http.Client carrying the configured
// cookie jar.
func (d *Downloader) newClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Jar: d.opt.Jar}
}

func (d *Downloader) applyHeaders(req *http.Request) {
	for k, vs := range d.opt.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// Get downloads url to dest. dest may be an explicit file path, an existing
// directory, or "" (filename derived from the response). The destination
// never holds a partial file: bytes are staged in dest+".part" and renamed
// only after verification. Interrupted downloads resume automatically.
func (d *Downloader) Get(ctx context.Context, url, dest string) (*Result, error) {
	start := time.Now()
	res, err := d.get(ctx, url, dest)
	if res != nil {
		res.Elapsed = time.Since(start)
	}
	d.rep.Done(err)
	return res, err
}

func (d *Downloader) get(ctx context.Context, rawURL, dest string) (*Result, error) {
	resp, err := d.elect(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	finalURL := resp.Request.URL.String()
	etag := resp.Header.Get("ETag")
	lastMod := resp.Header.Get("Last-Modified")
	contentType := resp.Header.Get("Content-Type")

	destPath, err := resolveDest(dest, finalURL, resp.Header)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if _, err := os.Stat(destPath); err == nil && !d.opt.Overwrite {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", ErrDestExists, destPath)
	}

	var total int64 = -1
	multipart := false
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		if _, _, t, err := parseContentRange(resp.Header.Get("Content-Range")); err == nil && t > 0 {
			total = t
			multipart = true
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		total = 0 // elect only lets a 416 through for a zero-length resource
	case resp.ContentLength > 0:
		total = resp.ContentLength
	}
	// The election response served its purpose (final URL, headers,
	// status); workers issue their own ranged requests.
	resp.Body.Close()

	d.log.Debug("election", "url", finalURL, "status", resp.StatusCode,
		"total", total, "multipart", multipart, "dest", destPath)

	r := &run{
		d:           d,
		url:         finalURL,
		destPath:    destPath,
		partPath:    destPath + ".part",
		total:       total,
		etag:        etag,
		lastMod:     lastMod,
		contentType: contentType,
	}
	if multipart {
		return r.multipart(ctx)
	}
	return r.single(ctx)
}

// elect sends the probe GET (Range: bytes=0-0, one byte) that follows
// redirects and decides between multipart (206), single-stream (200), and
// empty (416 on a zero-length resource). Transient failures are retried a
// few times.
func (d *Downloader) elect(ctx context.Context, rawURL string) (*http.Response, error) {
	client := d.newClient(d.roundTripper())
	var bo backoff
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			if err := sleepCtx(ctx, bo.next()); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		d.applyHeaders(req)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			lastErr = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
			return resp, nil
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && emptyContentRange(resp.Header):
			// A range probe on a zero-length resource is unsatisfiable:
			// the file exists and is empty.
			return resp, nil
		case isRetryableStatus(resp.StatusCode):
			resp.Body.Close()
			lastErr = StatusError(resp.StatusCode)
		default:
			resp.Body.Close()
			return nil, StatusError(resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("probe %s: %w", rawURL, lastErr)
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code < 600)
}

// emptyContentRange reports whether a 416 response declares a zero-length
// resource ("Content-Range: bytes */0").
func emptyContentRange(h http.Header) bool {
	return strings.TrimSpace(h.Get("Content-Range")) == "bytes */0"
}

// parseContentRange parses "bytes start-end/total"; total may be "*" (-1).
func parseContentRange(s string) (start, end, total int64, err error) {
	rest, ok := strings.CutPrefix(s, "bytes ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", s)
	}
	rangePart, totalPart, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", s)
	}
	if totalPart == "*" {
		total = -1
	} else if total, err = strconv.ParseInt(totalPart, 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range total %q", s)
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range range %q", s)
	}
	if start, err = strconv.ParseInt(startPart, 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range range %q", s)
	}
	if end, err = strconv.ParseInt(endPart, 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range range %q", s)
	}
	return start, end, total, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// verifyAndFinalize checks the staged .part file (size, optional sha256),
// syncs it, and atomically renames it into place.
func (r *run) verifyAndFinalize(file *os.File, resumed bool) (*Result, error) {
	fi, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", r.partPath, err)
	}
	if r.total >= 0 && fi.Size() != r.total {
		return nil, &SizeError{Expected: r.total, Actual: fi.Size()}
	}
	res := &Result{
		Path:         r.destPath,
		Size:         fi.Size(),
		ETag:         r.etag,
		LastModified: r.lastMod,
		ContentType:  r.contentType,
		Resumed:      resumed,
	}
	if r.d.opt.ExpectedSHA256 != "" || r.d.opt.ExpectedSHA1 != "" {
		sum256, sum1, err := hashFile(file, r.d.opt.ExpectedSHA256 != "", r.d.opt.ExpectedSHA1 != "")
		if err != nil {
			return nil, err
		}
		if r.d.opt.ExpectedSHA256 != "" {
			if sum256 != r.d.opt.ExpectedSHA256 {
				return nil, r.discardStaged(file,
					&ChecksumError{Algo: "sha256", Expected: r.d.opt.ExpectedSHA256, Actual: sum256})
			}
			res.SHA256 = sum256
		}
		if r.d.opt.ExpectedSHA1 != "" {
			if sum1 != r.d.opt.ExpectedSHA1 {
				return nil, r.discardStaged(file,
					&ChecksumError{Algo: "sha1", Expected: r.d.opt.ExpectedSHA1, Actual: sum1})
			}
			res.SHA1 = sum1
		}
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync %s: %w", r.partPath, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", r.partPath, err)
	}
	if err := os.Rename(r.partPath, r.destPath); err != nil {
		return nil, fmt.Errorf("rename %s -> %s: %w", r.partPath, r.destPath, err)
	}
	if err := os.Remove(statePath(r.partPath)); err != nil && !os.IsNotExist(err) {
		r.d.log.Debug("removing resume sidecar failed", "err", err)
	}
	return res, nil
}

// discardStaged removes the staged .part and sidecar after failed
// verification: the bytes are proven bad, so a resume of them could never
// succeed. It returns cause for convenient chaining.
func (r *run) discardStaged(file *os.File, cause error) error {
	if err := file.Close(); err != nil {
		r.d.log.Debug("closing bad staged file failed", "err", err)
	}
	if err := os.Remove(r.partPath); err != nil && !os.IsNotExist(err) {
		r.d.log.Debug("removing bad staged file failed", "err", err)
	}
	if err := os.Remove(statePath(r.partPath)); err != nil && !os.IsNotExist(err) {
		r.d.log.Debug("removing resume sidecar failed", "err", err)
	}
	return cause
}

// hashFile reads file once and returns the requested hex digests.
func hashFile(file *os.File, want256, want1 bool) (sum256, sum1 string, err error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", fmt.Errorf("seek for hashing: %w", err)
	}
	var writers []io.Writer
	var h256, h1 hash.Hash
	if want256 {
		h256 = sha256.New()
		writers = append(writers, h256)
	}
	if want1 {
		h1 = sha1.New() // #nosec G401 -- verification against a published SHA-1
		writers = append(writers, h1)
	}
	buf := make([]byte, bufSize)
	if _, err := io.CopyBuffer(io.MultiWriter(writers...), file, buf); err != nil {
		return "", "", fmt.Errorf("hash %s: %w", file.Name(), err)
	}
	if h256 != nil {
		sum256 = hex.EncodeToString(h256.Sum(nil))
	}
	if h1 != nil {
		sum1 = hex.EncodeToString(h1.Sum(nil))
	}
	return sum256, sum1, nil
}

// run carries the state of one Get call.
type run struct {
	d           *Downloader
	url         string
	destPath    string
	partPath    string
	total       int64 // -1 when unknown
	etag        string
	lastMod     string
	contentType string // from the election probe response
}

func (r *run) name() string { return filepath.Base(r.destPath) }

// validator returns the If-Range value proving the content is unchanged
// between requests: a strong ETag, else Last-Modified, else "".
func (r *run) validator() string {
	if isStrongETag(r.etag) {
		return r.etag
	}
	return r.lastMod
}

// resumable reports whether a resume sidecar is worth writing: without any
// server validator, loadState could never prove the content unchanged and
// would reject the sidecar anyway.
func (r *run) resumable() bool {
	return r.etag != "" || r.lastMod != ""
}

// multipart downloads r.url (known size, ranges honored) with parallel
// workers, dynamic chunk splitting, and resume.
func (r *run) multipart(ctx context.Context) (*Result, error) {
	sched := newScheduler(r.d.opt.MinPartSize)
	st := loadState(statePath(r.partPath))
	resumed := st != nil && st.usable(r.partPath, r.total, r.etag, r.lastMod)

	flag := os.O_RDWR | os.O_CREATE
	file, err := os.OpenFile(r.partPath, flag, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", r.partPath, err)
	}
	defer file.Close()

	var resumedBytes int64
	if resumed {
		for _, c := range st.Chunks {
			if c.Done < c.End-c.Off {
				sched.addPending(c.Off, c.End, c.Done)
			}
		}
		resumedBytes = r.total - st.remaining()
		r.d.log.Debug("resuming", "bytes", resumedBytes, "chunks", len(st.Chunks))
	} else {
		if err := file.Truncate(r.total); err != nil {
			return nil, fmt.Errorf("preallocate %s: %w", r.partPath, err)
		}
		sched.addPending(0, r.total, 0)
	}
	st = &stateFile{
		Version: stateVersion, URL: r.url, Size: r.total,
		ETag: r.etag, LastModified: r.lastMod,
	}
	r.d.rep.Start(Info{Name: r.name(), Total: r.total, Resumed: resumedBytes})

	err = r.runWorkers(ctx, sched, file, st)
	if err != nil {
		if r.resumable() {
			// Leave .part and sidecar in place for a future resume.
			st.Chunks = sched.snapshot()
			if serr := st.save(statePath(r.partPath)); serr != nil {
				r.d.log.Debug("saving resume state failed", "err", serr)
			}
		}
		return nil, err
	}
	return r.verifyAndFinalize(file, resumed)
}

// runWorkers drives the worker pool and the periodic sidecar flusher,
// returning the first real error (or the context's cause).
func (r *run) runWorkers(
	ctx context.Context, sched *scheduler, file *os.File, st *stateFile,
) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var picker *picker
	if r.d.pinningEnabled(r.url) {
		u, err := url.Parse(r.url)
		if err == nil {
			picker = newPicker(u.Hostname(), portOf(u), r.d.log)
			if r.d.resolveHook != nil {
				picker.resolve = r.d.resolveHook
			}
		}
	}

	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel(err)
		})
	}
	for i := range r.d.opt.Parts {
		w := newWorker(i, r, sched, file, picker)
		wg.Go(func() {
			if err := w.run(runCtx); err != nil {
				fail(err)
			}
		})
	}

	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		if !r.resumable() {
			return
		}
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		lastRemaining := int64(-1)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				st.Chunks = sched.snapshot()
				rem := st.remaining()
				if rem == lastRemaining {
					// No bytes landed since the last flush: the sidecar
					// on disk still describes valid coverage, so skip
					// the rewrite.
					continue
				}
				lastRemaining = rem
				if err := st.save(statePath(r.partPath)); err != nil {
					r.d.log.Debug("flush resume state failed", "err", err)
				}
			}
		}
	}()

	wg.Wait()
	cancel(nil)
	<-flushDone
	if firstErr != nil {
		return firstErr
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if !sched.idle() {
		return errors.New("internal: workers exited with work remaining")
	}
	return nil
}

// pinningEnabled reports whether per-node connection pinning applies: it
// requires the internal transport and no proxy for the target URL.
func (d *Downloader) pinningEnabled(rawURL string) bool {
	if d.base == nil {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	if d.base.Proxy != nil {
		if proxyURL, err := d.base.Proxy(req); err != nil || proxyURL != nil {
			return false
		}
	}
	return true
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "http" {
		return "80"
	}
	return "443"
}

// single downloads r.url over one sequential stream (server ignored Range or
// size is unknown). No resume: a retry restarts from byte zero.
func (r *run) single(ctx context.Context) (*Result, error) {
	// No O_TRUNC and no eager sidecar removal: an existing multipart .part
	// stays resumable until a single-stream attempt actually starts writing
	// (singleAttempt truncates only after a successful response). A stale
	// sidecar is removed by verifyAndFinalize on success and is harmless
	// otherwise (usable() rejects it once the .part size changed).
	file, err := os.OpenFile(r.partPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", r.partPath, err)
	}
	defer file.Close()

	r.d.rep.Start(Info{Name: r.name(), Total: r.total})
	w := newWorker(0, r, nil, file, nil)
	if err := w.singleStream(ctx); err != nil {
		return nil, err
	}
	return r.verifyAndFinalize(file, false)
}
