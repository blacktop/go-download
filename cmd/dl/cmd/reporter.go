package cmd

import (
	"fmt"
	"sync"
	"time"

	"github.com/blacktop/go-download"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const totalBarPriority = 1 << 20

// mpbReporter renders download.Reporter events as mpb progress bars: one bar
// per chunk (created and re-sized as the scheduler splits work) plus an
// aggregate total bar.
type mpbReporter struct {
	p     *mpb.Progress
	mu    sync.Mutex
	bars  map[int]*mpb.Bar
	total *mpb.Bar
	// counted tracks the bytes attributed to each chunk so a restarted
	// chunk (single-stream retry re-announces id 0 with written=0) rolls
	// its contribution back out of the aggregate instead of double
	// counting.
	counted map[int]int64
	sum     int64
}

func newMpbReporter() *mpbReporter {
	return &mpbReporter{
		p: mpb.New(
			mpb.WithWidth(64),
			mpb.WithRefreshRate(200*time.Millisecond),
		),
		bars:    make(map[int]*mpb.Bar),
		counted: make(map[int]int64),
	}
}

func (r *mpbReporter) Start(info download.Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := max(info.Total, 0)
	r.total = r.p.AddBar(total,
		mpb.BarPriority(totalBarPriority),
		mpb.PrependDecorators(
			decor.Name(info.Name, decor.WCSyncWidthR),
			decor.NewPercentage(" % d", decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.AverageSpeed(decor.SizeB1024(0), "% .1f", decor.WCSyncSpace),
			decor.Name(" ETA:", decor.WCSyncSpace),
			decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncSpace),
		),
	)
	if info.Resumed > 0 {
		r.sum = info.Resumed
		r.total.SetCurrent(info.Resumed)
		r.total.SetRefill(info.Resumed)
	}
}

func (r *mpbReporter) ChunkStart(id int, off, length, written int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bar, ok := r.bars[id]; ok {
		// Re-announced chunk: either its tail was stolen (shrink the bar)
		// or a single-stream retry restarted it from written=0 (reset the
		// bar and roll its bytes back out of the aggregate).
		if written < r.counted[id] {
			r.sum += written - r.counted[id]
			r.counted[id] = written
			bar.SetCurrent(written)
			if r.total != nil {
				r.total.SetCurrent(r.sum)
			}
		}
		bar.SetTotal(length, false)
		return
	}
	bar := r.p.AddBar(max(length, 0),
		mpb.BarPriority(id),
		mpb.BarRemoveOnComplete(),
		mpb.PrependDecorators(
			decor.Name(fmt.Sprintf("  part %02d", id), decor.WCSyncWidthR),
			decor.NewPercentage(" % d", decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.EwmaSpeed(decor.SizeB1024(0), "% .1f", 30, decor.WCSyncSpace),
		),
	)
	if written > 0 {
		bar.SetCurrent(written)
		bar.SetRefill(written)
	}
	// Resumed bytes are already in sum via Info.Resumed; just record them.
	r.counted[id] = written
	r.bars[id] = bar
}

func (r *mpbReporter) Connected(id int, addr string) {
	log.Debug("connected", "part", id, "addr", addr)
}

func (r *mpbReporter) ChunkProgress(id int, n int, d time.Duration) {
	r.mu.Lock()
	bar, ok := r.bars[id]
	var total *mpb.Bar
	var sum int64
	if n > 0 {
		r.counted[id] += int64(n)
		r.sum += int64(n)
		total, sum = r.total, r.sum
	}
	r.mu.Unlock()
	if ok {
		bar.EwmaIncrBy(n, d)
	}
	if total != nil {
		total.SetCurrent(sum)
	}
}

func (r *mpbReporter) ChunkRetry(id int, attempt int, err error) {
	fmt.Fprintf(r.p, "part %02d retry #%d: %v\n", id, attempt, err)
}

func (r *mpbReporter) ChunkDone(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bar, ok := r.bars[id]; ok {
		bar.SetTotal(-1, true) // complete at current progress
	}
}

func (r *mpbReporter) Done(err error) {
	r.mu.Lock()
	if r.total != nil {
		if err != nil {
			r.total.Abort(false)
		} else {
			r.total.SetTotal(-1, true)
		}
	}
	for _, bar := range r.bars {
		if err != nil {
			bar.Abort(true)
		}
	}
	r.mu.Unlock()
	r.p.Wait()
}
