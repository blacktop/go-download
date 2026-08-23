package download

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

type dialOutcome struct {
	conn          net.Conn
	winner        string
	failed        []string
	preferredLost bool
	err           error
}

type dialResult struct {
	conn net.Conn
	addr string
	err  error
}

// dialPreferred starts primary immediately and starts at most one fallback
// when primary fails or remains pending for delay. The unbuffered result
// channel ensures a race-losing dial observes cancellation and closes any
// connection it obtained instead of leaking it.
func dialPreferred(
	ctx context.Context,
	network, primary, fallback string,
	delay time.Duration,
	dial func(context.Context, string, string) (net.Conn, error),
) dialOutcome {
	if primary == "" {
		return dialOutcome{err: errors.New("empty preferred address")}
	}
	raceCtx, cancel := context.WithCancel(ctx)
	results := make(chan dialResult)
	start := func(addr string) {
		go func() {
			conn, err := dial(raceCtx, network, addr)
			select {
			case results <- dialResult{conn: conn, addr: addr, err: err}:
			case <-raceCtx.Done():
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}
	start(primary)
	started, finished := 1, 0
	fallbackStarted := false
	// Without a fallback there is nothing to race; a nil channel never fires.
	var fallbackDue <-chan time.Time
	if fallback != "" {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		fallbackDue = timer.C
	}
	var failed []string
	var errs []error
	for {
		select {
		case <-ctx.Done():
			cancel()
			return dialOutcome{failed: failed, err: ctx.Err()}
		case <-fallbackDue:
			if !fallbackStarted {
				fallbackStarted = true
				started++
				start(fallback)
			}
		case result := <-results:
			finished++
			if result.err == nil && result.conn != nil {
				cancel()
				return dialOutcome{
					conn: result.conn, winner: result.addr, failed: failed,
					preferredLost: result.addr != primary,
				}
			}
			failed = append(failed, result.addr)
			if result.err == nil {
				result.err = errors.New("dial returned nil connection")
			}
			errs = append(errs, fmt.Errorf("dial %s: %w", result.addr, result.err))
			if result.addr == primary && fallback != "" && !fallbackStarted {
				// The primary failed outright: start the fallback now rather
				// than at the delay. A later timer tick is harmless because
				// fallbackStarted guards it.
				fallbackStarted = true
				started++
				start(fallback)
			}
			if finished == started && (fallback == "" || fallbackStarted) {
				cancel()
				return dialOutcome{failed: failed, err: errors.Join(errs...)}
			}
		}
	}
}
