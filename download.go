package download

import (
	"context"
	"crypto/md5" // #nosec G501 -- integrity checks against published MD5 values
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultParts is the maximum connection count selected by New when
	// Options.Parts is zero.
	DefaultParts = 8
	// DefaultMinParts is the eager connection floor selected by New when
	// Options.MinParts is zero.
	DefaultMinParts = 1
	// DefaultMinPartSize is the split threshold selected by New when
	// Options.MinPartSize is zero.
	DefaultMinPartSize int64 = 16 << 20
)

const (
	defaultTimeout    = 15 * time.Second
	defaultMaxRetries = 10

	// bufSize is the worker read-buffer size. TLS caps records at 16 KiB; a
	// large user-space buffer amortizes the syscall and copy overhead.
	bufSize = 512 << 10

	flushEvery = time.Second
)

// Options configures a Downloader. The zero value (or nil) means defaults.
type Options struct {
	// Parts is the maximum number of parallel connections. Adaptive expansion
	// starts only when the remaining work contains at least one MinPartSize per
	// configured part. Default 8. 1 disables parallelism (but keeps resume).
	Parts int
	// MinParts is the concurrency floor: that many connections open at start
	// and the adaptive governor never retires below it. 0 (the default) means
	// 1, which keeps the measured ramp; MinParts == Parts is fixed parallelism
	// for hosts known to be per-flow limited. Must not exceed Parts.
	//
	// The floor is clamped to the ranges the work can supply: the scheduler
	// halves the largest remainder and never splits below 2*MinPartSize, so
	// a 3*MinPartSize object yields two ranges, not three. An explicit 429
	// overrides the floor — an overloaded server sheds eager flows.
	MinParts int
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
	// Headers are added to every request, for example User-Agent and
	// authentication. On a redirect, credentials follow net/http's policy:
	// sensitive headers are not copied to an unrelated host.
	Headers http.Header
	// Jar supplies cookies to every request (session auth). Nil means no
	// cookie handling.
	Jar http.CookieJar
	// Transport overrides the internal HTTP/1.1 transport. It is also the
	// HTTP/3 escape hatch: plug in a quic-go RoundTripper here. WARNING: an
	// HTTP/2 transport defeats parallel parts by multiplexing every range
	// request onto one TCP connection. For *http.Transport, force HTTP/1.1
	// (Protocols) and size its connection and header buffers for the workload.
	Transport http.RoundTripper
	// TLSConfig is used by the internal transport. Ignored when Transport
	// is set.
	TLSConfig *tls.Config
	// Proxy selects a proxy per request for the internal transport (nil means
	// http.ProxyFromEnvironment). Node selection follows the routing of the
	// actual election request: a proxied election disables placement for that
	// resource; a later worker request the function routes differently is
	// simply proxied and never attributed to an origin address. Ignored when
	// Transport is set.
	Proxy func(*http.Request) (*url.URL, error)
	// Policy, when set, adjusts Parts/MinParts/MinPartSize per resource once
	// the byte-serving URL is known: after the election request (itself a
	// ranged GET that follows redirects) and before additional range
	// requests, resume selection, and placement. Zero fields keep the
	// Options values, except that an inherited MinParts is clamped to a
	// lowered Parts. An invalid result fails the download. Policy may be
	// called concurrently when the Downloader is used concurrently. It lets a
	// caller apply a host-specific policy (fixed parallelism for a
	// per-flow-limited CDN, say) without a preflight request of its own.
	Policy func(finalURL string) Concurrency
	// EnableNodeSelection opts into direct-host address placement for eligible
	// multipart runs with an owned direct transport and at least two
	// resolved/election addresses. The zero value keeps the base transport.
	EnableNodeSelection bool
	// ExpectedSHA256 is the hex-encoded checksum to verify before the
	// final install. Empty disables verification.
	ExpectedSHA256 string
	// ExpectedSHA1 is the hex-encoded SHA-1 checksum to verify before the
	// final install (Apple's firmware APIs still publish SHA-1). Empty
	// disables verification. May be combined with ExpectedSHA256; the
	// file is read once.
	ExpectedSHA1 string
	// ExpectedMD5 is the hex-encoded MD5 checksum to verify before the final
	// install. It supports APIs that publish MD5 as an integrity value; it is
	// not a collision-resistant trust root. Empty disables verification. May
	// be combined with the SHA checksums; the file is read once.
	ExpectedMD5 string
	// RejectContentTypes aborts a download at the initial response — before any byte
	// is staged — when the response's media type matches an entry (e.g.
	// "text/html" for CDNs that answer dead links with an HTML error page
	// and status 200). Entries are compared case-insensitively against the
	// Content-Type's media type, parameters ignored.
	RejectContentTypes []string
	// Overwrite allows replacing an existing destination file.
	Overwrite bool
	// ResumeID overrides the URL-derived resume identity recorded in the
	// sidecar. By default the identity hashes the request URL, query
	// included, so a retry with a different query is a different resource.
	// Set ResumeID (for example to the scheme, host, and path) when request
	// URLs carry rotating signed credentials but name the same object, so an
	// interrupted download resumes under a refreshed URL. Server validators
	// (ETag/Last-Modified) and size still decide whether the staged bytes
	// are current. Empty keeps the URL-derived identity.
	ResumeID string
	// Reporter receives progress events. Nil means silent.
	Reporter Reporter
	// Logger receives debug-level internals. Nil means discard.
	Logger *slog.Logger
}

// Concurrency is a connection policy: Parts caps parallel connections,
// MinParts is the floor opened eagerly and never retired by throughput
// measurement, and MinPartSize bounds range splitting. Zero fields mean
// "unchanged" wherever a Concurrency is applied over another.
type Concurrency struct {
	Parts       int
	MinParts    int
	MinPartSize int64
}

// over fills c's zero fields from base and validates the result. If c lowers
// Parts below an inherited MinParts, the floor is clamped to the new cap; an
// explicitly set MinParts above Parts is an error.
func (c Concurrency) over(base Concurrency) (Concurrency, error) {
	if c.Parts == 0 {
		c.Parts = base.Parts
	}
	if c.Parts < 1 {
		return c, fmt.Errorf("invalid Parts %d: must be >= 1", c.Parts)
	}
	if c.MinParts == 0 {
		c.MinParts = min(base.MinParts, c.Parts)
	}
	if c.MinParts < 1 || c.MinParts > c.Parts {
		return c, fmt.Errorf("invalid MinParts %d: must satisfy 1 <= MinParts <= Parts (%d)",
			c.MinParts, c.Parts)
	}
	if c.MinPartSize == 0 {
		c.MinPartSize = base.MinPartSize
	}
	if c.MinPartSize < 1 {
		return c, fmt.Errorf("invalid MinPartSize %d: must be >= 1", c.MinPartSize)
	}
	return c, nil
}

// Result describes a completed download.
type Result struct {
	// Path is the final destination path.
	Path string
	// FinalURL is the byte-serving URL after redirects.
	FinalURL string
	// Size is the downloaded size in bytes.
	Size int64
	// ETag and LastModified are the server validators, when present.
	ETag         string
	LastModified string
	// ContentType is the Content-Type header of the initial
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
	// MD5 is the hex checksum, set only when ExpectedMD5 was verified.
	MD5 string
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

	// dial is the internal transport's TCP dialer; tests override it.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// resolve is the final-host resolver; tests replace it with deterministic
	// address sets. Resolver failures disable placement for that run.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	// conc is the validated base concurrency from Options; Policy overlays it
	// per resource.
	conc Concurrency
	// placements tracks in-flight node placements so CloseIdleConnections can
	// reach their pinned transports; each run registers and unregisters its own.
	placementsMu sync.Mutex
	placements   map[*nodePlacement]struct{}
	// sleepHook replaces every worker's retry/backoff sleeper in tests
	// (channel-coordinated fakes instead of wall-clock assertions).
	sleepHook func(ctx context.Context, d time.Duration) error
}

// New returns a Downloader. A nil opt selects all defaults.
func New(opt *Options) (*Downloader, error) {
	var o Options
	if opt != nil {
		o = *opt
	}
	defaults := Concurrency{
		Parts: DefaultParts, MinParts: DefaultMinParts, MinPartSize: DefaultMinPartSize,
	}
	requested := Concurrency{Parts: o.Parts, MinParts: o.MinParts, MinPartSize: o.MinPartSize}
	baseConc, err := requested.over(defaults)
	if err != nil {
		return nil, err
	}
	o.Parts, o.MinParts, o.MinPartSize = baseConc.Parts, baseConc.MinParts, baseConc.MinPartSize
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
	if o.ExpectedSHA256, err = normalizeChecksum(o.ExpectedSHA256, sha256HexLen, "ExpectedSHA256"); err != nil {
		return nil, err
	}
	if o.ExpectedSHA1, err = normalizeChecksum(o.ExpectedSHA1, sha1HexLen, "ExpectedSHA1"); err != nil {
		return nil, err
	}
	if o.ExpectedMD5, err = normalizeChecksum(o.ExpectedMD5, md5HexLen, "ExpectedMD5"); err != nil {
		return nil, err
	}
	// The Downloader owns its option-level header map and value slices. A
	// caller may reuse or mutate the Options after New returns without racing
	// future requests.
	o.Headers = canonicalHeaderClone(o.Headers)
	var reportSem chan struct{}
	if o.Reporter != nil {
		reportSem = make(chan struct{}, 1)
	} else {
		o.Reporter = NopReporter{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	d := &Downloader{
		opt: o, conc: baseConc, rep: o.Reporter, log: o.Logger, reportSem: reportSem,
		placements: make(map[*nodePlacement]struct{}),
	}
	d.bufs.New = func() any {
		b := make([]byte, bufSize)
		return &b
	}
	d.dial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	d.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	if o.Transport == nil {
		d.base = newTransport(o, d.dialContext)
	}
	return d, nil
}

// dialContext routes internal transport dials through the test seam.
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
	// Record every routing decision into the request's proxyRoute, when one
	// is attached (elect attaches one per attempt). The transport stays
	// ignorant of what the answer is for; consumers gate themselves.
	selectProxy := proxy
	proxy = func(req *http.Request) (*url.URL, error) {
		proxyURL, err := selectProxy(req)
		if route, ok := req.Context().Value(proxyRouteKey{}).(*proxyRoute); ok {
			route.proxied = err != nil || proxyURL != nil
		}
		return proxyURL, err
	}
	maxIdleConnsPerHost := o.Parts + 1
	if o.Policy != nil {
		// The per-resource Parts is unknown here, so allow a generous pool
		// rather than churn connections when a policy raises it.
		maxIdleConnsPerHost = max(maxIdleConnsPerHost, 64)
	}
	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           dial,
		Protocols:             &protocols,
		TLSClientConfig:       o.TLSConfig,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
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

func applyHeaders(req *http.Request, headers http.Header, source *url.URL) {
	copySensitive := shouldCopySensitiveHeaders(source, req.URL)
	for k, vs := range headers {
		if !copySensitive && isSensitiveRequestHeader(k) {
			continue
		}
		// Replace rather than append. The resolved set already merged option
		// and request values, and retries must not accumulate duplicates.
		req.Header[k] = slices.Clone(vs)
	}
}

// canonicalHeaderClone owns every value slice and folds equivalent header
// names into one canonical key. Duplicate spellings within one input retain
// their values; mergeHeaders decides precedence between layers.
func canonicalHeaderClone(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	out := make(http.Header, len(src))
	keys := slices.Sorted(maps.Keys(src))
	for _, key := range keys {
		canonical := http.CanonicalHeaderKey(key)
		if canonical == "" {
			canonical = key
		}
		out[canonical] = append(out[canonical], src[key]...)
	}
	return out
}

// mergeHeaders resolves immutable request headers. Per-request values replace
// the complete option-level slice for the same canonical header name.
func mergeHeaders(options, request http.Header) http.Header {
	merged := canonicalHeaderClone(options)
	if merged == nil && len(request) > 0 {
		merged = make(http.Header, len(request))
	}
	for key, values := range canonicalHeaderClone(request) {
		merged[key] = slices.Clone(values)
	}
	return merged
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
	// ExpectedSHA256 / ExpectedSHA1 / ExpectedMD5 override the Options checksums for
	// this download (hex; empty falls back).
	ExpectedSHA256 string
	ExpectedSHA1   string
	ExpectedMD5    string
	// Headers are merged over Options.Headers by canonical name. A request
	// value replaces the complete option-level value slice. The map and value
	// slices are cloned when Do begins, so later caller mutation cannot change
	// this run.
	Headers http.Header
	// ResumeID overrides Options.ResumeID for this download. Empty inherits the
	// option-level identity. Validators and expected size remain authoritative
	// before staged bytes are reused.
	ResumeID string
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
	// Snapshot scalar request state before any reporter serialization can block.
	// Headers are deeply cloned below.
	rawURL, dest := req.URL, req.Dest
	sha256sum, err := normalizeChecksum(req.ExpectedSHA256, sha256HexLen, "ExpectedSHA256")
	if err != nil {
		return nil, err
	}
	sha1sum, err := normalizeChecksum(req.ExpectedSHA1, sha1HexLen, "ExpectedSHA1")
	if err != nil {
		return nil, err
	}
	md5sum, err := normalizeChecksum(req.ExpectedMD5, md5HexLen, "ExpectedMD5")
	if err != nil {
		return nil, err
	}
	if sha256sum == "" {
		sha256sum = d.opt.ExpectedSHA256
	}
	if sha1sum == "" {
		sha1sum = d.opt.ExpectedSHA1
	}
	if md5sum == "" {
		md5sum = d.opt.ExpectedMD5
	}
	resumeID := d.opt.ResumeID
	if req.ResumeID != "" {
		resumeID = req.ResumeID
	}
	headers := mergeHeaders(d.opt.Headers, req.Headers)
	measurement := newRunMeasurement(ctx, d.log)
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
		url: rawURL, dest: dest, rep: rep,
		sha256: sha256sum, sha1: sha1sum, md5: md5sum,
		headers: headers, resumeID: resumeID,
		measurement: measurement,
	})
	elapsed := time.Since(start)
	if res != nil {
		res.Elapsed = elapsed
	}
	measurement.log(ctx, d.log, elapsed, res, err)
	rep.Done(err)
	return res, err
}

// resolvedRequest is a Request with all fallbacks applied.
type resolvedRequest struct {
	url, dest    string
	rep          Reporter
	sha256, sha1 string
	md5          string
	headers      http.Header
	resumeID     string
	measurement  *runMeasurement
}

type proxyRouteKey struct{}

// proxyRoute records the internal transport's routing decision for one
// election attempt. Proxy resolution runs synchronously inside the attempt's
// client.Do, so a plain bool suffices.
type proxyRoute struct {
	proxied bool
}

// concurrencyFor resolves the run's concurrency: Options values, adjusted by
// Options.Policy for the byte-serving URL when one is configured.
func (d *Downloader) concurrencyFor(finalURL string) (Concurrency, error) {
	if d.opt.Policy == nil {
		return d.conc, nil
	}
	conc, err := d.opt.Policy(finalURL).over(d.conc)
	if err != nil {
		return conc, fmt.Errorf("policy for %s: %w", redactURL(finalURL), err)
	}
	return conc, nil
}

func (d *Downloader) get(ctx context.Context, rq *resolvedRequest) (*Result, error) {
	rawURL, dest := rq.url, rq.dest
	sourceURL, err := parseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	electStart := time.Now()
	elected, err := d.elect(ctx, rawURL, sourceURL, rq.headers, rq.measurement)
	if err != nil {
		return nil, err
	}
	electDur := time.Since(electStart)
	resp, remoteAddr, electCancel := elected.resp, elected.remoteAddr, elected.cancel
	// fail abandons the election response on an early exit; every return
	// between here and the run taking ownership must go through it.
	fail := func(err error) (*Result, error) {
		electCancel(nil)
		resp.Body.Close()
		return nil, err
	}
	finalURL := resp.Request.URL.String()
	conc, err := d.concurrencyFor(finalURL)
	if err != nil {
		return fail(err)
	}
	rq.measurement.configure(resp.Request.URL, resp.Proto, conc,
		rq.sha256 != "" || rq.sha1 != "" || rq.md5 != "")
	etag := resp.Header.Get("ETag")
	lastMod := resp.Header.Get("Last-Modified")
	contentType := resp.Header.Get("Content-Type")

	if rejected(contentType, d.opt.RejectContentTypes) {
		return fail(&ContentTypeError{ContentType: contentType})
	}

	destPath, err := resolveDest(dest, finalURL, resp.Header)
	if err != nil {
		return fail(err)
	}
	var total int64 = -1
	multipart := false
	initialUsable := false
	fullInitialRange := false
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		start, end, t, crErr := parseContentRange(resp.Header.Get("Content-Range"))
		if crErr == nil && start == 0 && end >= 0 && t > 0 && end < t {
			total = t
			multipart = true
			initialUsable = true
			fullInitialRange = end == t-1
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		total = 0 // elect only lets a 416 through for a zero-length resource
		initialUsable = true
	case resp.StatusCode == http.StatusOK:
		initialUsable = true
		if resp.ContentLength >= 0 {
			total = resp.ContentLength
		}
	}

	d.log.Debug("election", "url", redactURL(finalURL), "status", resp.StatusCode,
		"total", total, "multipart", multipart, "dest", destPath)

	r := &run{
		d:               d,
		rep:             rq.rep,
		sha256:          rq.sha256,
		sha1:            rq.sha1,
		md5:             rq.md5,
		headers:         rq.headers,
		resumeID:        rq.resumeID,
		measurement:     rq.measurement,
		url:             finalURL,
		conc:            conc,
		electionProxied: elected.proxied,
		sourceURL:       sourceURL,
		destPath:        destPath,
		partPath:        destPath + ".part",
		total:           total,
		etag:            etag,
		lastMod:         lastMod,
		contentType:     contentType,
		electDur:        electDur,
	}
	if initialUsable {
		resp.Body = &closeOnceBody{ReadCloser: resp.Body}
		r.initial = resp
		r.initialAddr = remoteAddr
		r.electionAddr = remoteAddr
		r.initialCancel = electCancel
	} else {
		electCancel(nil)
		resp.Body.Close()
	}
	defer r.closeInitial()
	if r.total > 0 && r.validator() == "" && !r.checksumConfigured() {
		multipart = false
		if resp.StatusCode == http.StatusPartialContent && !fullInitialRange {
			// A capped initial range cannot be continued safely without a
			// validator or checksum binding later requests to this response.
			// Fall back to a fresh full GET and let it declare its own length.
			r.closeInitial()
			r.total = -1
			d.log.Debug("capped initial range discarded without validator or checksum",
				"url", redactURL(finalURL))
		}
	}

	unlock, contended, err := tryAcquireDestination(ctx, destPath)
	if err != nil {
		return nil, fmt.Errorf("lock destination %s: %w", destPath, err)
	}
	if contended {
		// Waiting behind another download to the same destination: do not
		// park an open, unread initial stream (and its server connection)
		// for the winner's whole run. The loser re-requests after the wait.
		r.closeInitial()
		unlock, err = acquireDestination(ctx, destPath)
		if err != nil {
			return nil, fmt.Errorf("lock destination %s: %w", destPath, err)
		}
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
	sha256HexLen = sha256.Size * 2
	sha1HexLen   = sha1.Size * 2
	md5HexLen    = md5.Size * 2
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
	d.placementsMu.Lock()
	placements := slices.Collect(maps.Keys(d.placements))
	d.placementsMu.Unlock()
	for _, p := range placements {
		p.closeIdleConnections()
	}
}

func (d *Downloader) registerPlacement(p *nodePlacement) {
	d.placementsMu.Lock()
	d.placements[p] = struct{}{}
	d.placementsMu.Unlock()
}

func (d *Downloader) unregisterPlacement(p *nodePlacement) {
	d.placementsMu.Lock()
	delete(d.placements, p)
	d.placementsMu.Unlock()
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

// elect sends a useful initial GET (Range: bytes=0-) that follows redirects
// and decides between multipart (206), single-stream (200), and empty (416 on
// a zero-length resource). A successful 200/206 body is transferred directly
// to worker 0 instead of paying for a second request. Transient failures are
// retried a few times.
//
// The returned cancel aborts the initial request itself. It is the only
// reliable way to unblock a stalled body read: an io.ReadCloser carries no
// contract that Close is safe (or effective) concurrently with Read, and
// Options.Transport bodies are arbitrary. Callers must invoke it exactly
// once the response is finished with (any cause; nil for ordinary cleanup).
// election is a successful elect result: the initial response, where its
// final hop connected, how it was routed, and the cancel that aborts it.
type election struct {
	resp       *http.Response
	remoteAddr string
	proxied    bool
	cancel     context.CancelCauseFunc
}

const electionAttempts = 3

func (d *Downloader) elect(
	ctx context.Context, rawURL string, sourceURL *url.URL, headers http.Header,
	measurement *runMeasurement,
) (election, error) {
	client := d.newClient(d.roundTripper())
	var bo backoff
	var lastErr error
	for attempt := range electionAttempts {
		if attempt > 0 {
			if err := sleepCtx(ctx, bo.next()); err != nil {
				return election{}, err
			}
		}
		ectx, ecancel := context.WithCancelCause(ctx)
		route := &proxyRoute{}
		ectx = context.WithValue(ectx, proxyRouteKey{}, route)
		req, err := http.NewRequestWithContext(ectx, http.MethodGet, rawURL, nil)
		if err != nil {
			ecancel(nil)
			return election{}, fmt.Errorf("build request: %w", err)
		}
		applyHeaders(req, headers, sourceURL)
		req.Header.Set("Range", "bytes=0-")
		var remoteAddr string
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(ci httptrace.GotConnInfo) {
				remoteAddr = ci.Conn.RemoteAddr().String()
			},
		}))
		resp, err := client.Do(req)
		if err != nil {
			ecancel(nil)
			err = redactErr(err)
			if ctx.Err() != nil {
				return election{}, err
			}
			lastErr = err
			if attempt+1 < electionAttempts {
				measurement.retry()
			}
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
			return election{resp: resp, remoteAddr: remoteAddr, proxied: route.proxied, cancel: ecancel}, nil
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && emptyContentRange(resp.Header):
			// A range request on a zero-length resource is unsatisfiable:
			// the file exists and is empty.
			return election{resp: resp, remoteAddr: remoteAddr, proxied: route.proxied, cancel: ecancel}, nil
		case isRetryableStatus(resp.StatusCode):
			resp.Body.Close()
			ecancel(nil)
			lastErr = StatusError(resp.StatusCode)
			if attempt+1 < electionAttempts {
				measurement.retry()
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				measurement.throttle()
			}
		default:
			resp.Body.Close()
			ecancel(nil)
			return election{}, StatusError(resp.StatusCode)
		}
	}
	return election{}, fmt.Errorf("initial request %s: %w", redactURL(rawURL), lastErr)
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
		FinalURL:     r.url,
		Size:         fi.Size(),
		ETag:         r.etag,
		LastModified: r.lastMod,
		ContentType:  r.contentType,
		Resumed:      resumed,
	}
	if r.checksumConfigured() {
		bp := r.d.bufs.Get().(*[]byte)
		sum256, sum1, sumMD5, err := hashFile(file,
			r.sha256 != "", r.sha1 != "", r.md5 != "", *bp)
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
		if r.md5 != "" {
			if sumMD5 != r.md5 {
				return nil, &ChecksumError{Algo: "md5",
					Expected: r.md5, Actual: sumMD5, Path: r.partPath}
			}
			res.MD5 = sumMD5
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
func hashFile(
	file *os.File, want256, want1, wantMD5 bool, buf []byte,
) (sum256, sum1, sumMD5 string, err error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", "", fmt.Errorf("seek for hashing: %w", err)
	}
	var writers []io.Writer
	var h256, h1, hMD5 hash.Hash
	if want256 {
		h256 = sha256.New()
		writers = append(writers, h256)
	}
	if want1 {
		h1 = sha1.New() // #nosec G401 -- verification against a published SHA-1
		writers = append(writers, h1)
	}
	if wantMD5 {
		hMD5 = md5.New() // #nosec G401 -- verification against a published MD5
		writers = append(writers, hMD5)
	}
	if _, err := io.CopyBuffer(io.MultiWriter(writers...), file, buf); err != nil {
		return "", "", "", fmt.Errorf("hash %s: %w", file.Name(), err)
	}
	if h256 != nil {
		sum256 = hex.EncodeToString(h256.Sum(nil))
	}
	if h1 != nil {
		sum1 = hex.EncodeToString(h1.Sum(nil))
	}
	if hMD5 != nil {
		sumMD5 = hex.EncodeToString(hMD5.Sum(nil))
	}
	return sum256, sum1, sumMD5, nil
}

// run carries the state of one Get call.
type run struct {
	d   *Downloader
	rep Reporter
	// conc is this resource's effective concurrency: Options adjusted by
	// Options.Policy for the final URL.
	conc        Concurrency
	sha256      string
	sha1        string
	md5         string
	headers     http.Header
	resumeID    string
	url         string
	sourceURL   *url.URL
	destPath    string
	partPath    string
	total       int64 // -1 when unknown
	etag        string
	lastMod     string
	contentType string // from the initial response
	// electDur is the election round-trip wall time: the best available
	// proxy for what a fresh connection on this path costs (DNS, dial, TLS,
	// TTFB). It scales the ramp's settling floor.
	electDur time.Duration
	// initial is the successful election response. The worker granted the
	// byte-zero chunk consumes it directly, eliminating the old
	// probe-plus-worker request pair. A resumed multipart run closes it
	// before issuing requests for its missing ranges. initialCancel aborts
	// the initial request itself — the only reliable way to unblock a stalled
	// body read on an arbitrary transport; it travels with the response and
	// is invoked exactly once by whoever disposes of it. initialMu makes the
	// hand-off atomic across eagerly started workers.
	initialMu   sync.Mutex
	initial     *http.Response
	initialAddr string
	// electionProxied: the election request went through a proxy.
	electionProxied bool
	// electionAddr survives closeInitial so placement can always union the
	// actual election connection into the final host's later DNS answer.
	electionAddr  string
	initialCancel context.CancelCauseFunc
	// progress counts body bytes read this run (drives the concurrency ramp).
	progress atomic.Int64
	// ramp is the adaptive-concurrency governor; nil on the single-stream
	// path. Its throughput ramp may be finished from the start, but its
	// throttle control stays live for the run.
	ramp        *rampState
	placement   *nodePlacement
	measurement *runMeasurement
}

// closeOnceBody lets the worker timeout close an initial response to unblock a
// read while normal ownership cleanup still calls Close. The underlying body
// sees exactly one close, including for custom RoundTrippers.
type closeOnceBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *closeOnceBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}

func (r *run) checksumConfigured() bool {
	return r.sha256 != "" || r.sha1 != "" || r.md5 != ""
}

// takeInitial hands the pending initial response (with its request cancel,
// never nil) to exactly one caller; later callers get a nil response.
func (r *run) takeInitial() (*http.Response, string, context.CancelCauseFunc) {
	r.initialMu.Lock()
	defer r.initialMu.Unlock()
	resp, addr, cancel := r.initial, r.initialAddr, r.initialCancel
	r.initial, r.initialAddr, r.initialCancel = nil, "", nil
	if cancel == nil {
		cancel = func(error) {}
	}
	return resp, addr, cancel
}

func (r *run) closeInitial() {
	resp, _, cancel := r.takeInitial()
	cancel(nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// placementInput projects the run's placement-relevant facts; canMultiply is
// the one fact the caller computes.
func (r *run) placementInput(canMultiply bool) placementInput {
	return placementInput{
		url: r.url, electionRemote: r.electionAddr, electionProxied: r.electionProxied,
		electionInUse: r.hasInitial(), canMultiply: canMultiply, parts: r.conc.Parts,
	}
}

func (r *run) hasInitial() bool {
	r.initialMu.Lock()
	defer r.initialMu.Unlock()
	return r.initial != nil
}

func (r *run) name() string { return filepath.Base(r.destPath) }

func (r *run) applyHeaders(req *http.Request) {
	applyHeaders(req, r.headers, r.sourceURL)
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
	sched := newScheduler(r.conc.MinPartSize)
	sourceID := resumeIdentity(r.resumeID, r.sourceURL)

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
	if resumed {
		// The useful initial response starts at byte zero, while a resume must
		// request only its missing ranges. Closing it preserves the sidecar's
		// exact resume semantics without staging duplicate bytes.
		r.closeInitial()
	}

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
// batch of connections must show before the governor admits more; below
// rampDemote the batch is judged not to be paying at all and is retired.
const (
	rampImprovement = 1.15
	rampDemote      = 1.02
	// Two equal-byte decision windows damp short limiter/scheduler timing noise
	// without delaying the first probe batch or adding a public tuning surface.
	rampMeasureWindows = 2
	// A just-admitted batch must get wall time to dial before it is judged:
	// byte windows alone can elapse before a spawned worker has even
	// connected (instant on fast links), which would demote a flow that
	// never got to contribute. The needed time is the path's connect cost,
	// so each run scales its settle floor from the measured election
	// round-trip, clamped to [rampSettleFloor, rampSettleCap]. The floor
	// only guards a degenerate near-zero measurement — settling always
	// spans at least one full byte window on top of it.
	rampSettleFloor = time.Millisecond
	rampSettleCap   = 200 * time.Millisecond
)

// rampEligible reports whether the remaining work can supply at least one
// minimum-sized chunk to every configured connection. Below that point the
// scheduler cannot make meaningful use of the full cap, while merely testing
// another connection can dominate the remaining transfer. Using remaining
// work also keeps a nearly complete resume on its proven flow.
func rampEligible(remaining, minPartSize int64, parts int) bool {
	return parts > 1 && minPartSize > 0 && remaining/minPartSize >= int64(parts)
}

// settleFloorFor derives a run's settling wall floor from its election
// round-trip: twice the observed cost of a fresh request on this path,
// clamped so a degenerate measurement can neither erase the floor nor
// stretch it past the fixed cap.
func settleFloorFor(electDur time.Duration) time.Duration {
	return min(max(2*electDur, rampSettleFloor), rampSettleCap)
}

// rampState is the adaptive-concurrency governor: workers are admitted in
// doubling steps (1→2→4→…). The first window is slow-start burn-in; each
// admission is preceded by a baseline window and followed by one full
// settling window (dial/TLS startup must not be read as "not paying"), then
// a stabilized decision measurement judged against that baseline:
//
//	rate <  1.02×baseline → demote back to prevAdmitted, done
//	rate <  1.15×baseline → freeze and keep the admitted flows, done
//	rate >= 1.15×baseline → admit the next clamped batch
//
// Reaching Parts stops spawning but the final batch is still evaluated — a
// flat final batch demotes like any other. Workers trip note() as bytes
// land, so the ramp is byte-accurate at any link speed.
type rampState struct {
	done atomic.Bool // fast path for the per-read check
	mu   sync.Mutex
	// spawn and demote are side effects executed by note AFTER rs.mu is
	// released; rs.mu never nests with the scheduler or controller locks.
	spawn  func(int)
	demote func(keep int)
	now    func() time.Time // injected in tests
	parts  int
	// floor is the flow count the run started with (Options.MinParts,
	// clamped); the governor probes upward from it and never retires below it.
	floor    int
	window   int64
	warmed   bool // burn-in window consumed
	settling bool // one no-decision window after each admission
	// settleMin is the wall-time floor settling must span, derived from the
	// election round-trip at run start (see rampSettleFloor/rampSettleCap).
	settleMin time.Duration
	admitted  int
	// admittedAt is when the most recent batch was admitted; settling does
	// not complete before settleMin has elapsed since then.
	admittedAt time.Time
	// unreadyWindows counts full windows after settleMin while at least one
	// created worker has yet to contribute. The batch gets the ordinary
	// settling window plus the stabilized measurement budget before rejection.
	unreadyWindows int
	// prevAdmitted is the flow count before the most recent batch, recorded
	// at admission time (batches are clamped: Parts=6 admits 1→2→4→6).
	prevAdmitted int
	// admissionBaseline is the steady sample preceding the most recent
	// admission; settling windows never overwrite it.
	admissionBaseline rampSample
	sampleBytes       int64
	sampleTime        time.Duration
	sampleCount       int
	markAt            int64
	markTime          time.Time

	// batchReady becomes true when any worker from the batch under judgment
	// delivers a body byte. This prevents natural acceleration of an existing
	// flow from being credited to a batch that has contributed nothing.
	batchReady bool
}

type rampSample struct {
	bytes   int64
	elapsed time.Duration
}

func (s rampSample) rate() float64 {
	return float64(s.bytes) / max(s.elapsed.Seconds(), 1e-9)
}

func (rs *rampState) note(total int64) {
	if rs.done.Load() {
		return
	}
	rs.mu.Lock()
	spawnFrom, spawnN, demoteTo := rs.noteLocked(total, rs.now())
	rs.mu.Unlock()
	for i := range spawnN {
		rs.spawn(spawnFrom + i)
	}
	if demoteTo > 0 {
		rs.demote(demoteTo)
	}
}

// noteWorkerReady marks worker id as contributing when it delivers its first
// body byte.
func (rs *rampState) noteWorkerReady(id int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	// admittedAt zero means no batch was ever admitted: the initial worker
	// is not a batch and has no admission to be measured against.
	if rs.admittedAt.IsZero() || id < rs.prevAdmitted || id >= rs.admitted {
		return
	}
	rs.batchReady = true
}

func (rs *rampState) rejectUnreadyLocked() (spawnFrom, spawnN, demoteTo int) {
	rs.done.Store(true)
	return 0, 0, rs.prevAdmitted
}

// rejectThrottled handles an explicit 429 on a ranged request from worker
// id. MinParts is a performance preference; a server overload response wins:
//
//	above the floor (batch under judgment or frozen) → roll back to prevAdmitted
//	at the floor (including MinParts == Parts)       → demote to one flow
//	already at one flow                              → nothing to shed
//
// Expansion always stops. A 429 from a worker id at or past the current
// admitted count is stale — that worker was already selected for retirement
// by an earlier rollback — and must not compound the demotion; survivors
// hold the lowest ids. After a rollback the next 429 steps to the floor, so a
// persistently hostile host walks admitted → floor → 1 in explicit steps.
func (rs *rampState) rejectThrottled(id int) {
	rs.mu.Lock()
	rs.done.Store(true)
	if id >= rs.admitted || rs.admitted <= 1 {
		rs.mu.Unlock()
		return
	}
	keep := 1
	if rs.admitted > rs.floor {
		keep = max(rs.prevAdmitted, rs.floor)
	}
	rs.admitted = keep
	rs.prevAdmitted = rs.floor
	rs.mu.Unlock()
	rs.demote(keep)
}

// noteLocked advances the ramp state machine by at most one window and
// returns the side effects to run after the lock is released: workers to
// spawn and/or a flow count to demote to. Pure given (total, now).
func (rs *rampState) noteLocked(total int64, now time.Time) (spawnFrom, spawnN, demoteTo int) {
	if rs.done.Load() || total-rs.markAt < rs.window {
		return 0, 0, 0
	}
	bytes := total - rs.markAt
	elapsed := now.Sub(rs.markTime)
	rs.markAt = total
	rs.markTime = now
	switch {
	case !rs.warmed:
		// Burn-in: mostly TCP slow-start of the first connection.
		rs.warmed = true
	case rs.settling:
		// The just-admitted batch is still dialing/ramping; consume windows
		// without deciding and without touching the baseline, and do not
		// finish settling before the batch had wall time to connect and at least
		// one created worker has delivered body bytes.
		if now.Sub(rs.admittedAt) >= rs.settleMin {
			if rs.batchReady {
				rs.settling = false
				rs.unreadyWindows = 0
			} else {
				rs.unreadyWindows++
				if rs.unreadyWindows > rampMeasureWindows {
					return rs.rejectUnreadyLocked()
				}
			}
		}
	case rs.admitted == rs.floor && rs.admittedAt.IsZero():
		// Record the pre-admission steady window and probe the first batch.
		return rs.admitLocked(rampSample{bytes: bytes, elapsed: elapsed}, now)
	default:
		sample, ready := rs.measureLocked(bytes, elapsed)
		if !ready {
			return 0, 0, 0
		}
		return rs.decideLocked(sample, now)
	}
	return 0, 0, 0
}

// measureLocked returns one aggregate rate over consecutive equal-byte
// windows. Summing bytes and duration avoids averaging per-window rates, which
// would overweight a short, fast sample.
func (rs *rampState) measureLocked(bytes int64, elapsed time.Duration) (rampSample, bool) {
	rs.sampleBytes += bytes
	rs.sampleTime += elapsed
	rs.sampleCount++
	if rs.sampleCount < rampMeasureWindows {
		return rampSample{}, false
	}
	sample := rampSample{bytes: rs.sampleBytes, elapsed: rs.sampleTime}
	rs.sampleBytes = 0
	rs.sampleTime = 0
	rs.sampleCount = 0
	return sample, true
}

func (rs *rampState) decideLocked(sample rampSample, now time.Time) (spawnFrom, spawnN, demoteTo int) {
	rate := sample.rate()
	baseline := rs.admissionBaseline.rate()
	switch {
	case rate < baseline*rampDemote:
		rs.done.Store(true)
		return 0, 0, rs.prevAdmitted
	case rate < baseline*rampImprovement:
		rs.done.Store(true)
	case rs.admitted < rs.parts:
		return rs.admitLocked(sample, now)
	default:
		rs.done.Store(true)
	}
	return 0, 0, 0
}

func (rs *rampState) admitLocked(sample rampSample, now time.Time) (spawnFrom, spawnN, demoteTo int) {
	add := min(rs.admitted, rs.parts-rs.admitted)
	rs.admissionBaseline = sample
	rs.admittedAt = now
	rs.prevAdmitted = rs.admitted
	spawnFrom = rs.admitted
	rs.admitted += add
	rs.settling = true
	rs.unreadyWindows = 0
	rs.batchReady = false
	return spawnFrom, add, 0
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
	remaining := sched.remainingBytes()
	start := sched.prepare(r.conc.MinParts)
	canRamp := rampEligible(remaining, r.conc.MinPartSize, r.conc.Parts)
	placement := r.d.newNodePlacement(runCtx, r.placementInput(start > 1 || canRamp))
	if placement != nil {
		r.placement = placement
		r.measurement.setPlacement(true)
		defer placement.close()
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
	// ctl holds each worker's cancel so retirement can wake it out of
	// reads, dials, and sleeps. Never hold ctl.mu while cancelling or while
	// calling scheduler/Reporter code.
	ctl := struct {
		mu      sync.Mutex
		cancels map[int]context.CancelCauseFunc
	}{cancels: make(map[int]context.CancelCauseFunc)}
	spawn := func(id int) {
		wctx, wcancel := context.WithCancelCause(runCtx)
		ctl.mu.Lock()
		ctl.cancels[id] = wcancel
		ctl.mu.Unlock()
		w := newWorker(id, r, sched, file)
		wg.Go(func() {
			defer func() {
				ctl.mu.Lock()
				delete(ctl.cancels, id)
				ctl.mu.Unlock()
				wcancel(nil)
			}()
			if err := w.run(wctx); err != nil {
				fail(err)
			}
		})
	}
	retire := func(keep int) {
		victims := sched.demote(keep)
		for _, id := range victims {
			ctl.mu.Lock()
			wcancel := ctl.cancels[id]
			ctl.mu.Unlock()
			if wcancel != nil {
				wcancel(errWorkerRetired)
			}
		}
	}
	// Window: big enough to measure meaningfully while leaving room to
	// evaluate several doubling steps. At the remaining/16 branch, the
	// default 1→2→4→8 ramp reaches its final judgment near the midpoint;
	// the fixed 2*MinPartSize cap makes it earlier on larger objects.
	// Size from REMAINING work so a near-complete resume still ramps.
	window := max(min(2*r.conc.MinPartSize, remaining/16), 1)
	r.ramp = &rampState{
		spawn:     spawn,
		demote:    retire,
		now:       time.Now,
		parts:     r.conc.Parts,
		floor:     start,
		window:    window,
		settleMin: settleFloorFor(r.electDur),
		admitted:  start,
		markTime:  time.Now(),
	}
	if start >= r.conc.Parts || !canRamp {
		// No throughput ramp: the floor already fills Parts, or the remaining
		// work cannot feed every configured connection. The governor still
		// exists so an explicit 429 can shed eager flows.
		r.ramp.done.Store(true)
	}
	if placement != nil {
		placement.startCuller(runCtx, sched, r.ramp)
	}
	for id := range start {
		spawn(id)
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
		sched.mu.Lock()
		var detail strings.Builder
		detail.WriteString(fmt.Sprintf("pending=%d active=%d live=%v retiring=%v limit=%d",
			len(sched.pending), len(sched.active), sched.live, sched.retiring, sched.limit))
		for _, c := range sched.active {
			detail.WriteString(fmt.Sprintf(" active[%d]{owner=%d off=%d done=%d end=%d}",
				c.id, c.owner, c.off, c.done, c.end))
		}
		for _, c := range sched.pending {
			detail.WriteString(fmt.Sprintf(" pending[%d]{off=%d done=%d end=%d}", c.id, c.off, c.done, c.end))
		}
		sched.mu.Unlock()
		return fmt.Errorf("internal: workers exited with work remaining (%s)", detail.String())
	}
	return nil
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
	w := newWorker(0, r, nil, file)
	if err := w.singleStream(ctx); err != nil {
		return nil, err
	}
	return r.verifyAndFinalize(file, false)
}
