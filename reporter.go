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
// download goroutines (ChunkResize additionally under an internal scheduler
// lock, which is what keeps it ordered against the ChunkStart of the chunk
// that stole the range). Downloads sharing the Options-level Reporter are
// serialized so one run's Start-through-Done stream never interleaves with
// another's; per-Request reporters may run concurrently.
//
// Chunk ids are stable for the life of a chunk, and ChunkStart fires exactly
// once per id.
//
// Implement ChunkResizer and ChunkRestarter (NopReporter provides both) to
// additionally observe tail-steals and single-stream restarts; without them
// those transitions are silent, never re-announced through ChunkStart.
type Reporter interface {
	// Start fires once, after the first server response resolves the
	// download's name and size.
	Start(info Info)
	// ChunkStart announces a new chunk covering length bytes at absolute
	// file offset off, of which written bytes are already on disk from a
	// resumed session (0 otherwise).
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

// ChunkResizer is an optional Reporter extension. ChunkResize fires when a
// faster worker steals the tail of chunk id: the chunk now covers length
// bytes (its offset never changes). It is delivered under an internal
// scheduler lock, totally ordered against the stealing chunk's ChunkStart.
type ChunkResizer interface {
	ChunkResize(id int, length int64)
}

// ChunkRestarter is an optional Reporter extension. ChunkRestart fires when
// a single-stream retry discards chunk id's bytes and starts over from zero.
type ChunkRestarter interface {
	ChunkRestart(id int)
}

// NopReporter is a Reporter that ignores all events. Embed it to implement
// only the events you care about.
type NopReporter struct{}

func (NopReporter) Start(Info)                            {}
func (NopReporter) ChunkStart(int, int64, int64, int64)   {}
func (NopReporter) ChunkResize(int, int64)                {}
func (NopReporter) ChunkRestart(int)                      {}
func (NopReporter) Connected(int, string)                 {}
func (NopReporter) ChunkProgress(int, int, time.Duration) {}
func (NopReporter) ChunkRetry(int, int, error)            {}
func (NopReporter) ChunkDone(int)                         {}
func (NopReporter) Done(error)                            {}
