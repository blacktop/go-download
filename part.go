package download

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v8"
)

const bufSize = 4096

var globTry uint32

type HttpError int

func (e HttpError) Error() string {
	return fmt.Sprintf("HTTP error: %d", int(e))
}

type Part struct {
	FileName string
	Start    int64
	Stop     int64
	Written  int64
	Skip     bool
	Elapsed  time.Duration

	ctx          context.Context
	name         string
	order        int
	maxTry       uint
	single       bool
	progress     *mpb.Progress
	logger       *slog.Logger
	totalBarIncr func(int)
}

func (p *Part) download(client *http.Client, req *http.Request, prefix string) (err error) {
	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return errors.WithMessage(err, p.name)
	}
	defer func() {
		fpart.Close()
		if p.Written == 0 {
			p.logger.Debug(fmt.Sprintf("file %q is empty, removing", fpart.Name()))
			os.Remove(fpart.Name())
		}
		err = errors.WithMessage(err, p.name)
	}()

	var bar progressBar
	var curTry uint32
	var statusPartialContent bool
	var attempt uint32

	if p.progress != nil {
		bar.initialized.Load()
	}

	// resetTimeout := timeout
	// prefix := p.logger.Prefix()

	req.Header.Set("Range", p.getRange())

	p.logger = p.logger.WithGroup(fmt.Sprintf(prefix, attempt))
	p.logger.Debug("GET", "url", req.URL)
	for k, v := range req.Header {
		p.logger.Debug("Header", k, v)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		if p.Written == 0 {
			if p.progress != nil {
				fmt.Fprintf(p.progress, "%s%s\n", prefix, err.Error())
			} else {
				p.logger.Error(err.Error())
			}
		} else {
			if p.progress != nil {
				if err := bar.init(p, &curTry); err != nil {
					return err
				}
			}
		}
		return err
	}

	p.logger.Debug("HTTP Status", "status", resp.Status)
	p.logger.Debug("ContentLength", "length", resp.ContentLength)

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if p.progress != nil {
			if err := bar.init(p, &curTry); err != nil {
				return err
			}
		}
		if p.Written != 0 {
			go func(written int64) {
				p.logger.Debug("Setting Bar Refill", "written", written)
				if p.progress != nil {
					bar.SetRefill(written)
				}
			}(p.Written)
		}
		statusPartialContent = true
	case http.StatusOK: // no partial content, download with single part
		if statusPartialContent {
			panic("http.StatusOK after http.StatusPartialContent")
		}
		if p.Written == 0 {
			if p.order != 1 {
				p.Skip = true
				p.logger.Error("Stopping: no partial content")
				return nil
			}
			p.single = true
			if resp.ContentLength > 0 {
				p.Stop = resp.ContentLength - 1
			}
			if p.progress != nil {
				if err := bar.init(p, &curTry); err != nil {
					return err
				}
			}
		} else if p.progress != nil && bar.initialized.Load() {
			err := fpart.Close()
			if err != nil {
				panic(err)
			}
			fpart, err = os.OpenFile(p.FileName, os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				panic(err)
			}
			p.Written = 0
			if p.progress != nil {
				bar.SetCurrent(0)
			}
		} else {
			panic(fmt.Sprintf("expected 0 bytes got %d", p.Written))
		}
	case http.StatusServiceUnavailable:
		if p.progress != nil && bar.initialized.Load() {
			bar.flashErr(resp.Status)
		} else {
			fmt.Fprintf(p.progress, "%s%s\n", prefix, resp.Status)
		}
		return HttpError(resp.StatusCode)
	default:
		fmt.Fprintf(p.progress, "%s%s\n", prefix, resp.Status)
		if p.progress != nil && bar.initialized.Load() {
			bar.Abort(true)
		}
		// if attempt != 0 {
		// 	atomic.AddUint32(&globTry, ^uint32(0))
		// }
		return HttpError(resp.StatusCode)
	}

	defer resp.Body.Close()

	buf := make([]byte, bufSize)
	for n := 0; err == nil; {
		start := time.Now()
		n, err = io.ReadFull(resp.Body, buf)
		dur := time.Since(start)

		switch err {
		case io.EOF:
			continue
		case io.ErrUnexpectedEOF:
			if n == 0 {
				continue
			}
			err = nil // let io.ReadFull return io.EOF
		}
		_, e := fpart.Write(buf[:n])
		if e != nil {
			panic(e)
		}
		p.Written += int64(n)
		if p.total() <= 0 {
			if p.progress != nil {
				bar.SetTotal(p.Written, false)
			}
		} else {
			p.totalBarIncr(n)
		}
		if p.progress != nil {
			bar.EwmaIncrBy(n, dur)
		}
	}

	if err == io.EOF {
		if p.total() <= 0 {
			p.Stop = p.Written - 1 // so p.isDone() retruns true
			if p.progress != nil {
				bar.EnableTriggerComplete()
			}
		}
		if p.isDone() {
			return nil
		}
		panic("part isn't done after EOF")
	}

	if p.isDone() {
		panic(fmt.Sprintf("part is done before EOF: %v", err))
	}

	return nil
}

func (p Part) getRange() string {
	if p.Stop < 1 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

func (p Part) total() int64 {
	return p.Stop - p.Start + 1
}

func (p Part) isDone() bool {
	return p.Written != 0 && p.Written == p.total()
}

func (p Part) makeMsgHandler(msgCh chan<- message) func(message) {
	return func(msg message) {
		select {
		case msgCh <- msg:
		default:
			fmt.Fprintf(p.progress, "%s%s\n", p.logger.Handler(), msg.msg)
		}
	}
}
