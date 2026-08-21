package cmd

import (
	"io"
	"testing"
	"time"

	"github.com/blacktop/go-download"
	"github.com/vbauerster/mpb/v8"
)

// newTestReporter is newMpbReporter with rendering discarded and a fast
// refresh so completion/removal races surface within the test timeout.
func newTestReporter() *mpbReporter {
	return &mpbReporter{
		p: mpb.New(
			mpb.WithOutput(io.Discard),
			mpb.WithWidth(64),
			mpb.WithRefreshRate(10*time.Millisecond),
		),
		bars:    make(map[int]*mpb.Bar),
		counted: make(map[int]int64),
	}
}

// TestMpbRetirementResizeToZeroCompletes pins the mpb v8.15.2 contract the
// retirement path depends on: ChunkResize(id, 0) (SetTotal(0, false)) must
// NOT complete a bar, the following ChunkDone(id) (SetTotal(-1, true)) must
// force completion, and Done/Wait must return without the remove-on-complete
// bar lingering.
func TestMpbRetirementResizeToZeroCompletes(t *testing.T) {
	r := newTestReporter()
	r.Start(download.Info{Name: "file.bin", Total: 100})
	r.ChunkStart(1, 0, 50, 0)
	r.ChunkProgress(1, 10, time.Millisecond)

	r.ChunkResize(1, 0)
	if r.bars[1].Completed() {
		t.Fatal("SetTotal(0, false) completed the bar; retirement relies on it staying incomplete")
	}

	r.ChunkDone(1)
	if !r.bars[1].Completed() {
		t.Fatal("SetTotal(-1, true) did not force completion of the zero-total bar")
	}

	done := make(chan struct{})
	go func() {
		r.Done(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Done/Wait did not return: a bar is lingering after retirement resize-to-zero")
	}
}
