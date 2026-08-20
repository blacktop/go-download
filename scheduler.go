package download

import (
	"sync"
	"sync/atomic"
)

// chunk is a half-open byte range [off, end) of the target file. end may
// shrink while the chunk is in flight when another worker steals its tail.
type chunk struct {
	id  int
	off int64
	end int64 // guarded by scheduler.mu
	// done is the claim cursor: bytes handed to the owner for writing,
	// guarded by scheduler.mu.
	done int64
	// written counts bytes whose WriteAt completed; it trails done and is
	// what the resume sidecar records.
	written atomic.Int64
}

// grant is the race-free view of a chunk handed to a worker, including the
// shrunken victim when the chunk was carved out of an in-flight one.
type grant struct {
	c       *chunk
	off     int64
	length  int64
	written int64
	victim  *regrant
}

// regrant describes a chunk whose tail was just stolen, for re-reporting.
type regrant struct {
	id      int
	off     int64
	length  int64
	written int64
}

// scheduler hands byte ranges to workers. Fresh downloads start with a single
// chunk covering the whole file; idle workers obtain work by splitting the
// largest in-flight remainder. Resumed downloads seed pending with the
// incomplete chunks from the sidecar.
type scheduler struct {
	mu      sync.Mutex
	pending []*chunk
	active  map[int]*chunk
	minSize int64
	nextID  int
}

func newScheduler(minSize int64) *scheduler {
	return &scheduler{active: make(map[int]*chunk), minSize: minSize}
}

// addPending registers a not-yet-owned chunk. done bytes at the front of the
// range are already on disk (resume path).
func (s *scheduler) addPending(off, end, done int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &chunk{id: s.nextID, off: off, end: end, done: done}
	c.written.Store(done)
	s.nextID++
	s.pending = append(s.pending, c)
}

// next returns the next work grant for an idle worker: a pending chunk if
// any, otherwise the second half of the largest in-flight remainder (when
// that remainder is at least 2*minSize). Nil means no work is left for this
// worker.
func (s *scheduler) next() *grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		c := s.pending[0]
		s.pending = s.pending[1:]
		s.active[c.id] = c
		return &grant{c: c, off: c.off, length: c.end - c.off, written: c.done}
	}
	var victim *chunk
	for _, a := range s.active {
		if victim == nil || a.end-(a.off+a.done) > victim.end-(victim.off+victim.done) {
			victim = a
		}
	}
	if victim == nil {
		return nil
	}
	remaining := victim.end - (victim.off + victim.done)
	if remaining < 2*s.minSize {
		return nil
	}
	mid := victim.off + victim.done + remaining/2
	c := &chunk{id: s.nextID, off: mid, end: victim.end}
	s.nextID++
	victim.end = mid
	s.active[c.id] = c
	return &grant{
		c: c, off: c.off, length: c.end - c.off,
		victim: &regrant{
			id:      victim.id,
			off:     victim.off,
			length:  victim.end - victim.off,
			written: victim.done,
		},
	}
}

// claim reserves up to n bytes of c for writing. It returns the absolute file
// offset to write at, how many bytes may be written (0 when the chunk's tail
// was stolen out from under the owner), and whether the chunk is finished.
// The owner must write exactly the claimed bytes at the returned offset.
func (s *scheduler) claim(c *chunk, n int) (offset int64, write int, stop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	offset = c.off + c.done
	remaining := c.end - offset
	write = int(min(int64(n), remaining))
	c.done += int64(write)
	return offset, write, c.off+c.done >= c.end
}

// cursor returns the absolute offset of the next byte the owner should
// request, the current (possibly shrunken) range end, and whether work
// remains.
func (s *scheduler) cursor(c *chunk) (off, end int64, todo bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	off = c.off + c.done
	return off, c.end, off < c.end
}

// complete removes a finished chunk from the active set.
func (s *scheduler) complete(c *chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, c.id)
}

// idle reports whether no work remains anywhere.
func (s *scheduler) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) == 0 && len(s.active) == 0
}

// snapshot returns a consistent view of all incomplete chunks for the resume
// sidecar. Ranges not covered by the result are fully written.
func (s *scheduler) snapshot() []chunkState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]chunkState, 0, len(s.pending)+len(s.active))
	for _, c := range s.pending {
		out = append(out, chunkState{Off: c.off, End: c.end, Done: c.written.Load()})
	}
	for _, c := range s.active {
		out = append(out, chunkState{Off: c.off, End: c.end, Done: min(c.written.Load(), c.end-c.off)})
	}
	return out
}
