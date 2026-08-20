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

var (
	errStall          = errors.New("read stalled")
	errCulled         = errors.New("node statistically slow, reassigning")
	errShortBody      = errors.New("server closed body before range completed")
	errRangeIgnored   = errors.New("server ignored Range request")
	errContentChanged = errors.New("remote content changed during download")
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

	node    *node
	client  *http.Client
	timeout time.Duration
	dtt     int // full buffers until the next timeout decay step
	bo      backoff
	buf     []byte
}

func newWorker(id int, r *run, sched *scheduler, file *os.File, p *picker) *worker {
	return &worker{
		id:      id,
		r:       r,
		sched:   sched,
		file:    file,
		picker:  p,
		timeout: r.d.opt.Timeout,
		buf:     make([]byte, bufSize),
	}
}

// run pulls chunks until the scheduler has nothing left for this worker.
func (w *worker) run(ctx context.Context) error {
	defer w.dropNode()
	for {
		if ctx.Err() != nil {
			return nil
		}
		g := w.sched.next()
		if g == nil {
			return nil
		}
		if g.victim != nil {
			w.r.d.rep.ChunkStart(g.victim.id, g.victim.off, g.victim.length, g.victim.written)
		}
		w.r.d.rep.ChunkStart(g.c.id, g.off, g.length, g.written)
		if err := w.downloadChunk(ctx, g.c); err != nil {
			if ctx.Err() != nil {
				return nil // cancellation noise; the cause carries the story
			}
			return err
		}
	}
}

// downloadChunk retries a chunk until done or the retry budget is spent.
// Every attempt recomputes its Range from the claim cursor, so a retry never
// re-downloads written bytes.
func (w *worker) downloadChunk(ctx context.Context, c *chunk) error {
	for attempt := 0; ; attempt++ {
		err := w.attempt(ctx, c)
		if err == nil {
			w.sched.complete(c)
			w.r.d.rep.ChunkDone(c.id)
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if perm, ok := errors.AsType[*permanentError](err); ok {
			return fmt.Errorf("chunk %d: %w", c.id, perm.err)
		}
		if attempt+1 >= w.r.d.opt.MaxRetries {
			return fmt.Errorf("chunk %d: %w: %w", c.id, ErrMaxRetry, err)
		}
		w.r.d.rep.ChunkRetry(c.id, attempt+1, err)
		w.r.d.log.Debug("retrying chunk", "worker", w.id, "chunk", c.id,
			"attempt", attempt+1, "err", err)
		if serr := sleepCtx(ctx, w.bo.next()); serr != nil {
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

	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()

	trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
		w.r.d.rep.Connected(c.id, ci.Conn.RemoteAddr().String())
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, trace),
		http.MethodGet, w.r.url, nil)
	if err != nil {
		return &permanentError{err}
	}
	w.r.d.applyHeaders(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", cursor, end-1))
	if v := w.r.validator(); v != "" {
		req.Header.Set("If-Range", v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return w.classify(err, actx)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		s, _, _, crErr := parseContentRange(resp.Header.Get("Content-Range"))
		if crErr != nil || s != cursor {
			return &permanentError{fmt.Errorf("wrong Content-Range %q for cursor %d",
				resp.Header.Get("Content-Range"), cursor)}
		}
	case resp.StatusCode == http.StatusOK:
		if w.r.validator() != "" {
			// If-Range mismatch: the remote file was replaced.
			return &permanentError{errContentChanged}
		}
		w.strikeNode()
		w.dropNode()
		return errRangeIgnored
	case isRetryableStatus(resp.StatusCode):
		w.strikeNode()
		return StatusError(resp.StatusCode)
	default:
		return &permanentError{StatusError(resp.StatusCode)}
	}

	var body io.Reader = resp.Body
	if w.picker != nil {
		body = &observedReader{r: resp.Body, w: w}
	}
	if err := w.readLoop(body, timer, w.chunkSink(c)); err != nil {
		return w.classify(err, actx)
	}
	return nil
}

// chunkSink writes each read at the claimed offset. Claiming before writing
// makes tail-stealing safe: bytes past a shrunken end are simply discarded.
func (w *worker) chunkSink(c *chunk) func(buf []byte, d time.Duration) (bool, error) {
	return func(buf []byte, d time.Duration) (bool, error) {
		if len(buf) == 0 {
			w.r.d.rep.ChunkProgress(c.id, 0, d)
			return false, nil
		}
		off, n, stop := w.sched.claim(c, len(buf))
		if n > 0 {
			if _, err := w.file.WriteAt(buf[:n], off); err != nil {
				return false, &permanentError{fmt.Errorf("write %s: %w", w.file.Name(), err)}
			}
			c.written.Add(int64(n))
			w.r.d.rep.ChunkProgress(c.id, n, d)
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

// observedReader feeds per-read throughput into the node picker and aborts
// the body with errCulled once the node proves statistically slow. Observing
// raw reads (typically one TLS record) keeps the EWMA responsive even when
// the node is too slow to ever fill a whole buffer.
type observedReader struct {
	r io.Reader
	w *worker
}

func (o *observedReader) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := o.r.Read(p)
	if n > 0 {
		o.w.picker.observe(o.w.node, int64(n), time.Since(start))
	}
	if err == nil && o.w.picker.shouldCull(o.w.node) {
		return n, errCulled
	}
	return n, err
}

// readLoop reads body in buf-sized pieces with stall detection, feeding each
// read to sink. It returns nil when sink reports done, io.EOF when the body
// ended first, or the read/sink error.
func (w *worker) readLoop(body io.Reader, timer *time.Timer,
	sink func([]byte, time.Duration) (bool, error)) error {
	for {
		timer.Reset(w.timeout)
		start := time.Now()
		n, err := io.ReadFull(body, w.buf)
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
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return io.EOF
			}
			return err
		}
	}
}

// classify sorts an attempt failure into retryable/permanent and applies the
// node-health consequences.
func (w *worker) classify(err error, actx context.Context) error {
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
	w.strikeNode()
	w.dropNode()
	return err
}

func (w *worker) bumpTimeout() {
	w.timeout = min(w.timeout+timeoutStep, maxStallTimeout)
	w.dtt = decayWindow
}

func (w *worker) decayTimeout() {
	base := w.r.d.opt.Timeout
	if w.timeout <= base {
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
		w.client = &http.Client{Transport: w.r.d.roundTripper()}
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
	w.client = &http.Client{Transport: tr}
	w.r.d.log.Debug("worker pinned to node", "worker", w.id, "addr", pinned)
	return nil
}

// dropNode releases the worker's pinned node so the next attempt picks a
// (possibly different) one. A no-op when pinning is disabled: the shared
// client must not be torn down.
func (w *worker) dropNode() {
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

func (w *worker) strikeNode() {
	if w.picker != nil {
		w.picker.strike(w.node)
	}
}

// singleStream downloads the whole body sequentially (no Range support or
// unknown size). A retry restarts from byte zero.
func (w *worker) singleStream(ctx context.Context) error {
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
		w.r.d.rep.ChunkRetry(0, attempt+1, err)
		if serr := sleepCtx(ctx, w.bo.next()); serr != nil {
			return err
		}
	}
}

func (w *worker) singleAttempt(ctx context.Context) error {
	if err := w.ensureClient(ctx); err != nil {
		return err
	}
	if err := w.file.Truncate(0); err != nil {
		return &permanentError{fmt.Errorf("truncate %s: %w", w.file.Name(), err)}
	}

	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(w.timeout, func() { cancel(errStall) })
	defer timer.Stop()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, w.r.url, nil)
	if err != nil {
		return &permanentError{err}
	}
	w.r.d.applyHeaders(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return w.classify(err, actx)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case isRetryableStatus(resp.StatusCode):
		return StatusError(resp.StatusCode)
	default:
		return &permanentError{StatusError(resp.StatusCode)}
	}

	w.r.d.rep.ChunkStart(0, 0, w.r.total, 0)
	var written int64
	sink := func(buf []byte, d time.Duration) (bool, error) {
		if len(buf) == 0 {
			w.r.d.rep.ChunkProgress(0, 0, d)
			return false, nil
		}
		if _, err := w.file.WriteAt(buf, written); err != nil {
			return false, &permanentError{fmt.Errorf("write %s: %w", w.file.Name(), err)}
		}
		written += int64(len(buf))
		w.r.d.rep.ChunkProgress(0, len(buf), d)
		if len(buf) == len(w.buf) {
			w.decayTimeout()
		}
		return w.r.total >= 0 && written >= w.r.total, nil
	}
	err = w.readLoop(resp.Body, timer, sink)
	switch {
	case err == nil:
		w.r.d.rep.ChunkDone(0)
		return nil
	case errors.Is(err, io.EOF):
		if w.r.total < 0 {
			w.r.d.rep.ChunkDone(0)
			return nil // unknown length: EOF is success
		}
		return w.classify(errShortBody, actx)
	default:
		return w.classify(err, actx)
	}
}
