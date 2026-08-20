package download

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	// ewmaAlpha smooths per-node throughput over ~30 observations.
	ewmaAlpha = 2.0 / 31
	// cullRatio: a node slower than this fraction of the best node's
	// throughput is abandoned (aria2 --lowest-speed-limit, made relative).
	cullRatio = 0.25
	// strikesToBan governs temporary node blacklisting.
	strikesToBan  = 2
	resolveMaxAge = 2 * time.Minute
)

// Vars rather than consts so tests can tighten them.
var (
	// cullWarmupBytes must be observed from a node before it can be culled.
	cullWarmupBytes int64 = 8 << 20
	banDuration           = 30 * time.Second
)

// node is one resolved address of the download host, with observed stats.
type node struct {
	addr     netip.Addr
	bps      float64 // EWMA throughput; 0 until first observation
	bytes    int64   // total bytes observed (warmup gate)
	conns    int     // workers currently pinned here
	strikes  int
	banUntil time.Time
}

// picker assigns workers to CDN edge nodes using power-of-two-choices over
// per-node EWMA throughput. All fields are guarded by mu.
type picker struct {
	mu         sync.Mutex
	host, port string
	nodes      []*node
	resolvedAt time.Time
	resolve    func(ctx context.Context, host string) ([]netip.Addr, error)
	log        *slog.Logger
}

func newPicker(host, port string, log *slog.Logger) *picker {
	return &picker{
		host: host,
		port: port,
		log:  log,
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}
}

// pick returns the node a worker should dial next. It re-resolves DNS when
// the node list is stale or every node is banned (also unbanning the
// least-bad node so progress is always possible).
func (p *picker) pick(ctx context.Context) (*node, error) {
	p.mu.Lock()
	stale := len(p.nodes) == 0 || time.Since(p.resolvedAt) > resolveMaxAge
	empty := len(p.nodes) == 0
	p.mu.Unlock()
	if stale {
		if err := p.refresh(ctx); err != nil && empty {
			return nil, err
		}
	}

	p.mu.Lock()
	candidates, _ := p.eligibleLocked(time.Now())
	if len(candidates) == 0 {
		p.mu.Unlock()
		if err := p.refresh(ctx); err != nil {
			p.log.Debug("re-resolve failed, unbanning least-bad node", "err", err)
		}
		p.mu.Lock()
		var best *node
		candidates, best = p.eligibleLocked(time.Now())
		if len(candidates) == 0 && best != nil {
			best.banUntil = time.Time{}
			best.strikes = 0
			candidates = append(candidates, best)
		}
	}
	defer p.mu.Unlock()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no usable addresses for %s", p.host)
	}
	chosen := pickTwo(candidates)
	chosen.conns++
	return chosen, nil
}

// eligibleLocked returns the unbanned nodes and, when every node is banned,
// the one whose ban expires soonest. Callers must hold p.mu.
func (p *picker) eligibleLocked(now time.Time) (candidates []*node, leastBad *node) {
	candidates = make([]*node, 0, len(p.nodes))
	for _, n := range p.nodes {
		if now.After(n.banUntil) {
			candidates = append(candidates, n)
		} else if leastBad == nil || n.banUntil.Before(leastBad.banUntil) {
			leastBad = n
		}
	}
	return candidates, leastBad
}

// pickTwo implements power-of-two-choices: sample two random candidates and
// take the better one. Unsampled nodes (no throughput data yet) win ties so
// the whole set gets explored early; equal nodes tiebreak on fewer conns.
func pickTwo(candidates []*node) *node {
	if len(candidates) == 1 {
		return candidates[0]
	}
	i := rand.IntN(len(candidates))
	j := rand.IntN(len(candidates) - 1)
	if j >= i {
		j++
	}
	return betterNode(candidates[i], candidates[j])
}

func betterNode(a, b *node) *node {
	aNew, bNew := a.bytes == 0, b.bytes == 0
	switch {
	case aNew && !bNew:
		return a
	case bNew && !aNew:
		return b
	case aNew && bNew:
		if a.conns <= b.conns {
			return a
		}
		return b
	case a.bps == b.bps:
		if a.conns <= b.conns {
			return a
		}
		return b
	case a.bps > b.bps:
		return a
	default:
		return b
	}
}

// refresh re-resolves the host and merges the result into the node list.
// The DNS lookup deliberately runs outside p.mu: observe/shouldCull take that
// mutex on every body read, so a slow resolver must never block data flow.
func (p *picker) refresh(ctx context.Context) error {
	addrs, err := p.resolve(ctx, p.host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", p.host, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resolvedAt = time.Now()
	known := make(map[netip.Addr]*node, len(p.nodes))
	for _, n := range p.nodes {
		known[n.addr] = n
	}
	nodes := make([]*node, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if n, ok := known[a]; ok {
			nodes = append(nodes, n)
			delete(known, a)
		} else {
			nodes = append(nodes, &node{addr: a})
		}
	}
	if len(nodes) > 0 {
		p.nodes = nodes
	}
	return nil
}

// observe folds one read's throughput into the node's EWMA.
func (p *picker) observe(n *node, bytes int64, d time.Duration) {
	if n == nil || d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n.bytes += bytes
	rate := float64(bytes) / d.Seconds()
	if n.bps == 0 {
		n.bps = rate
	} else {
		n.bps = n.bps*(1-ewmaAlpha) + rate*ewmaAlpha
	}
}

// shouldCull reports whether n is, after warmup, statistically much slower
// than the best node and there is somewhere better to go.
func (p *picker) shouldCull(n *node) bool {
	if n == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.nodes) < 2 || n.bytes < cullWarmupBytes || n.bps == 0 {
		return false
	}
	now := time.Now()
	var best float64
	for _, o := range p.nodes {
		if o != n && now.After(o.banUntil) && o.bps > best {
			best = o.bps
		}
	}
	return best > 0 && n.bps < best*cullRatio
}

// strike records a failure (stall, cull, range-ignored response) against a
// node; repeat offenders are banned for banDuration.
func (p *picker) strike(n *node) {
	if n == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n.strikes++
	if n.strikes >= strikesToBan {
		n.banUntil = time.Now().Add(banDuration)
		n.strikes = 0
		// Reset stats: after the ban the node deserves a fresh look.
		n.bps = 0
		n.bytes = 0
		p.log.Debug("node banned", "addr", n.addr, "until", n.banUntil)
	}
}

// release drops a worker's pin on n.
func (p *picker) release(n *node) {
	if n == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n.conns--
}
