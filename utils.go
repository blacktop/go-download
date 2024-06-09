package download

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"
)

type delay struct {
	base   time.Duration
	factor float64
	jitter float64
}

func Backoff(dur time.Duration) *delay {
	return &delay{
		base:   dur,
		factor: 1.6,
		jitter: 0.2,
	}
}

func (d *delay) next(attempt int) time.Duration {
	if attempt == 0 {
		return d.base
	}
	backoff := float64(d.base)
	for backoff < float64(maxTimeout) && attempt > 0 {
		backoff *= d.factor
		attempt--
	}
	if backoff > float64(maxTimeout) {
		backoff = float64(maxTimeout)
	}
	if d.jitter > 0 {
		backoff *= 1 + (rand.Float64()*2-1)*d.jitter
	}
	if backoff < 0 {
		return 0
	}
	return time.Duration(backoff)
}

// Retry will retry a function f a number of attempts with a sleep duration in between
func Retry(attempts int, sleep *delay, f func() error) (err error) {
	return RetryWithContext(context.Background(), attempts, sleep, f)
}

func RetryWithContext(ctx context.Context, attempts int, sleep *delay, f func() error) (err error) {
	for i := 0; i < attempts; i++ {
		err = f()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(sleep.next(i))
		select {
		case <-timer.C:
			// continue
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func RandomAgent() string {
	var userAgents = []string{
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/112.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/61.0.3163.100 Safari/537.36",
		"Mozilla/5.0 (Windows NT 6.1; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/61.0.3163.100 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/61.0.3163.100 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/604.1.38 (KHTML, like Gecko) Version/11.0 Safari/604.1.38",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:56.0) Gecko/20100101 Firefox/56.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13) AppleWebKit/604.1.38 (KHTML, like Gecko) Version/11.0 Safari/604.1.38",
	}
	return userAgents[rand.IntN(len(userAgents))]
}

// GetProxy takes either an input string or read the enviornment and returns a proxy function
func GetProxy(proxy string) func(*http.Request) (*url.URL, error) {
	if len(proxy) > 0 {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return http.ProxyFromEnvironment
		}
		return http.ProxyURL(proxyURL)
	}
	return http.ProxyFromEnvironment
}
