package download

import "time"

// Info describes a download at start time.
type Info struct {
	// Name is the resolved output filename (base name of the final path).
	Name string
	// Total is the expected size in bytes, or -1 when unknown
	// (single-stream download without a Content-Length).
	Total int64
	// Resumed is the number of bytes already on disk from a prior run.
	Resumed int64
}

// Reporter receives progress events. Implementations must be safe for
// concurrent use and must not block: events are delivered synchronously from
// download goroutines.
//
// Chunk ids are stable for the life of a chunk. A chunk that is split emits a
// fresh ChunkStart for the newly created chunk and re-emits ChunkStart for
// the shrunken victim with its new length.
type Reporter interface {
	// Start fires once, after the first server response resolves the
	// download's name and size.
	Start(info Info)
	// ChunkStart announces a chunk covering length bytes at absolute file
	// offset off, of which written bytes are already downloaded.
	ChunkStart(id int, off, length, written int64)
	// Connected reports the remote address serving chunk id.
	Connected(id int, addr string)
	// ChunkProgress reports n bytes read for chunk id over duration d.
	ChunkProgress(id int, n int, d time.Duration)
	// ChunkRetry reports a retryable failure on chunk id.
	ChunkRetry(id int, attempt int, err error)
	// ChunkDone fires when a chunk is fully downloaded.
	ChunkDone(id int)
	// Done fires once at the end with the download's final error, if any.
	Done(err error)
}

// NopReporter is a Reporter that ignores all events. Embed it to implement
// only the events you care about.
type NopReporter struct{}

func (NopReporter) Start(Info)                            {}
func (NopReporter) ChunkStart(int, int64, int64, int64)   {}
func (NopReporter) Connected(int, string)                 {}
func (NopReporter) ChunkProgress(int, int, time.Duration) {}
func (NopReporter) ChunkRetry(int, int, error)            {}
func (NopReporter) ChunkDone(int)                         {}
func (NopReporter) Done(error)                            {}
