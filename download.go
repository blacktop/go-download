package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const (
	maxTimeout   = 180
	maxRedirects = 10
	refreshRate  = 200
)

type Config struct {
	Context  context.Context
	Proxy    string
	Insecure bool
	CertPath string

	Timeout time.Duration

	Parts int

	//
	Resume     bool
	SkipAll    bool
	ResumeAll  bool
	RestartAll bool

	Progress bool
	Logger   *slog.Logger

	IgnoreHash bool
	Verbose    bool
}

type Manager struct {
	ctx  context.Context
	conf *Config

	client *http.Client

	log *slog.Logger

	progress     *mpb.Progress
	totalBarIncr func(int)
	totalCancel  func(bool)

	URL           *url.URL
	Hash          any
	DestName      string
	AcceptRanges  string
	ContentType   string
	ContentAge    string
	ContentDate   string
	ContentSha256 string
	ContentSha1   string
	ContentLength int64
	Redirected    bool

	Headers map[string]string

	canResume bool

	doneCount    uint32
	size         int64
	bytesResumed int64

	Elapsed time.Duration
	Parts   []*Part
}

func New(conf *Config) (*Manager, error) {
	if conf.Timeout == 0 {
		conf.Timeout = 15 * time.Second
	}
	transport := DefaultTransport()
	if conf.Parts != 0 {
		transport = DefaultPooledTransport()
	}

	transport.Proxy = GetProxy(conf.Proxy)
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: conf.Insecure}

	if conf.CertPath != "" {
		buf, err := os.ReadFile(conf.CertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read cert file %s: %w", conf.CertPath, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("failed to get system cert pool: %w", err)
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return nil, fmt.Errorf("failed to append cert from %s: %w", conf.CertPath, err)
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	mgr := &Manager{
		conf: conf,
		log:  conf.Logger,
		client: &http.Client{
			Transport: transport,
		},
	}

	if conf.Context == nil {
		mgr.ctx = context.Background()
	} else {
		mgr.ctx = conf.Context
	}

	if err := mgr.createProgress(); err != nil {
		return nil, err
	}

	return mgr, nil
}

func (mgr *Manager) Get(url string) error {
	// get URL info
	if err := mgr.head(url); err != nil {
		return fmt.Errorf("failed to get URL info: %w", err)
	}
	// get state of the download
	if err := mgr.state(); err != nil {
		return fmt.Errorf("failed to get download state: %w", err)
	}

	ctx, cancel := context.WithCancel(mgr.ctx)
	defer cancel()

	for i, p := range mgr.Parts {
		if p.isDone() {
			atomic.AddUint32(&mgr.doneCount, 1)
			continue
		}
		p.ctx = ctx
		p.order = i + 1
		p.name = fmt.Sprintf("P%02d", p.order)
		// p.maxTry = cmd.options.MaxRetry
		p.single = len(mgr.Parts) == 1
		p.progress = mgr.progress
		p.totalBarIncr = mgr.totalBarIncr
		p.logger = mgr.log
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return err
		}
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		// var eg errgroup.Group
		// eg.Go(func() error {
		// 	defer func() {
		// 		// if e := recover(); e != nil {
		// 		// 	cancel()
		// 		// 	onceSessionHandle.Do(sessionHandle)
		// 		// 	panic(fmt.Sprintf("%s panic: %v", p.name, e)) // https://go.dev/play/p/55nmnsXyfSA
		// 		// }
		// 		switch {
		// 		case p.isDone():
		// 			atomic.AddUint32(&mgr.doneCount, 1)
		// 		case p.Skip:
		// 			mgr.totalCancel(true)
		// 		}
		// 	}()
		// return p.download(mgr.client, req, fmt.Sprintf("[%s:R%%02d] ", p.name))
		if err := p.download(mgr.client, req, fmt.Sprintf("[%s:R%%02d] ", p.name)); err != nil {
			return err
		}
		// })
	}

	return mgr.concatenateParts()
}

func (mgr *Manager) state() error {
	if mgr.resumable() {
		if f, err := os.Stat(mgr.DestName + ".download"); !os.IsNotExist(err) {
			mgr.size = f.Size()
		} else {
			if err := mgr.createParts(); err != nil {
				return fmt.Errorf("failed to create download parts: %w", err)
			}
		}
	} else {
		if err := mgr.createParts(); err != nil {
			return fmt.Errorf("failed to create download parts: %w", err)
		}
	}
	return nil
}

func (mgr *Manager) head(url string) error {
	mgr.log.Debug("HEAD", "url", url)

	var redirected bool
	defer func() {
		if redirected {
			mgr.client.CloseIdleConnections()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return RetryWithContext(ctx, 3, Backoff(500*time.Millisecond), func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return err
		}

		for k, v := range mgr.Headers {
			mgr.log.Debug("Headers", k, v)
			req.Header.Set(k, v)
		}

		resp, err := mgr.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			return errors.New(resp.Status)
		}

		// if cookies := jar.Cookies(req.URL); len(cookies) != 0 {
		// 	mgr.log.Println("CookieJar:")
		// 	for _, cookie := range cookies {
		// 		mgr.log.Printf("  %q", cookie)
		// 	}
		// }

		if resp.StatusCode > 299 && resp.StatusCode < 400 {
			redirected = true
			loc, err := resp.Location()
			if err != nil {
				return err
			}
			url = loc.String()
			if resp.Body != nil {
				resp.Body.Close()
			}
			return errors.New("Redirected")
		}

		mgr.URL = resp.Request.URL
		mgr.AcceptRanges = resp.Header.Get("Accept-Ranges")
		mgr.ContentType = resp.Header.Get("Content-Type")
		mgr.ContentAge = resp.Header.Get("Age")
		mgr.ContentDate = resp.Header.Get("Date")
		mgr.ContentSha256 = resp.Header.Get("x-amz-meta-digest-sha256")
		mgr.ContentSha1 = resp.Header.Get("x-amz-meta-digest-sh1")
		mgr.ContentLength = resp.ContentLength

		if mgr.DestName == "" {
			mgr.DestName = path.Base(req.URL.Path)
		}

		return nil
	})
}

func (mgr *Manager) resumable() bool {
	return strings.EqualFold(mgr.AcceptRanges, "bytes") && mgr.ContentLength > 0
}

func (mgr *Manager) written() int64 {
	var total int64
	for _, p := range mgr.Parts {
		total += p.Written
	}
	return total
}

func (mgr *Manager) createParts() error {
	if mgr.conf.Parts == 0 {
		mgr.conf.Parts = 1
	} else if !mgr.resumable() {
		mgr.conf.Parts = 1
	}

	fragment := mgr.ContentLength / int64(mgr.conf.Parts)
	if mgr.conf.Parts != 1 && fragment < 64 {
		return errors.New("Too fragmented")
	}

	mgr.Parts = make([]*Part, mgr.conf.Parts)
	mgr.Parts[0] = &Part{
		FileName: mgr.DestName,
	}

	var stop int64
	start := mgr.ContentLength
	for i := mgr.conf.Parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - fragment
		mgr.Parts[i] = &Part{
			FileName: fmt.Sprintf("%s.%02d", mgr.DestName, i),
			Start:    start,
			Stop:     stop,
		}
	}

	mgr.Parts[0].Stop = start - 1

	return nil
}

func (mgr *Manager) createProgress() error {
	if mgr.conf.Progress {
		mgr.progress = mpb.NewWithContext(mgr.ctx,
			mpb.WithOutput(os.Stdout),
			mpb.WithDebugOutput(io.Discard),
			mpb.WithRefreshRate(refreshRate*time.Millisecond),
			mpb.WithWidth(64),
		)
		bar, err := mgr.progress.Add(mgr.ContentLength,
			barRefiller(barStyle()).Build(),
			mpb.BarFillerTrim(),
			mpb.BarExtender(mpb.BarFillerFunc(
				func(w io.Writer, _ decor.Statistics) error {
					_, err := fmt.Fprintln(w)
					return err
				}), true),
			mpb.PrependDecorators(
				decor.Any(func(_ decor.Statistics) string {
					return fmt.Sprintf("Total(%d/%d)", atomic.LoadUint32(&mgr.doneCount), len(mgr.Parts))
				}, decor.WCSyncWidthR),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
			),
			mpb.AppendDecorators(
				decor.OnCompleteOrOnAbort(decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidth), ":"),
				decor.AverageSpeed(decor.SizeB1024(0), "%.1f", decor.WCSyncSpace),
				decor.Name("", decor.WCSyncSpace),
				decor.Name("", decor.WCSyncSpace),
			),
		)
		if err != nil {
			return err
		}
		if written := mgr.written(); written != 0 {
			bar.SetCurrent(written)
			bar.SetRefill(written)
			bar.DecoratorAverageAdjust(time.Now().Add(-mgr.Elapsed))
		}
		ch := make(chan int, len(mgr.Parts)-int(atomic.LoadUint32(&mgr.doneCount)))
		ctx, cancel := context.WithCancel(mgr.ctx)
		dropCtx, dropCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case n := <-ch:
					bar.IncrBy(n)
				case <-dropCtx.Done():
					cancel()
					bar.Abort(true)
					return
				case <-ctx.Done():
					dropCancel()
					bar.Abort(false)
					return
				}
			}
		}()
		mgr.totalBarIncr = func(n int) {
			select {
			case ch <- n:
			case <-done:
			}
		}
		mgr.totalCancel = func(drop bool) {
			if drop {
				dropCancel()
			} else {
				cancel()
			}
		}
	} else {
		mgr.progress = nil
		mgr.totalBarIncr = func(n int) {}    // NOP
		mgr.totalCancel = func(drop bool) {} // NOP
	}
	return nil
}

func (mgr *Manager) concatenateParts() (err error) {
	if len(mgr.Parts) < 2 {
		return nil
	}

	if mgr.written() != mgr.ContentLength {
		return fmt.Errorf("size mismatch: expected %d got %d", mgr.ContentLength, mgr.written())
	}

	var bar *mpb.Bar
	if mgr.progress != nil {
		bar, err = mgr.progress.Add(int64(len(mgr.Parts)-1),
			barStyle().Build(),
			mpb.BarFillerTrim(),
			mpb.BarPriority(len(mgr.Parts)+1),
			mpb.PrependDecorators(
				decor.Name("Concatenating", decor.WCSyncWidthR),
				decor.NewPercentage("%d", decor.WCSyncSpace),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidth), ":"),
				decor.Name("", decor.WCSyncSpace),
				decor.Name("", decor.WCSyncSpace),
				decor.Name("", decor.WCSyncSpace),
			),
		)
		if err != nil {
			return err
		}
	}

	fpart0, err := os.OpenFile(mgr.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if e := fpart0.Close(); err == nil {
			err = e
		}
		if bar != nil {
			bar.Abort(false) // if bar is completed bar.Abort is nop
		}
	}()

	for i := 1; i < len(mgr.Parts); i++ {
		if !mgr.Parts[i].Skip {
			fparti, err := os.Open(mgr.Parts[i].FileName)
			if err != nil {
				return err
			}
			mgr.log.Info(fmt.Sprintf("concatenating: %q into %q", fparti.Name(), fpart0.Name()))
			if _, err = io.Copy(fpart0, fparti); err != nil {
				return err
			}
			if err := fparti.Close(); err != nil {
				return err
			}
			if err = os.Remove(fparti.Name()); err != nil {
				return err
			}
		}
		if bar != nil {
			bar.Increment()
		}
	}

	if stat, err := fpart0.Stat(); err != nil {
		return err
	} else {
		if mgr.written() != stat.Size() {
			return fmt.Errorf("total bytes written != stat.Size()")
		}
	}

	return nil
}
