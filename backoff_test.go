package download

import (
	"testing"
	"time"
)

func TestBackoffGrowthAndCap(t *testing.T) {
	t.Parallel()
	var b backoff
	prevMax := time.Duration(0)
	for i := range 10 {
		d := b.next()
		lo := time.Duration(float64(d) / (1 + backoffJitter))
		hi := time.Duration(float64(d) / (1 - backoffJitter))
		// The un-jittered value for attempt i is base*factor^i, capped.
		want := float64(backoffBase)
		for range i {
			want *= backoffFactor
			if want >= float64(backoffCap) {
				want = float64(backoffCap)
				break
			}
		}
		if float64(lo) > want || float64(hi) < want {
			t.Fatalf("attempt %d: delay %v not within jitter of %v", i, d, time.Duration(want))
		}
		if d > time.Duration(float64(backoffCap)*(1+backoffJitter)) {
			t.Fatalf("attempt %d: delay %v exceeds cap", i, d)
		}
		if d > prevMax {
			prevMax = d
		}
	}
	if prevMax < backoffCap/2 {
		t.Fatalf("backoff never approached cap: max %v", prevMax)
	}
}

func TestBackoffReset(t *testing.T) {
	t.Parallel()
	var b backoff
	for range 6 {
		b.next()
	}
	b.reset()
	d := b.next()
	if d > time.Duration(float64(backoffBase)*(1+backoffJitter)) {
		t.Fatalf("after reset expected ~base delay, got %v", d)
	}
}
