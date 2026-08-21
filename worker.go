package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"time"
)

const (
	// timeoutStep and maxStallTimeout drive the adaptive stall ladder:
	// each stall grows the timeout by a step (up to the cap); sustained
	// healthy reads decay it back toward Options.Timeout.
	timeoutStep     = 5 * time.Second
	maxStallTimeout = 90 * time.Second
	// decayWindow is how many consecutive full-buffer reads earn one
	// decay step.
	decayWindow = 16
)

// maxFreeRotations bounds budget-free node rotations per chunk (culls and
// dial failures) so a fully unreachable host still exhausts the retry
// budget instead of rotating forever.
const maxFreeRotations = 8

var (
	errStall           = errors.New("read stalled")
	errCulled          = errors.New("node statistically slow, reassigning")
	errNodeUnreachable = errors.New("node unreachable, reassigning")
	// errWorkerRetired is the cancellation cause used to wake and gracefully
	// stop a worker the ramp demoted; it must never surface as a download
	// failure.
	errWorkerRetired   = errors.New("worker retired by concurrency governor")
	errSharedLeaseMiss = errors.New("reusable probe connection unavailable")
	errRangeCapped     = errors.New("server capped range response")
	errShortBody       = errors.New("server closed body before range completed")
	errRangeIgnored    = errors.New("server ignored Range request")
	errContentChanged  = errors.New("remote content changed during download")
)

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// worker owns one connection slot: it pulls chunks from the scheduler and
// downloads each with retry, stall detection, and (when enabled) CDN node
// pinning.
type worker struct {
	id     int
	r      *run
	sched  *scheduler
	file   *os.File
	picker *picker // nil when pinning is disabled

	node   *node
	client *http.Client
	// sharedClient is worker 0's one-attempt lease on the base transport. It
	// must be detached without closing the base pool after that attempt.
	sharedClient bool
	timeout      time.Duration
	dtt          int // full buffers until the next timeout decay step
	bo           backoff
	buf          []byte
	bufp         *[]byte // pool token for releaseBuf
	// announced tracks whether the single-stream path has emitted its
	// ChunkStart (retries emit ChunkRestart instead).
	announced bool
	// sleep is the retry/backoff sleeper; tests replace it with a
	// channel-coordinated fake to prove cancellation without wall-clock
	// assertions. Internal seam only.
	sleep func(ctx context.Context, d time.Duration) error
}

func newWorker(id int, r *run, sched *scheduler, file *os.File, p *picker) *worker {
	bp := r.d.bufs.Get().(*[]byte)
	w := &worker{
		id:      id,
		r:       r,
		sched:   sched,
		file:    file,
		picker:  p,
		timeout: r.d.opt.Timeout,
		buf:     *bp,
		bufp:    bp,
		sleep:   sleepCtx,
	}
	if r.d.sleepHook != nil {
		w.sleep = r.d.sleepHook
	}
	if id == 0 && p != nil && r.probeReusable {
		w.client = r.d.newClient(r.d.base)
		w.sharedClient = true
	}
	return w
}

// releaseBuf returns the worker's read buffer to the Downloader pool.
// The worker must not read after this.
func (w *worker) releaseBuf() {
	if w.bufp == nil {
		return
	}
	w.buf = nil
	w.r.d.bufs.Put(w.bufp)
	w.bufp = nil
}

// run pulls chunks until the scheduler has nothing left for this worker.
func (w *worker) run(ctx context.Context) error {
	defer w.releaseBuf()
	defer w.dropNode()
	defer w.sched.exit(w.id)
	for {
		if ctx.Err() != nil {
			return nil
		}
		c := w.sched.next(w.id)
		if c == nil {
			return nil
		}
		if err := w.downloadChunk(ctx, c); err != nil {
			if context.Cause(ctx) == errWorkerRetired { //nolint:errorlint // exact internal sentinel
				// Genuine permanent failures (integrity, protocol, writes, and
				// the retirement invariant) must reach fail when retirement races
				// them. Other cancellation causes retain caller/parent precedence.
				if genuineUnderRetirement(err) {
					return err
				}
				// Universal retirement net: the governor shrank this chunk
				// to its claim cursor under the scheduler lock before
				// cancelling, so whatever error escaped downloadChunk
				// (cancellation can land between any two instructions
				// there), the chunk is drained and every claimed byte was
				// written — complete it and exit cleanly.
				off, _, todo := w.sched.cursor(c)
				if !todo && c.written.Load() == off-c.off {
					w.sched.complete(c)
					w.r.rep.ChunkDone(c.id)
					return nil
				}
				// Structurally unreachable; fail loudly, never strand.
				return &permanentError{fmt.Errorf(
					"internal: retired worker %d left chunk %d incomplete", w.id, c.id)}
			}
			if ctx.Err() != nil {
				return nil // cancellation noise; the cause carries the story
			}
			return err
		}
	}
}

// genuineUnderRetirement decides error precedence when retirement races a
// failure: permanent errors (integrity, protocol, content-change, writes,
// and the retirement invariant itself) must still reach the pool's fail
// path; anything else is retirement noise the graceful net may absorb.
func genuineUnderRetirement(err error) bool {
	_, genuine := errors.AsType[*permanentError](err)
	return genuine
}

// downloadChunk retries a chunk until done or the retry budget is spent.
// Every attempt recomputes its Range from the claim cursor, so a retry never
// re-downloads written bytes.
func (w *worker) downloadChunk(ctx context.Context, c *chunk) error {
	attempt := 0
	freeRotations := 0
	chargedAt := w.r.progress.Load()
	for {
		err := w.attempt(ctx, c)
		if err == nil {
			w.sched.complete(c)
			w.r.rep.ChunkDone(c.id)
			return nil
		}
		if errors.Is(err, errSharedLeaseMiss) {
			if ctx.Err() != nil {
				return err
			}
			// The base pool had no acceptable reused connection. attempt detached
			// the shared client, so retry immediately through normal pinned
			// selection without a strike, backoff, or retry-budget charge.
			continue
		}
		if perm, ok := errors.AsType[*permanentError](err); ok {
			// Genuine permanent/integrity/write failures always win a race
			// with retirement and keep today's first-error behavior. Wrap
			// the marker itself so run() can distinguish genuine failures
			// from cancellation noise.
			return fmt.Errorf("chunk %d: %w", c.id, perm)
		}
		if ctx.Err() != nil {
			// Retirement handling lives in run()'s universal net: the
			// cancel can land between any two instructions here, so no
			// in-loop check can be complete.
			return err
		}
		if errors.Is(err, errRangeCapped) {
			// A complete server-declared subrange advanced the cursor. Continue
			// immediately without charging progress against the retry budget.
			w.r.d.log.Debug("continuing capped range", "worker", w.id, "chunk", c.id)
			continue
		}
		if (errors.Is(err, errCulled) || errors.Is(err, errNodeUnreachable)) &&
			freeRotations < maxFreeRotations {
			// Abandoning a statistically slow or unreachable node for a
			// better one is progress, not failure: no retry budget, no
			// backoff. Rotations are bounded here and by strikes/bans, so
			// a fully dead host still falls through to charged retries.
			freeRotations++
			w.r.d.log.Debug("reassigning chunk", "worker", w.id, "chunk", c.id, "err", err)
			continue
		}
		throttled := errors.Is(err, StatusError(http.StatusTooManyRequests))
		if throttled {
			// While siblings ARE progressing, waiting out the throttle is
			// queued work rather than failure, so it costs no retry budget —
			// a server admitting one range at a time must serialize us, not
			// kill the download. Budget is charged only when nobody has
			// advanced since this chunk's last charged attempt.
			if cur := w.r.progress.Load(); cur > chargedAt {
				chargedAt = cur
				w.r.d.log.Debug("waiting out server throttle", "worker", w.id, "chunk", c.id)
				if serr := w.sleep(ctx, time.Second); serr != nil {
					return err
				}
				continue
			}
		}
		chargedAt = w.r.progress.Load()
		attempt++
		if attempt >= w.r.d.opt.MaxRetries {
			return fmt.Errorf("chunk %d: %w: %w", c.id, ErrMaxRetry, err)
		}
		w.r.rep.ChunkRetry(c.id, attempt, err)
		w.r.d.log.Debug("retrying chunk", "worker", w.id, "chunk", c.id,
			"attempt", attempt, "err", err)
		// 429s always sleep the flat politeness pause and never touch
		// bo.next(), so throttle waits — free or charged — cannot escalate
		// the exponential backoff that later 5xx/reset retries will use.
		delay := time.Second
		if !throttled {
			delay = w.bo.next()
		}
		if serr := w.sleep(ctx, delay); serr != nil {
			return err
		}
	}
}

// attempt performs one ranged request for the chunk's remaining bytes.
func (w *worker) attempt(ctx context.Context, c *chunk) error {
	cursor, end, todo := w.sched.cursor(c)
	if !todo {
		return nil
	}
	if err := w.ensureClient(ctx); err != nil {
		return err
	}
	sharedAttempt := w.sharedClient
	if sharedAttempt {
		defer w.detachSharedClient()
	}

	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()

	trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
		if sharedAttempt && !ci.Reused {
			cancel(errSharedLeaseMiss)
			return
		}
		w.r.rep.Connected(c.id, ci.Conn.RemoteAddr().String())
	}}
	reqCtx := httptrace.WithClientTrace(actx, trace)
	if sharedAttempt {
		reqCtx = context.WithValue(reqCtx, sharedLeaseContextKey{}, struct{}{})
	}
	req, err := http.NewRequestWithContext(reqCtx,
		http.MethodGet, w.r.url, nil)
	if err != nil {
		return &permanentError{err}
	}
	w.r.applyHeaders(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", cursor, end-1))
	if v := w.r.validator(); v != "" {
		req.Header.Set("If-Range", v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		if context.Cause(actx) == errSharedLeaseMiss || errors.Is(err, errSharedLeaseMiss) { //nolint:errorlint // exact internal sentinel
			return errSharedLeaseMiss
		}
		return w.classify(err, actx)
	}
	if context.Cause(actx) == errSharedLeaseMiss { //nolint:errorlint // exact internal sentinel
		_ = resp.Body.Close()
		return errSharedLeaseMiss
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		contentRange := resp.Header.Get("Content-Range")
		s, e, total, crErr := parseContentRange(contentRange)
		// The range must start at the cursor and describe our representation
		// (total "*" is RFC-valid unknown). A server-revised shorter range is
		// accepted, but it may not extend beyond the requested range.
		if crErr != nil || s != cursor || e < s || e >= end ||
			(total != -1 && total != w.r.total) {
			_ = resp.Body.Close()
			return &permanentError{fmt.Errorf(
				"wrong Content-Range %q for requested bytes %d-%d/%d",
				contentRange, cursor, end-1, w.r.total)}
		}
		responseEnd := e + 1 // convert inclusive HTTP end to scheduler-exclusive end
		capped := responseEnd < end
		defer resp.Body.Close()
		body := &observedReader{r: io.LimitReader(resp.Body, responseEnd-s), w: w}
		err := w.readLoop(body, timer, w.chunkSink(c))
		if err == nil {
			return nil
		}
		if capped && errors.Is(err, io.EOF) {
			next, _, _ := w.sched.cursor(c)
			if next >= responseEnd {
				return errRangeCapped
			}
		}
		return w.classify(err, actx)
	case resp.StatusCode == http.StatusOK:
		_ = resp.Body.Close()
		if w.r.validator() != "" {
			// If-Range mismatch: the remote file was replaced.
			return &permanentError{errContentChanged}
		}
		w.strikeNode()
		w.dropNode()
		return errRangeIgnored
	case resp.StatusCode == http.StatusTooManyRequests:
		// "Slow down" is server-wide, not this node's fault: back off via
		// the retry path without striking (a strike-ban here would churn
		// through every node and make the overload worse), and freeze the
		// concurrency ramp — the server just told us the flow count is
		// already too high.
		_ = resp.Body.Close()
		if w.r.ramp != nil {
			w.r.ramp.done.Store(true)
		}
		return StatusError(resp.StatusCode)
	case isRetryableStatus(resp.StatusCode):
		_ = resp.Body.Close()
		w.strikeNode()
		w.dropNode()
		return StatusError(resp.StatusCode)
	default:
		_ = resp.Body.Close()
		return &permanentError{StatusError(resp.StatusCode)}
	}
}

// chunkSink writes each read at the claimed offset. Claiming before writing
// makes tail-stealing safe: bytes past a shrunken end are simply discarded.
func (w *worker) chunkSink(c *chunk) func(buf []byte, d time.Duration) (bool, error) {
	return func(buf []byte, d time.Duration) (bool, error) {
		if len(buf) == 0 {
			w.r.rep.ChunkProgress(c.id, 0, d)
			return false, nil
		}
		off, n, stop := w.sched.claim(c, len(buf))
		if n > 0 {
			if _, err := w.file.WriteAt(buf[:n], off); err != nil {
				return false, &permanentError{fmt.Errorf("write %s: %w", w.file.Name(), err)}
			}
			c.written.Add(int64(n))
			w.r.rep.ChunkProgress(c.id, n, d)
		}
		if stop {
			return true, nil
		}
		if len(buf) == len(w.buf) {
			w.decayTimeout()
		}
		return false, nil
	}
}

// observedReader feeds per-read throughput into the node picker and the
// concurrency ramp, and aborts the body with errCulled once the node proves
// statistically slow. Observing raw reads (typically one TLS record) keeps
// both responsive even when the connection is too slow to ever fill a whole
// buffer.
type observedReader struct {
	r io.Reader
	w *worker
}

func (o *observedReader) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := o.r.Read(p)
	if n > 0 {
		if o.w.picker != nil {
			o.w.picker.observe(o.w.node, int64(n), time.Since(start))
		}
		total := o.w.r.progress.Add(int64(n))
		if o.w.r.ramp != nil {
			o.w.r.ramp.note(total)
		}
	}
	if err == nil && o.w.picker != nil && o.w.picker.shouldCull(o.w.node) {
		return n, errCulled
	}
	return n, err
}

// readLoop reads a known-length body in buf-sized pieces via io.ReadFull,
// which maximizes per-read throughput; a short final buffer surfaces as
// io.EOF because the sink's byte accounting decides completeness.
func (w *worker) readLoop(body io.Reader, timer *time.Timer,
	sink func([]byte, time.Duration) (bool, error)) error {
	return w.pump(timer, sink, func(buf []byte) (int, error) {
		n, err := io.ReadFull(body, buf)
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = io.EOF
		}
		return n, err
	})
}

// readUnknownLoop reads a response with no declared length until a clean EOF.
// Unlike readLoop, it must not use io.ReadFull: for an unknown-length body a
// short final buffer is normal, while an unexpected EOF indicates truncation.
func (w *worker) readUnknownLoop(body io.Reader, timer *time.Timer,
	sink func([]byte, time.Duration) (bool, error)) error {
	return w.pump(timer, sink, body.Read)
}

// pump drives read in buf-sized pieces with stall detection, feeding each
// read to sink. It returns nil when sink reports done, or the read/sink
// error (io.EOF when the body ended before sink was done).
func (w *worker) pump(timer *time.Timer,
	sink func([]byte, time.Duration) (bool, error),
	read func([]byte) (int, error)) error {
	if len(w.buf) == 0 {
		// A zero-length buffer would spin forever making no progress
		// (released back to the pool, or never acquired).
		return &permanentError{errors.New("internal: worker read buffer unavailable")}
	}
	for {
		timer.Reset(w.timeout)
		start := time.Now()
		n, err := read(w.buf)
		d := time.Since(start)
		done, serr := sink(w.buf[:n], d)
		if serr != nil {
			return serr
		}
		if done {
			timer.Stop()
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// classify sorts an attempt failure into retryable/permanent and applies the
// node-health consequences. It is the single funnel for transport errors, so
// URL redaction lives here rather than at each client.Do call site.
func (w *worker) classify(err error, actx context.Context) error {
	err = redactErr(err)
	if _, ok := errors.AsType[*permanentError](err); ok {
		return err
	}
	if errors.Is(err, errCulled) {
		w.strikeNode()
		w.dropNode()
		return errCulled
	}
	if context.Cause(actx) == errStall { //nolint:errorlint // exact sentinel set by our AfterFunc
		w.bumpTimeout()
		w.strikeNode()
		w.dropNode()
		return errStall
	}
	if errors.Is(err, io.EOF) {
		return errShortBody
	}
	// Connection-level failure (unreachable address, reset, TLS error):
	// strike the pinned node and rotate so the retry can try another one.
	// A pinned dial failure (e.g. an unroutable AAAA node on a v4-only
	// network) fails in microseconds and should rotate for free rather
	// than burning retry budget and backoff sleeps.
	dialFailed := isDialError(err)
	w.strikeNode()
	w.dropNode()
	if dialFailed && w.picker != nil {
		return fmt.Errorf("%w: %w", errNodeUnreachable, err)
	}
	return err
}

// isDialError reports whether err failed before a connection existed.
func isDialError(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok {
		return op.Op == "dial"
	}
	return false
}

func (w *worker) bumpTimeout() {
	base := w.r.d.opt.Timeout
	if w.timeout < base {
		w.timeout = base
	}
	ceiling := max(base, maxStallTimeout)
	if w.timeout >= ceiling-timeoutStep {
		w.timeout = ceiling
	} else {
		w.timeout += timeoutStep
	}
	w.dtt = decayWindow
}

func (w *worker) decayTimeout() {
	base := w.r.d.opt.Timeout
	if w.timeout <= base {
		w.timeout = base
		return
	}
	if w.dtt > 0 {
		w.dtt--
		return
	}
	w.timeout -= timeoutStep
	w.dtt = decayWindow
	if w.timeout <= base {
		w.timeout = base
		w.bo.reset()
	}
}

// ensureClient lazily builds the worker's HTTP client. With a picker, the
// client's transport pins connections to one chosen node.
func (w *worker) ensureClient(ctx context.Context) error {
	if w.client != nil {
		return nil
	}
	if w.picker == nil {
		w.client = w.r.d.newClient(w.r.d.roundTripper())
		return nil
	}
	n, err := w.picker.pick(ctx)
	if err != nil {
		return err
	}
	w.node = n
	tr := w.r.d.base.Clone()
	expected := net.JoinHostPort(w.picker.host, w.picker.port)
	pinned := net.JoinHostPort(n.addr.String(), w.picker.port)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == expected {
			addr = pinned
		}
		return w.r.d.dial(ctx, network, addr)
	}
	tr.MaxConnsPerHost = 1
	w.client = w.r.d.newClient(tr)
	w.r.d.log.Debug("worker pinned to node", "worker", w.id, "addr", pinned)
	return nil
}

// dropNode releases the worker's pinned node so the next attempt picks a
// (possibly different) one. A no-op when pinning is disabled: the shared
// client must not be torn down.
func (w *worker) dropNode() {
	if w.sharedClient {
		w.detachSharedClient()
		return
	}
	if w.picker == nil || w.node == nil {
		return
	}
	if w.client != nil {
		w.client.CloseIdleConnections()
	}
	w.client = nil
	w.picker.release(w.node)
	w.node = nil
}

func (w *worker) detachSharedClient() {
	if !w.sharedClient {
		return
	}
	w.client = nil
	w.sharedClient = false
}

func (w *worker) strikeNode() {
	if w.picker != nil {
		w.picker.strike(w.node)
	}
}

// singleStream downloads the whole body sequentially (no Range support or
// unknown size). A retry restarts from byte zero.
func (w *worker) singleStream(ctx context.Context) error {
	defer w.releaseBuf()
	for attempt := 0; ; attempt++ {
		err := w.singleAttempt(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if perm, ok := errors.AsType[*permanentError](err); ok {
			return perm.err
		}
		if attempt+1 >= w.r.d.opt.MaxRetries {
			return fmt.Errorf("%w: %w", ErrMaxRetry, err)
		}
		w.r.rep.ChunkRetry(0, attempt+1, err)
		if serr := w.sleep(ctx, w.bo.next()); serr != nil {
			return err
		}
	}
}

func (w *worker) singleAttempt(ctx context.Context) error {
	if err := w.ensureClient(ctx); err != nil {
		return err
	}

	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, w.r.url, nil)
	if err != nil {
		return &permanentError{err}
	}
	w.r.applyHeaders(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return w.classify(err, actx)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		defer resp.Body.Close()
	case isRetryableStatus(resp.StatusCode):
		_ = resp.Body.Close()
		w.strikeNode()
		w.dropNode()
		return StatusError(resp.StatusCode)
	default:
		_ = resp.Body.Close()
		return &permanentError{StatusError(resp.StatusCode)}
	}

	// Truncate only after a successful response: a failed attempt must not
	// destroy previously staged bytes (e.g. a resumable multipart .part).
	if err := w.file.Truncate(0); err != nil {
		return &permanentError{fmt.Errorf("truncate %s: %w", w.file.Name(), err)}
	}

	expected := w.r.total
	if expected < 0 && resp.ContentLength >= 0 {
		expected = resp.ContentLength
	}
	if !w.announced {
		w.announced = true
		w.r.rep.ChunkStart(0, 0, expected, 0)
	} else if rs, ok := w.r.rep.(ChunkRestarter); ok {
		// A retry discarded the previous attempt's bytes (Truncate above).
		rs.ChunkRestart(0)
	}
	var written int64
	sink := func(buf []byte, d time.Duration) (bool, error) {
		if len(buf) == 0 {
			w.r.rep.ChunkProgress(0, 0, d)
			// A zero-length body (empty file) is complete without ever
			// delivering bytes.
			return expected >= 0 && written >= expected, nil
		}
		if _, err := w.file.WriteAt(buf, written); err != nil {
			return false, &permanentError{fmt.Errorf("write %s: %w", w.file.Name(), err)}
		}
		written += int64(len(buf))
		w.r.rep.ChunkProgress(0, len(buf), d)
		if len(buf) == len(w.buf) {
			w.decayTimeout()
		}
		return expected >= 0 && written >= expected, nil
	}
	if expected < 0 {
		// A plain Read preserves the distinction between a clean EOF and an
		// unexpected EOF from a truncated chunked response. io.ReadFull, used
		// for known-size bodies below, necessarily collapses both after a
		// final short buffer.
		err = w.readUnknownLoop(resp.Body, timer, sink)
	} else {
		err = w.readLoop(resp.Body, timer, sink)
	}
	switch {
	case err == nil:
		if w.r.total < 0 && expected >= 0 {
			w.r.total = expected
		}
		w.r.rep.ChunkDone(0)
		return nil
	case errors.Is(err, io.EOF):
		if expected < 0 {
			w.r.total = written
			w.r.rep.ChunkDone(0)
			return nil // unknown length: EOF is success
		}
		return w.classify(errShortBody, actx)
	default:
		return w.classify(err, actx)
	}
}
