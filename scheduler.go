package download

import (
	"slices"
	"sync"
	"sync/atomic"
)

// chunk is a half-open byte range [off, end) of the target file. end may
// shrink while the chunk is in flight when another worker steals its tail or
// its owner is retired.
type chunk struct {
	id    int
	owner int // worker currently granted this chunk; guarded by scheduler.mu
	off   int64
	end   int64 // guarded by scheduler.mu
	// done is the claim cursor: bytes handed to the owner for writing,
	// guarded by scheduler.mu.
	done int64
	// written counts bytes whose WriteAt completed; it trails done and is
	// what the resume sidecar records.
	written atomic.Int64
}

// scheduler hands byte ranges to workers. Fresh downloads start with a single
// chunk covering the whole file, pre-split by prepare for eagerly started
// workers; idle workers obtain work by splitting the largest in-flight
// remainder. Resumed downloads seed pending with the
// incomplete chunks from the sidecar. The ramp can retire excess workers via
// demote: their unclaimed remainders move to pending and the flow limit
// refuses them on their next visit.
//
// Locking: mu is a leaf lock, with one sanctioned exception — the onGrant and
// onResize Reporter callbacks execute under it so grant/resize events reach
// the Reporter in a total order. No other lock may be held on that path.
type scheduler struct {
	mu      sync.Mutex
	pending []*chunk
	active  map[int]*chunk
	// live holds workers between registration (first next call) and their
	// deregistration; retiring marks workers demote selected for exit.
	// A worker's register/refuse/grant/drain decision happens in ONE
	// critical section — liveness is never split across calls.
	live     map[int]struct{}
	retiring map[int]struct{}
	// limit caps concurrently granted (non-retiring) workers. 0 means
	// unlimited; it is only ever lowered, and never below 1.
	limit   int
	minSize int64
	nextID  int
	// onGrant and onResize, when set, are called under mu as chunks are
	// handed out and shrunk, so Reporter events arrive in a total order:
	// a chunk's ChunkStart always precedes any ChunkResize touching it.
	onGrant  func(id int, off, length, written int64)
	onResize func(id int, length int64)
}

func newScheduler(minSize int64) *scheduler {
	return &scheduler{
		active:   make(map[int]*chunk),
		live:     make(map[int]struct{}),
		retiring: make(map[int]struct{}),
		minSize:  minSize,
	}
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

// prepare splits pending so up to n workers can each be granted a range
// immediately, and returns how many grants that yields (at least 1). It uses
// the same halving rule as next — largest remainder first, never below
// 2*minSize — so every eager worker spawned for the returned count finds
// work. Pending ranges are unannounced, so splitting them emits no Reporter
// events.
func (s *scheduler) prepare(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.pending) < n {
		var victim *chunk
		for _, c := range s.pending {
			if victim == nil || c.end-(c.off+c.done) > victim.end-(victim.off+victim.done) {
				victim = c
			}
		}
		if victim == nil || victim.end-(victim.off+victim.done) < 2*s.minSize {
			break
		}
		s.pending = append(s.pending, s.splitLocked(victim))
	}
	return max(min(len(s.pending), n), 1)
}

// splitLocked halves c's unclaimed remainder (which must be at least
// 2*minSize) and returns the new upper chunk; c keeps the lower half.
func (s *scheduler) splitLocked(c *chunk) *chunk {
	cursor := c.off + c.done
	mid := cursor + (c.end-cursor)/2
	upper := &chunk{id: s.nextID, off: mid, end: c.end}
	s.nextID++
	c.end = mid
	return upper
}

// next returns the next chunk for worker workerID, or nil when the worker
// must exit. Registration, retirement checks, limit refusal, granting, and
// drain deregistration all happen in one critical section, so "live" always
// means "will call next again or is inside downloadChunk".
func (s *scheduler) next(workerID int) *chunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[workerID] = struct{}{}
	if _, ok := s.retiring[workerID]; ok {
		s.deregisterLocked(workerID)
		return nil
	}
	if s.limit > 0 && s.nonRetiringLiveLocked() > s.limit {
		// Refusing this caller leaves at least limit >= 1 live workers.
		s.deregisterLocked(workerID)
		return nil
	}
	if len(s.pending) > 0 {
		c := s.pending[0]
		s.pending = s.pending[1:]
		c.owner = workerID
		s.active[c.id] = c
		s.grantLocked(c)
		return c
	}
	var victim *chunk
	for _, a := range s.active {
		if victim == nil || a.end-(a.off+a.done) > victim.end-(victim.off+victim.done) {
			victim = a
		}
	}
	if victim != nil {
		if remaining := victim.end - (victim.off + victim.done); remaining >= 2*s.minSize {
			c := s.splitLocked(victim)
			c.owner = workerID
			s.active[c.id] = c
			if s.onResize != nil {
				s.onResize(victim.id, victim.end-victim.off)
			}
			s.grantLocked(c)
			return c
		}
	}
	s.deregisterLocked(workerID)
	return nil
}

// exit deregisters workerID; idempotent. Only for abnormal unwinds (error or
// context cancellation) — normal exits deregister inside next.
func (s *scheduler) exit(workerID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deregisterLocked(workerID)
}

func (s *scheduler) deregisterLocked(workerID int) {
	delete(s.live, workerID)
	delete(s.retiring, workerID)
}

func (s *scheduler) nonRetiringLiveLocked() int {
	n := len(s.live)
	for id := range s.retiring {
		if _, ok := s.live[id]; ok {
			n--
		}
	}
	return n
}

// demote caps concurrent flows at keep and selects exactly the excess
// non-retiring live workers as retirement victims (preferring higher ids,
// never the last keep survivors). Each victim's unclaimed remainder moves to
// a fresh pending chunk and its active chunk shrinks to the claim cursor, so
// the remainder is re-granted to a survivor while the victim finishes within
// one buffer. Returns the victim ids for wakeup cancellation; assignment
// coverage (remainingBytes) is conserved exactly.
func (s *scheduler) demote(keep int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep = max(keep, 1)
	if s.limit == 0 || keep < s.limit {
		s.limit = keep
	}
	excess := s.nonRetiringLiveLocked() - keep
	if excess <= 0 {
		return nil
	}
	victims := make([]int, 0, excess)
	for len(victims) < excess {
		best := -1
		for id := range s.live {
			if _, ok := s.retiring[id]; ok {
				continue
			}
			if !slices.Contains(victims, id) && id > best {
				best = id
			}
		}
		if best < 0 {
			break
		}
		victims = append(victims, best)
		s.retiring[best] = struct{}{}
	}
	for _, c := range s.active {
		if _, ok := s.retiring[c.owner]; !ok {
			continue
		}
		cursor := c.off + c.done
		if cursor >= c.end {
			continue // already drained; nothing to move
		}
		moved := &chunk{id: s.nextID, off: cursor, end: c.end}
		s.nextID++
		c.end = cursor
		s.pending = append(s.pending, moved)
		if s.onResize != nil {
			s.onResize(c.id, c.end-c.off)
		}
	}
	return victims
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

// workerRemaining returns the unclaimed tail currently owned by workerID.
// Zero is conservative for culling: a worker between chunks is treated as a
// draining tail and cannot make its address eligible for judgment.
func (s *scheduler) workerRemaining(workerID int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.active {
		if c.owner == workerID {
			return max(c.end-(c.off+c.done), 0)
		}
	}
	return 0
}

// complete removes a finished chunk from the active set.
func (s *scheduler) complete(c *chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, c.id)
}

// remainingBytes returns how much is still to download across all chunks.
func (s *scheduler) remainingBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, c := range s.pending {
		n += c.end - (c.off + c.done)
	}
	for _, c := range s.active {
		n += c.end - (c.off + c.done)
	}
	return n
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

func (s *scheduler) grantLocked(c *chunk) {
	if s.onGrant != nil {
		s.onGrant(c.id, c.off, c.end-c.off, c.written.Load())
	}
}
