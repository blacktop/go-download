package download

import (
	"math/rand/v2"
	"time"
)

const (
	backoffBase   = 500 * time.Millisecond
	backoffFactor = 2.0
	backoffCap    = 30 * time.Second
	backoffJitter = 0.2
)

// backoff produces exponentially growing delays with jitter. reset() returns
// the sequence to the base delay; the adaptive stall-timeout ladder calls it
// once a connection is healthy again.
type backoff struct {
	attempt int
}

func (b *backoff) reset() { b.attempt = 0 }

// next returns the delay to sleep before the upcoming retry attempt.
func (b *backoff) next() time.Duration {
	d := float64(backoffBase)
	for range b.attempt {
		d *= backoffFactor
		if d >= float64(backoffCap) {
			d = float64(backoffCap)
			break
		}
	}
	b.attempt++
	jitter := 1 + backoffJitter*(2*rand.Float64()-1)
	return time.Duration(d * jitter)
}
