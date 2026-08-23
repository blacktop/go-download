package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"sync/atomic"
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

var (
	errStall           = errors.New("read stalled")
	errSlowNode        = errors.New("slow node culled")
	errNodeUnavailable = errors.New("node address unavailable")
	// errWorkerRetired is the cancellation cause used to wake and gracefully
	// stop a worker the ramp demoted; it must never surface as a download
	// failure.
	errWorkerRetired  = errors.New("worker retired by concurrency governor")
	errRangeCapped    = errors.New("server capped range response")
	errShortBody      = errors.New("server closed body before range completed")
	errRangeIgnored   = errors.New("server ignored Range request")
	errContentChanged = errors.New("remote content changed during download")
)

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

type nodeRotationError struct {
	addrs     []netip.Addr
	err       error
	permanent error
}

func (e *nodeRotationError) Error() string { return e.err.Error() }
func (e *nodeRotationError) Unwrap() error { return e.err }

// rotationBudget grants one budget-free attempt per unique failed address,
// then one DNS refresh, before an address failure falls back to the ordinary
// retry budget. A retained permanent cause (a bad certificate) regains
// precedence once every rotation is spent.
type rotationBudget struct {
	tried     map[netip.Addr]struct{}
	refreshed bool
	permanent error
}

// admit reports whether rotation r earns a free retry; otherwise it returns
// the permanent cause to surface, if any.
func (b *rotationBudget) admit(
	ctx context.Context, place *nodePlacement, r *nodeRotationError,
) (free bool, permanent error) {
	if r.permanent != nil {
		b.permanent = r.permanent
	}
	if b.tried == nil {
		b.tried = make(map[netip.Addr]struct{})
	}
	for _, addr := range r.addrs {
		if _, seen := b.tried[addr]; !seen {
			b.tried[addr] = struct{}{}
			free = true
		}
	}
	if free {
		return true, nil
	}
	if !b.refreshed && place != nil {
		b.refreshed = true
		place.refresh(ctx)
		return true, nil
	}
	return false, b.permanent
}

// worker owns one connection slot: it pulls chunks from the scheduler and
// downloads each with retry and stall detection.
type worker struct {
	id     int
	r      *run
	sched  *scheduler
	file   *os.File
	client *http.Client
	place  *nodePlacement
	// gotConn records whether the current attempt obtained a connection; a
	// failure before that point is connection establishment, not the server.
	gotConn atomic.Bool
	placed  *placedWorker
	timeout time.Duration
	dtt     int // full buffers until the next timeout decay step
	bo      backoff
	buf     []byte
	bufp    *[]byte // pool token for releaseBuf
	// announced tracks whether the single-stream path has emitted its
	// ChunkStart (retries emit ChunkRestart instead).
	announced bool
	// sawBody tracks the worker's first received body byte so the ramp cannot
	// judge a newly admitted batch before that worker contributes.
	sawBody bool
	// sleep is the retry/backoff sleeper; tests replace it with a
	// channel-coordinated fake to prove cancellation without wall-clock
	// assertions. Internal seam only.
	sleep func(ctx context.Context, d time.Duration) error
}

func newWorker(id int, r *run, sched *scheduler, file *os.File) *worker {
	bp := r.d.bufs.Get().(*[]byte)
	w := &worker{
		id:      id,
		r:       r,
		sched:   sched,
		file:    file,
		place:   r.placement,
		timeout: r.d.opt.Timeout,
		buf:     *bp,
		bufp:    bp,
		sleep:   sleepCtx,
	}
	if r.d.sleepHook != nil {
		w.sleep = r.d.sleepHook
	}
	if w.place != nil {
		w.placed = w.place.registerWorker(id)
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
	defer w.closePlacement()
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
	var rotations rotationBudget
	chargedAt := w.r.progress.Load()
	for {
		err := w.attempt(ctx, c)
		if err == nil {
			w.sched.complete(c)
			w.r.rep.ChunkDone(c.id)
			return nil
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
		if errors.Is(err, errSlowNode) {
			w.r.d.log.Debug("retrying chunk after slow-node migration",
				"worker", w.id, "chunk", c.id)
			continue
		}
		if rotation, ok := errors.AsType[*nodeRotationError](err); ok {
			if free, perm := rotations.admit(ctx, w.place, rotation); perm != nil {
				return &permanentError{perm}
			} else if free {
				w.r.d.log.Debug("rotating after address failure",
					"worker", w.id, "chunk", c.id, "err", rotation.err)
				continue
			}
		}
		throttled := errors.Is(err, StatusError(http.StatusTooManyRequests))
		if throttled {
			w.r.measurement.throttle()
			// While siblings ARE progressing, waiting out the throttle is
			// queued work rather than failure, so it costs no retry budget —
			// a server admitting one range at a time must serialize us, not
			// kill the download. Budget is charged only when nobody has
			// advanced since this chunk's last charged attempt.
			if cur := w.r.progress.Load(); cur > chargedAt {
				chargedAt = cur
				w.r.d.log.Debug("waiting out server throttle", "worker", w.id, "chunk", c.id)
				w.r.measurement.retry()
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
		w.r.measurement.retry()
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
	if w.placed != nil && w.placed.rotate.Swap(false) {
		// A cull landed between attempts: re-reserve before dialing.
		w.dropPlacementTransport()
	}
	if cursor == 0 {
		// The election body starts at byte zero: whichever worker is granted
		// that chunk consumes it instead of issuing a duplicate request.
		if resp, addr, ecancel := w.r.takeInitial(); resp != nil {
			return w.initialRangeAttempt(ctx, c, end, resp, addr, ecancel)
		}
	}
	if err := w.ensureClient(); err != nil {
		return err
	}

	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer w.place.beginAttempt(w.placed, cancel)()
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()

	w.gotConn.Store(false)
	trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
		w.recordGotConn(c.id, ci)
	}}
	reqCtx := httptrace.WithClientTrace(actx, trace)
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
		return w.classifyConnection(err, actx, w.gotConn.Load())
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		return w.readPartialResponse(resp, actx, timer, c, cursor, end, end)
	case resp.StatusCode == http.StatusOK:
		_ = resp.Body.Close()
		if w.r.validator() != "" {
			// If-Range mismatch: the remote file was replaced.
			return &permanentError{errContentChanged}
		}
		return errRangeIgnored
	case resp.StatusCode == http.StatusTooManyRequests:
		// The server just told us the flow count is too high; retaining it
		// would only create retry traffic. Shed flows, eager ones included.
		_ = resp.Body.Close()
		if w.r.ramp != nil {
			w.r.ramp.rejectThrottled(w.id)
		}
		return StatusError(resp.StatusCode)
	case isRetryableStatus(resp.StatusCode):
		_ = resp.Body.Close()
		return StatusError(resp.StatusCode)
	default:
		_ = resp.Body.Close()
		return &permanentError{StatusError(resp.StatusCode)}
	}
}

// recordGotConn attributes every connection selected by net/http. Reused
// connections cannot be skipped: after an origin dial, a cross-host redirect
// may reuse its own pooled connection and must clear the origin attribution.
func (w *worker) recordGotConn(chunkID int, ci httptrace.GotConnInfo) {
	w.gotConn.Store(true)
	addr := ci.Conn.RemoteAddr().String()
	if w.place != nil {
		addr = w.place.gotConn(w.id, addr)
	}
	w.r.rep.Connected(chunkID, addr)
	w.r.measurement.connected(addr)
}

func (w *worker) initialRangeAttempt(
	ctx context.Context, c *chunk, end int64,
	resp *http.Response, addr string, ecancel context.CancelCauseFunc,
) error {
	defer ecancel(nil)
	if w.place != nil && addr != "" {
		addr = w.place.attachInitial(w.id, addr)
		defer w.place.releaseAddress(w.placed)
	}
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer w.place.beginAttempt(w.placed, cancel)()
	// The response was created before worker 0 existed, on its own request
	// context. Forward every local cancellation cause (stall, retirement,
	// parent cancel) into that request so a blocked body read genuinely
	// unblocks — Body.Close carries no such contract for arbitrary
	// transports.
	stop := context.AfterFunc(actx, func() {
		ecancel(context.Cause(actx))
		// Also close the (close-once) body: request-context cancellation is
		// the contract, but Close-only custom bodies still deserve a wake.
		_ = resp.Body.Close()
	})
	defer stop()
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()
	if addr != "" {
		w.r.rep.Connected(c.id, addr)
		w.r.measurement.connected(addr)
	}
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return &permanentError{StatusError(resp.StatusCode)}
	}
	// The election asked for bytes=0-, so its Content-Range spans the whole
	// representation regardless of how much of this chunk's tail was
	// pre-split for eager siblings; validate against what was requested but
	// read only up to the chunk's prepared end. The unread remainder is not
	// drained: that would spend bandwidth on bytes siblings already own.
	return w.readPartialResponse(resp, actx, timer, c, 0, w.r.total, end)
}

func (w *worker) readPartialResponse(
	resp *http.Response,
	actx context.Context,
	timer *time.Timer,
	c *chunk,
	cursor, requested, end int64,
) error {
	contentRange := resp.Header.Get("Content-Range")
	s, e, total, crErr := parseContentRange(contentRange)
	// The range must start at the cursor and describe our representation
	// (total "*" is RFC-valid unknown). A server-revised shorter range is
	// accepted, but it may not extend beyond the requested range. Reads are
	// bounded by end, which is the chunk's extent at attempt time (equal to
	// requested except for the reused election response).
	if crErr != nil || s != cursor || e < s || e >= requested ||
		(total != -1 && total != w.r.total) {
		_ = resp.Body.Close()
		return &permanentError{fmt.Errorf(
			"wrong Content-Range %q for requested bytes %d-%d/%d",
			contentRange, cursor, requested-1, w.r.total)}
	}
	responseEnd := min(e+1, end) // inclusive HTTP end to scheduler-exclusive end
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

// observedReader feeds per-read throughput into the concurrency ramp.
// Observing raw reads keeps it responsive even when a connection is too slow
// to fill a whole buffer.
type observedReader struct {
	r io.Reader
	w *worker
}

func (o *observedReader) Read(p []byte) (int, error) {
	var counter *nodeByteCounter
	if o.w.placed != nil {
		counter = o.w.placed.counter.Load()
	}
	n, err := o.r.Read(p)
	if n > 0 {
		if counter != nil && o.w.placed.counter.Load() == counter {
			counter.bytes.Add(int64(n))
		}
		total := o.w.r.progress.Add(int64(n))
		if o.w.r.ramp != nil {
			if !o.w.sawBody {
				// First body byte this worker ever received: it can now
				// participate in the aggregate-rate judgment.
				o.w.sawBody = true
				o.w.r.ramp.noteWorkerReady(o.w.id)
			}
			o.w.r.ramp.note(total)
		}
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

// classify sorts an attempt failure into retryable/permanent. It is the
// single funnel for transport errors, so URL redaction lives here rather than
// at each client.Do call site.
func (w *worker) classify(err error, actx context.Context) error {
	err = redactErr(err)
	if _, ok := errors.AsType[*permanentError](err); ok {
		return err
	}
	if context.Cause(actx) == errSlowNode { //nolint:errorlint // exact internal policy sentinel
		w.dropPlacementTransport()
		return errSlowNode
	}
	if rotation, ok := errors.AsType[*nodeRotationError](err); ok {
		// dialPreferred already parked the failed addresses.
		w.dropPlacementTransport()
		return rotation
	}
	if w.place != nil && certificateFailure(err) {
		// A bad certificate on one address may rotate to another, but
		// verification is never weakened: once every unique address is
		// exhausted the original failure regains permanent precedence.
		return w.rotationError(err, err)
	}
	if context.Cause(actx) == errStall { //nolint:errorlint // exact sentinel set by our AfterFunc
		w.bumpTimeout()
		w.rotateAfterStall()
		return errStall
	}
	if errors.Is(err, io.EOF) {
		return errShortBody
	}
	return err
}

// classifyConnection extends ordinary attempt classification with the
// lifecycle fact supplied by httptrace: without GotConn, connection
// establishment failed before it became usable (during dial or TLS). Rotate the
// pinned transport immediately unless cancellation already explains the
// failure or a more specific classifier did.
func (w *worker) classifyConnection(err error, actx context.Context, gotConn bool) error {
	classified := w.classify(err, actx)
	if gotConn || w.place == nil || actx.Err() != nil {
		return classified
	}
	switch classified.(type) {
	case *nodeRotationError, *permanentError:
		return classified
	}
	return w.rotationError(classified, nil)
}

// rotationError parks the worker's current address without a slow-node
// strike, drops its pinned transport, and wraps err so downloadChunk can grant
// a budget-free rotation; permanent, when set, is surfaced once rotations are
// exhausted.
func (w *worker) rotationError(err, permanent error) error {
	addr := w.place.currentAddress(w.placed)
	w.place.markAvailabilityFailure(addr)
	w.dropPlacementTransport()
	var addrs []netip.Addr
	if addr.IsValid() {
		addrs = []netip.Addr{addr}
	}
	return &nodeRotationError{
		addrs: addrs, err: fmt.Errorf("%w: %w", errNodeUnavailable, err), permanent: permanent,
	}
}

func certificateFailure(err error) bool {
	var verification *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verification) || errors.As(err, &unknown) ||
		errors.As(err, &hostname) || errors.As(err, &invalid)
}

// rotateAfterStall gives this worker a one-reservation preference for another
// address. A read stall is not evidence that the address is unavailable to
// every worker, so it must not herd the run through a node-wide cooldown.
func (w *worker) rotateAfterStall() {
	w.place.avoidCurrentOnNextReservation(w.placed)
	w.dropPlacementTransport()
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

// ensureClient lazily builds either the shared base client or a cloned
// single-connection transport with a preferred logical address.
func (w *worker) ensureClient() error {
	if w.client != nil {
		return nil
	}
	if w.place == nil {
		w.client = w.r.d.newClient(w.r.d.roundTripper())
		return nil
	}
	primary, err := w.place.reserve(w.id)
	if err != nil {
		return &nodeRotationError{err: fmt.Errorf("%w: %w", errNodeUnavailable, err)}
	}
	tr := w.place.createTransport(w.id, primary)
	w.client = w.r.d.newClient(tr)
	w.r.d.log.Debug("worker preferred node", "worker", w.id, "addr", primary)
	return nil
}

// dropPlacementTransport closes the worker's pinned transport and releases
// its address; the next ensureClient reserves afresh. No-op without placement.
func (w *worker) dropPlacementTransport() {
	if w.place == nil {
		return
	}
	w.place.closeTransport(w.id)
	w.place.releaseAddress(w.placed)
	w.client = nil
}

func (w *worker) closePlacement() {
	w.dropPlacementTransport()
	w.place.removeWorker(w.id)
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
		if errors.Is(err, StatusError(http.StatusTooManyRequests)) {
			w.r.measurement.throttle()
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
		w.r.measurement.retry()
		if serr := w.sleep(ctx, w.bo.next()); serr != nil {
			return err
		}
	}
}

// announceSingle emits the single-stream ChunkStart exactly once; retries
// emit ChunkRestart instead (their truncation discarded prior bytes).
func (w *worker) announceSingle(expected int64) {
	if !w.announced {
		w.announced = true
		w.r.rep.ChunkStart(0, 0, expected, 0)
		return
	}
	if rs, ok := w.r.rep.(ChunkRestarter); ok {
		rs.ChunkRestart(0)
	}
}

// headerValidator mirrors run.validator's precedence over a response's own
// headers: a strong ETag, else Last-Modified, else "".
func headerValidator(h http.Header) string {
	if etag := h.Get("ETag"); isStrongETag(etag) {
		return etag
	}
	return h.Get("Last-Modified")
}

// checkSingleStatus classifies a single-stream response: proceed to the
// body (nil, nil), complete as a valid empty resource (empty=true), retry
// (StatusError), or fail permanently. The body is never read here.
func checkSingleStatus(resp *http.Response, initial bool, total int64) (empty bool, err error) {
	switch {
	case resp.StatusCode == http.StatusOK:
		return false, nil
	case initial && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && total == 0:
		return true, nil
	case initial && resp.StatusCode == http.StatusPartialContent:
		contentRange := resp.Header.Get("Content-Range")
		s, e, respTotal, crErr := parseContentRange(contentRange)
		if crErr != nil || s != 0 || respTotal < 0 || e != respTotal-1 ||
			(total >= 0 && respTotal != total) {
			return false, &permanentError{fmt.Errorf(
				"initial Content-Range %q does not cover the full representation",
				contentRange)}
		}
		return false, nil
	case isRetryableStatus(resp.StatusCode):
		return false, StatusError(resp.StatusCode)
	default:
		return false, &permanentError{StatusError(resp.StatusCode)}
	}
}

// completeEmptySingle finishes a zero-length download: truncate, announce,
// done — no body bytes are ever involved.
func (w *worker) completeEmptySingle() error {
	if err := w.file.Truncate(0); err != nil {
		return &permanentError{fmt.Errorf("truncate %s: %w", w.file.Name(), err)}
	}
	w.announceSingle(0)
	w.r.rep.ChunkDone(0)
	return nil
}

// singleSink stages sequential body bytes at the running offset with
// progress reporting; the download is done once the declared length is
// reached (a zero-length body completes without delivering bytes).
func (w *worker) singleSink(written *int64, expected int64) func([]byte, time.Duration) (bool, error) {
	return func(buf []byte, d time.Duration) (bool, error) {
		if len(buf) == 0 {
			w.r.rep.ChunkProgress(0, 0, d)
			return expected >= 0 && *written >= expected, nil
		}
		if _, err := w.file.WriteAt(buf, *written); err != nil {
			return false, &permanentError{fmt.Errorf("write %s: %w", w.file.Name(), err)}
		}
		*written += int64(len(buf))
		w.r.rep.ChunkProgress(0, len(buf), d)
		if len(buf) == len(w.buf) {
			w.decayTimeout()
		}
		return expected >= 0 && *written >= expected, nil
	}
}

func (w *worker) singleAttempt(ctx context.Context) error {
	resp, addr, ecancel := w.r.takeInitial()
	initial := resp != nil
	defer ecancel(nil)
	if !initial {
		if err := w.ensureClient(); err != nil {
			return err
		}
	}

	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if initial {
		// Forward every local cancellation cause into the election request
		// so a blocked body read genuinely unblocks (see initialRangeAttempt).
		stop := context.AfterFunc(actx, func() {
			ecancel(context.Cause(actx))
			_ = resp.Body.Close()
		})
		defer stop()
		if addr != "" {
			w.r.rep.Connected(0, addr)
			w.r.measurement.connected(addr)
		}
	}
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()

	if !initial {
		req, err := http.NewRequestWithContext(actx, http.MethodGet, w.r.url, nil)
		if err != nil {
			return &permanentError{err}
		}
		w.r.applyHeaders(req)
		resp, err = w.client.Do(req)
		if err != nil {
			return w.classify(err, actx)
		}
	}
	empty, err := checkSingleStatus(resp, initial, w.r.total)
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	if empty {
		// A valid empty-resource 416 carries an ERROR body, not content:
		// complete the empty download without reading or staging a byte.
		_ = resp.Body.Close()
		return w.completeEmptySingle()
	}
	defer resp.Body.Close()

	if !initial {
		// A fresh attempt may be answered by a different representation than
		// the election saw. When the election supplied a validator, a changed
		// or missing validator on the retry is a content change — fail before
		// touching previously staged bytes. Either way the fresh response
		// establishes its own full length: stopping it at the election total
		// installs a truncated prefix, and a configured checksum must cover
		// the COMPLETE representation actually downloaded, not a prefix that
		// happens to hash like the old object.
		if v := w.r.validator(); v != "" && headerValidator(resp.Header) != v {
			return &permanentError{errContentChanged}
		}
		w.r.total = -1
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
	w.announceSingle(expected)
	var written int64
	sink := w.singleSink(&written, expected)
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
