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
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// adapts upward (to 90s when the base is lower) on flaky links and never
	// drops below the configured base. Default 15s.
	Timeout time.Duration
	// MaxRetries is the per-chunk retry budget. Default 10.
	MaxRetries int
	// Headers are added to every request (User-Agent, auth, ...). On a
	// redirect, credentials follow net/http's policy: sensitive headers are
	// not copied to an unrelated host.
	Headers http.Header
	// Jar supplies cookies to every request (session auth). Nil means no
	// cookie handling.
	Jar http.CookieJar
	// Transport overrides the internal HTTP/1.1 transport. Setting it
	// disables CDN node pinning (this is the HTTP/3 escape hatch: plug in
	// a quic-go RoundTripper here). WARNING: an HTTP/2 transport defeats
	// parallel parts — h2 multiplexes every range request onto a single
	// TCP connection. For *http.Transport, force HTTP/1.1 (Protocols) and
	// consider a large ReadBufferSize, as the internal transport does.
	Transport http.RoundTripper
	// TLSConfig is used by the internal transport. Ignored when Transport
	// is set.
	TLSConfig *tls.Config
	// Proxy selects a proxy per request for the internal transport (nil
	// means http.ProxyFromEnvironment). Ignored when Transport is set.
	// Requests that go through a proxy disable CDN node pinning.
	Proxy func(*http.Request) (*url.URL, error)
	// ExpectedSHA256 is the hex-encoded checksum to verify before the
	// final install. Empty disables verification.
	ExpectedSHA256 string
	// ExpectedSHA1 is the hex-encoded SHA-1 checksum to verify before the
	// final install (Apple's firmware APIs still publish SHA-1). Empty
	// disables verification. May be combined with ExpectedSHA256; the
	// file is read once.
	ExpectedSHA1 string
	// RejectContentTypes aborts a download at the probe — before any byte
	// is staged — when the response's media type matches an entry (e.g.
	// "text/html" for CDNs that answer dead links with an HTML error page
	// and status 200). Entries are compared case-insensitively against the
	// Content-Type's media type, parameters ignored.
	RejectContentTypes []string
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

	// A Reporter has no run identifier, so configured reporter streams must
	// not interleave across concurrent Get calls on this Downloader. Nil
	// when no Reporter is configured; holds one token otherwise.
	reportSem chan struct{}

	// bufs recycles the large read/hash buffers across workers and Get
	// calls; they dominate per-download allocations otherwise.
	bufs sync.Pool

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
	var err error
	if o.ExpectedSHA256, err = normalizeChecksum(o.ExpectedSHA256, sha256HexLen, "ExpectedSHA256"); err != nil {
		return nil, err
	}
	if o.ExpectedSHA1, err = normalizeChecksum(o.ExpectedSHA1, sha1HexLen, "ExpectedSHA1"); err != nil {
		return nil, err
	}
	var reportSem chan struct{}
	if o.Reporter != nil {
		reportSem = make(chan struct{}, 1)
	} else {
		o.Reporter = NopReporter{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	d := &Downloader{opt: o, rep: o.Reporter, log: o.Logger, reportSem: reportSem}
	d.bufs.New = func() any {
		b := make([]byte, bufSize)
		return &b
	}
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

func (d *Downloader) applyHeaders(req *http.Request, source *url.URL) {
	copySensitive := shouldCopySensitiveHeaders(source, req.URL)
	for k, vs := range d.opt.Headers {
		if !copySensitive && isSensitiveRequestHeader(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// isSensitiveRequestHeader mirrors the header set protected by net/http
// while following redirects.
func isSensitiveRequestHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Www-Authenticate", "Cookie", "Cookie2",
		"Proxy-Authorization", "Proxy-Authenticate":
		return true
	default:
		return false
	}
}

// shouldCopySensitiveHeaders mirrors net/http's redirect policy: explicit
// credentials may flow to the same host or one of its subdomains, but never
// to an unrelated host. Invalid or non-hierarchical URLs fail closed.
func shouldCopySensitiveHeaders(initial, dest *url.URL) bool {
	if initial == nil || dest == nil {
		return false
	}
	parent := strings.ToLower(initial.Hostname())
	sub := strings.ToLower(dest.Hostname())
	if parent == "" || sub == "" {
		return false
	}
	if sub == parent {
		return true
	}
	if strings.ContainsAny(sub, ":%") || !strings.HasSuffix(sub, parent) {
		return false
	}
	return len(sub) > len(parent) && sub[len(sub)-len(parent)-1] == '.'
}

// Request is one download. Zero-value fields fall back to the Downloader's
// Options, so a single long-lived Downloader can serve a whole batch with
// per-file reporters and checksums.
type Request struct {
	// URL is the resource to download.
	URL string
	// Dest may be an explicit file path, an existing directory, or ""
	// (filename derived from the response).
	Dest string
	// Reporter receives this download's progress events, overriding
	// Options.Reporter. Downloads with per-request reporters may run
	// concurrently; downloads sharing the Options reporter are serialized.
	Reporter Reporter
	// ExpectedSHA256 / ExpectedSHA1 override the Options checksums for
	// this download (hex; empty falls back).
	ExpectedSHA256 string
	ExpectedSHA1   string
}

// Get downloads url to dest. dest may be an explicit file path, an existing
// directory, or "" (filename derived from the response). The destination
// never holds a partial file: bytes are staged in dest+".part" and installed
// only after verification. Interrupted downloads resume automatically.
func (d *Downloader) Get(ctx context.Context, url, dest string) (*Result, error) {
	return d.Do(ctx, &Request{URL: url, Dest: dest})
}

// Do downloads one Request. See Get for the download semantics.
func (d *Downloader) Do(ctx context.Context, req *Request) (*Result, error) {
	if req == nil {
		return nil, errors.New("nil Request")
	}
	sha256sum, err := normalizeChecksum(req.ExpectedSHA256, sha256HexLen, "ExpectedSHA256")
	if err != nil {
		return nil, err
	}
	sha1sum, err := normalizeChecksum(req.ExpectedSHA1, sha1HexLen, "ExpectedSHA1")
	if err != nil {
		return nil, err
	}
	if sha256sum == "" {
		sha256sum = d.opt.ExpectedSHA256
	}
	if sha1sum == "" {
		sha1sum = d.opt.ExpectedSHA1
	}
	rep := req.Reporter
	if rep == nil {
		rep = d.rep
		// Only downloads sharing the Options reporter must serialize.
		if d.reportSem != nil {
			select {
			case d.reportSem <- struct{}{}:
				defer func() { <-d.reportSem }()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	start := time.Now()
	res, err := d.get(ctx, &resolvedRequest{
		url: req.URL, dest: req.Dest, rep: rep,
		sha256: sha256sum, sha1: sha1sum,
	})
	if res != nil {
		res.Elapsed = time.Since(start)
	}
	rep.Done(err)
	return res, err
}

// resolvedRequest is a Request with all fallbacks applied.
type resolvedRequest struct {
	url, dest    string
	rep          Reporter
	sha256, sha1 string
}

func (d *Downloader) get(ctx context.Context, rq *resolvedRequest) (*Result, error) {
	rawURL, dest := rq.url, rq.dest
	sourceURL, err := parseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	resp, err := d.elect(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	finalURL := resp.Request.URL.String()
	etag := resp.Header.Get("ETag")
	lastMod := resp.Header.Get("Last-Modified")
	contentType := resp.Header.Get("Content-Type")

	if rejected(contentType, d.opt.RejectContentTypes) {
		resp.Body.Close()
		return nil, &ContentTypeError{ContentType: contentType}
	}

	destPath, err := resolveDest(dest, finalURL, resp.Header)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	var total int64 = -1
	multipart := false
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		start, end, t, crErr := parseContentRange(resp.Header.Get("Content-Range"))
		if crErr == nil && start == 0 && end == 0 && t > 0 {
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

	d.log.Debug("election", "url", redactURL(finalURL), "status", resp.StatusCode,
		"total", total, "multipart", multipart, "dest", destPath)

	r := &run{
		d:           d,
		rep:         rq.rep,
		sha256:      rq.sha256,
		sha1:        rq.sha1,
		url:         finalURL,
		sourceURL:   sourceURL,
		destPath:    destPath,
		partPath:    destPath + ".part",
		total:       total,
		etag:        etag,
		lastMod:     lastMod,
		contentType: contentType,
	}
	if r.total > 0 && r.validator() == "" && !r.checksumConfigured() {
		multipart = false
		// The probe's total belongs to a representation we cannot bind to
		// the following requests. Let the actual download declare its own
		// length rather than truncating it to a possibly stale size.
		r.total = -1
		d.log.Debug("probe size discarded without validator or checksum",
			"url", redactURL(finalURL))
	}

	unlock, err := acquireDestination(ctx, destPath)
	if err != nil {
		return nil, fmt.Errorf("lock destination %s: %w", destPath, err)
	}
	defer unlock()

	if !d.opt.Overwrite {
		if _, err := os.Lstat(destPath); err == nil {
			return nil, fmt.Errorf("%w: %s", ErrDestExists, destPath)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat destination %s: %w", destPath, err)
		}
	}
	if multipart {
		return r.multipart(ctx)
	}
	return r.single(ctx)
}

const (
	sha256HexLen = 64
	sha1HexLen   = 40
)

// normalizeChecksum validates a hex digest of the given length and lowers it.
func normalizeChecksum(sum string, hexLen int, field string) (string, error) {
	if sum == "" {
		return "", nil
	}
	if _, err := hex.DecodeString(sum); err != nil || len(sum) != hexLen {
		return "", fmt.Errorf("invalid %s %q: want %d hex chars", field, sum, hexLen)
	}
	return strings.ToLower(sum), nil
}

// rejected reports whether contentType's media type matches any entry.
func rejected(contentType string, reject []string) bool {
	if len(reject) == 0 || contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Malformed parameters must not smuggle the media type past the
		// check: compare everything before the first ';'.
		mediaType, _, _ = strings.Cut(contentType, ";")
		mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}
	for _, r := range reject {
		if strings.EqualFold(mediaType, strings.TrimSpace(r)) {
			return true
		}
	}
	return false
}

// CloseIdleConnections closes idle HTTP connections held by the internal
// transport (and a user-supplied Options.Transport that implements the
// method). Useful for long-lived Downloaders between batches.
func (d *Downloader) CloseIdleConnections() {
	if d.base != nil {
		d.base.CloseIdleConnections()
	}
	if tr, ok := d.opt.Transport.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
}

// Discard removes dest's staged .part file and resume sidecar so the next
// Get starts fresh. It takes the same in-process destination lock Get uses
// and refuses (ErrLocked) when another process holds the staging lock.
// Missing staging files are not an error.
func Discard(ctx context.Context, dest string) error {
	unlock, err := acquireDestination(ctx, dest)
	if err != nil {
		return fmt.Errorf("lock destination %s: %w", dest, err)
	}
	defer unlock()
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		if err := os.Remove(statePath(part)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove sidecar: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", part, err)
	}
	defer f.Close()
	if err := lockStaging(f); err != nil && !errors.Is(err, errFlockUnsupported) {
		return fmt.Errorf("%w: %s", err, part)
	}
	if !flockSupported {
		// No lock to hold through the removal, and platforms without flock
		// (notably Windows) may refuse to remove an open file: close first.
		f.Close()
	}
	if err := os.Remove(part); err != nil {
		return fmt.Errorf("remove %s: %w", part, err)
	}
	if err := os.Remove(statePath(part)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sidecar: %w", err)
	}
	return nil
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
		d.applyHeaders(req, req.URL)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			err = redactErr(err)
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
	return nil, fmt.Errorf("probe %s: %w", redactURL(rawURL), lastErr)
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

// verifyAndFinalize checks the staged .part file (size, optional checksums),
// syncs it, and atomically installs it at the destination.
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
	if r.checksumConfigured() {
		bp := r.d.bufs.Get().(*[]byte)
		sum256, sum1, err := hashFile(file,
			r.sha256 != "", r.sha1 != "", *bp)
		r.d.bufs.Put(bp)
		if err != nil {
			return nil, err
		}
		if r.sha256 != "" {
			if sum256 != r.sha256 {
				return nil, &ChecksumError{Algo: "sha256",
					Expected: r.sha256, Actual: sum256, Path: r.partPath}
			}
			res.SHA256 = sum256
		}
		if r.sha1 != "" {
			if sum1 != r.sha1 {
				return nil, &ChecksumError{Algo: "sha1",
					Expected: r.sha1, Actual: sum1, Path: r.partPath}
			}
			res.SHA1 = sum1
		}
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync %s: %w", r.partPath, err)
	}
	// Where flock exists, install while the descriptor — and with it the
	// cross-process lock — is still held: closing first would let a second
	// process grab the .part inode in the window before it becomes the
	// destination (renaming an open file is fine on those platforms; the
	// caller's deferred Close releases the lock afterwards). Platforms
	// without flock have no lock to preserve, and Windows opens files
	// without delete sharing, so rename/remove require closing first.
	if !flockSupported {
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close %s: %w", r.partPath, err)
		}
	}
	if err := r.install(); err != nil {
		return nil, err
	}
	if err := os.Remove(statePath(r.partPath)); err != nil && !os.IsNotExist(err) {
		r.d.log.Debug("removing resume sidecar failed", "err", err)
	}
	return res, nil
}

// install moves the verified .part file to the destination. With Overwrite it
// is a plain rename; otherwise Link creates the destination only if it is
// still absent. Filesystems without hard links fail safely and preserve the
// staging file because the standard library has no portable no-replace rename.
func (r *run) install() error {
	if r.d.opt.Overwrite {
		if err := os.Rename(r.partPath, r.destPath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", r.partPath, r.destPath, err)
		}
		return nil
	}
	if err := installNoReplace(r.partPath, r.destPath, os.Link); err != nil {
		return err
	}
	if err := os.Remove(r.partPath); err != nil && !os.IsNotExist(err) {
		// The leftover name is a second hard link to the installed file: a
		// later download to the same destination would truncate it in place.
		r.d.log.Warn("stale staging link left behind; remove it manually",
			"path", r.partPath, "err", err)
	}
	return nil
}

func installNoReplace(
	partPath, destPath string,
	link func(string, string) error,
) error {
	linkErr := link(partPath, destPath)
	if linkErr == nil {
		return nil
	}
	if os.IsExist(linkErr) {
		return fmt.Errorf("%w: %s", ErrDestExists, destPath)
	}
	if _, err := os.Lstat(destPath); err == nil {
		return fmt.Errorf("%w: %s", ErrDestExists, destPath)
	}
	return fmt.Errorf("install %s -> %s: %w", partPath, destPath, linkErr)
}

// hashFile reads file once and returns the requested hex digests.
func hashFile(file *os.File, want256, want1 bool, buf []byte) (sum256, sum1 string, err error) {
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
	rep         Reporter
	sha256      string
	sha1        string
	url         string
	sourceURL   *url.URL
	destPath    string
	partPath    string
	total       int64 // -1 when unknown
	etag        string
	lastMod     string
	contentType string // from the election probe response
	// progress counts body bytes read this run (drives the concurrency ramp).
	progress atomic.Int64
	// ramp is the adaptive-concurrency governor; nil when Parts is 1 or on
	// the single-stream path.
	ramp *rampState
}

func (r *run) checksumConfigured() bool {
	return r.sha256 != "" || r.sha1 != ""
}

func (r *run) name() string { return filepath.Base(r.destPath) }

func (r *run) applyHeaders(req *http.Request) {
	r.d.applyHeaders(req, r.sourceURL)
}

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
	return r.validator() != ""
}

// multipart downloads r.url (known size, ranges honored) with parallel
// workers, dynamic chunk splitting, and resume.
func (r *run) multipart(ctx context.Context) (*Result, error) {
	sched := newScheduler(r.d.opt.MinPartSize)
	sourceID := sourceIdentity(r.sourceURL)

	flag := os.O_RDWR | os.O_CREATE
	file, err := os.OpenFile(r.partPath, flag, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", r.partPath, err)
	}
	defer file.Close()
	if err := lockStaging(file); err != nil {
		if !errors.Is(err, errFlockUnsupported) {
			return nil, fmt.Errorf("%w: %s", err, r.partPath)
		}
		r.d.log.Debug("staging lock unavailable, proceeding unprotected",
			"path", r.partPath, "err", err)
	}
	st := loadState(statePath(r.partPath))
	resumed := st != nil && st.usable(file, sourceID, r.total, r.etag, r.lastMod)

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
		Version: stateVersion, SourceID: sourceID, Size: r.total,
		ETag: r.etag, LastModified: r.lastMod,
	}
	r.rep.Start(Info{Name: r.name(), Total: r.total, Resumed: resumedBytes})

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
	res, err := r.verifyAndFinalize(file, resumed)
	if err != nil && r.resumable() {
		if _, ok := errors.AsType[*ChecksumError](err); ok {
			// The bytes are complete; only the published checksum disagrees.
			// A complete sidecar lets a rerun with a corrected checksum (or
			// none) finalize without re-downloading.
			st.Chunks = sched.snapshot() // empty: everything is written
			if serr := st.save(statePath(r.partPath)); serr != nil {
				r.d.log.Debug("saving complete-state sidecar failed", "err", serr)
			}
		}
	}
	return res, err
}

// rampImprovement is the minimum aggregate-throughput gain a newly admitted
// batch of connections must show before the governor admits more.
const rampImprovement = 1.15

// rampState is the adaptive-concurrency governor: workers are admitted in
// doubling steps (1→2→4→…), each step gated on a measurement window of
// 2×MinPartSize downloaded bytes. The first extra connection is always
// probed; every later step must have improved aggregate throughput by
// rampImprovement, otherwise admission stops — the bottleneck is the path,
// not the flow count — and the download transparently behaves like a
// single-stream client. Workers trip note() as bytes land, so the ramp is
// byte-accurate at any link speed.
type rampState struct {
	done     atomic.Bool // fast path for the per-read check
	mu       sync.Mutex
	spawn    func(int)
	log      *slog.Logger
	parts    int
	window   int64
	warmed   bool // first window recorded as slow-start burn-in baseline
	admitted int
	lastRate float64
	markAt   int64
	markTime time.Time
}

func (rs *rampState) note(total int64) {
	if rs.done.Load() {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.done.Load() || total-rs.markAt < rs.window {
		return
	}
	rate := float64(total-rs.markAt) / max(time.Since(rs.markTime).Seconds(), 1e-9)
	if !rs.warmed {
		// Burn-in: the first window mostly measures TCP slow-start of the
		// initial connection. Recording it as the baseline (instead of
		// admitting on it) stops warm-up gains from being credited to
		// admissions that contributed nothing.
		rs.warmed = true
	} else if rs.admitted > 1 && rate < rs.lastRate*rampImprovement {
		rs.log.Debug("concurrency ramp stopped: extra connections not paying",
			"admitted", rs.admitted, "rate", int64(rate), "previous", int64(rs.lastRate))
		rs.done.Store(true)
		return
	} else {
		add := min(rs.admitted, rs.parts-rs.admitted)
		for i := range add {
			rs.spawn(rs.admitted + i)
		}
		rs.admitted += add
		rs.log.Debug("concurrency ramp", "admitted", rs.admitted, "rate", int64(rate))
		if rs.admitted >= rs.parts {
			rs.done.Store(true)
		}
	}
	rs.lastRate = rate
	rs.markAt = total
	rs.markTime = time.Now()
}

// runWorkers drives the worker pool and the periodic sidecar flusher,
// returning the first real error (or the context's cause).
func (r *run) runWorkers(
	ctx context.Context, sched *scheduler, file *os.File, st *stateFile,
) error {
	sched.onGrant = r.rep.ChunkStart
	if rz, ok := r.rep.(ChunkResizer); ok {
		sched.onResize = rz.ChunkResize
	}
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
	spawn := func(id int) {
		w := newWorker(id, r, sched, file, picker)
		wg.Go(func() {
			if err := w.run(runCtx); err != nil {
				fail(err)
			}
		})
	}
	if r.d.opt.Parts > 1 {
		// Window: big enough to measure meaningfully, small enough that the
		// transfer has room to reach full parallelism (~3 doubling steps in
		// the first fifth). Sized from the REMAINING work so a resumed
		// download near its end still ramps instead of serializing its
		// pending chunks.
		window := max(min(2*r.d.opt.MinPartSize, sched.remainingBytes()/16), 1)
		r.ramp = &rampState{
			spawn:    spawn,
			log:      r.d.log,
			parts:    r.d.opt.Parts,
			window:   window,
			admitted: 1,
			markTime: time.Now(),
		}
	}
	spawn(0)

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
	if err := lockStaging(file); err != nil {
		if !errors.Is(err, errFlockUnsupported) {
			return nil, fmt.Errorf("%w: %s", err, r.partPath)
		}
		r.d.log.Debug("staging lock unavailable, proceeding unprotected",
			"path", r.partPath, "err", err)
	}

	r.rep.Start(Info{Name: r.name(), Total: r.total})
	w := newWorker(0, r, nil, file, nil)
	if err := w.singleStream(ctx); err != nil {
		return nil, err
	}
	return r.verifyAndFinalize(file, false)
}
