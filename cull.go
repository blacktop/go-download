package download

import (
	"cmp"
	"context"
	"net/http"
	"net/netip"
	"slices"
	"time"
)

type placementWorkerSample struct {
	addr  netip.Addr
	gen   uint64
	bytes int64
	tail  bool
}

type placementSnapshot struct {
	at            time.Time
	membershipGen uint64
	workers       map[int]placementWorkerSample
}

type nodeWindowRate struct {
	addr       netip.Addr
	bytes      int64
	conns      int
	perConnBPS float64
	tailsOnly  bool
}

// snapshot samples every placed worker's address, generation, and byte
// counter at p.now(); tailOf reports whether a worker is only draining a tail
// too small to judge.
func (p *nodePlacement) snapshot(tailOf func(id int) bool) placementSnapshot {
	p.mu.Lock()
	snapshot := placementSnapshot{
		at:            p.now(),
		membershipGen: p.membershipGen,
		workers:       make(map[int]placementWorkerSample, len(p.workers)),
	}
	for id, worker := range p.workers {
		counter := worker.counter.Load()
		if counter == nil {
			continue
		}
		snapshot.workers[id] = placementWorkerSample{
			addr: worker.addr, gen: counter.generation, bytes: counter.bytes.Load(),
		}
	}
	p.mu.Unlock()
	for id, worker := range snapshot.workers {
		worker.tail = tailOf(id)
		snapshot.workers[id] = worker
	}
	return snapshot
}

func (p *nodePlacement) startCuller(
	ctx context.Context, sched *scheduler, ramp *rampState,
) {
	cullCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	p.mu.Lock()
	if p.closed || p.samplerDone != nil {
		p.mu.Unlock()
		cancel()
		return
	}
	p.samplerCancel = cancel
	p.samplerDone = done
	p.mu.Unlock()
	tailOf := func(id int) bool { return sched.workerRemaining(id) < 2*sched.minSize }
	go func() {
		defer close(done)
		p.sampleLoop(cullCtx, tailOf, ramp)
	}()
}

func (p *nodePlacement) sampleLoop(ctx context.Context, tailOf func(int) bool, ramp *rampState) {
	ticker := time.NewTicker(nodeSampleInterval)
	defer ticker.Stop()
	history := []placementSnapshot{p.snapshot(tailOf)}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := p.snapshot(tailOf)
			history = append(history, current)
			// The newest snapshot at least one window old starts the trailing
			// window; anything older can never be selected again.
			for i := len(history) - 2; i >= 0; i-- {
				if current.at.Sub(history[i].at) >= nodeSampleWindow {
					p.evaluateWindow(history[i], current, ramp.done.Load())
					history = history[i:]
					break
				}
			}
		}
	}
}

func (p *nodePlacement) resetCullEvidence() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, node := range p.ordered {
		node.belowSample = 0
		node.healthySample = 0
	}
}

func nodeRates(start, end placementSnapshot) ([]nodeWindowRate, bool) {
	if start.membershipGen != end.membershipGen || len(start.workers) != len(end.workers) {
		return nil, false
	}
	elapsed := end.at.Sub(start.at)
	if elapsed <= 0 {
		return nil, false
	}
	type aggregate struct {
		bytes     int64
		conns     int
		tailsOnly bool
	}
	byAddr := make(map[netip.Addr]*aggregate)
	for id, current := range end.workers {
		previous, ok := start.workers[id]
		if !ok || !current.addr.IsValid() || current.addr != previous.addr ||
			current.gen != previous.gen || current.bytes < previous.bytes {
			return nil, false
		}
		agg := byAddr[current.addr]
		if agg == nil {
			agg = &aggregate{tailsOnly: true}
			byAddr[current.addr] = agg
		}
		agg.bytes += current.bytes - previous.bytes
		agg.conns++
		agg.tailsOnly = agg.tailsOnly && current.tail
	}
	rates := make([]nodeWindowRate, 0, len(byAddr))
	for addr, agg := range byAddr {
		rate := float64(agg.bytes) / elapsed.Seconds() / float64(max(agg.conns, 1))
		rates = append(rates, nodeWindowRate{
			addr: addr, bytes: agg.bytes, conns: agg.conns,
			perConnBPS: rate, tailsOnly: agg.tailsOnly,
		})
	}
	slices.SortFunc(rates, func(a, b nodeWindowRate) int {
		if c := cmp.Compare(a.perConnBPS, b.perConnBPS); c != 0 {
			return c
		}
		return a.addr.Compare(b.addr)
	})
	return rates, true
}

// bestWindowRate is the fastest per-connection address with warm-up evidence
// (at least nodeCullWarmupBytes in the window); the candidate side needs only
// a complete, stable window, so an address too slow to deliver the warm-up
// bytes is still judged.
func bestWindowRate(rates []nodeWindowRate) (nodeWindowRate, bool) {
	var best nodeWindowRate
	found := false
	for _, rate := range rates {
		if rate.bytes >= nodeCullWarmupBytes && (!found || rate.perConnBPS > best.perConnBPS) {
			best, found = rate, true
		}
	}
	return best, found && best.perConnBPS > 0
}

// evaluateWindow applies policy to one complete, stable trailing window:
// judgment and state mutation happen under a single lock hold, and the
// resulting cancellations run after it is released.
func (p *nodePlacement) evaluateWindow(start, end placementSnapshot, rampFinished bool) {
	rates, stable := nodeRates(start, end)
	if !rampFinished || !stable || len(rates) < 2 {
		p.resetCullEvidence()
		return
	}
	for _, rate := range rates {
		p.d.log.Debug("node throughput sample",
			"address", rate.addr, "window_bytes", rate.bytes,
			"connections", rate.conns, "per_connection_bps", rate.perConnBPS,
			"tails_only", rate.tailsOnly)
	}
	best, ok := bestWindowRate(rates)
	if !ok {
		p.resetCullEvidence()
		return
	}

	p.mu.Lock()
	var candidate *nodeAddress
	var candidateRate float64
	for _, rate := range rates {
		node := p.byAddr[rate.addr]
		if node == nil || rate.addr == best.addr {
			continue
		}
		if p.judgeLocked(node, rate, best) && (candidate == nil || rate.perConnBPS < candidateRate) {
			candidate, candidateRate = node, rate.perConnBPS
		}
	}
	if candidate == nil || end.at.Sub(p.lastCull) < nodeCullCooldown {
		p.mu.Unlock()
		return
	}
	cancels, transports, banned := p.cullLocked(candidate, p.byAddr[best.addr], end.at)
	p.mu.Unlock()
	for _, tr := range transports {
		tr.CloseIdleConnections()
	}
	for _, cancel := range cancels {
		cancel(errSlowNode)
	}
	p.d.log.Debug("node culled", "address", candidate.addr, "per_connection_bps", candidateRate,
		"best_per_connection_bps", best.perConnBPS, "banned", banned)
}

// judgeLocked updates one address's evidence for this window and reports
// whether it has accumulated enough consecutive slow windows to be culled. A
// probing address that samples healthy twice is restored instead.
func (p *nodePlacement) judgeLocked(node *nodeAddress, rate, best nodeWindowRate) bool {
	healthy := rate.perConnBPS >= best.perConnBPS*nodeCullRatio
	if node.probing() {
		if !healthy {
			node.healthySample = 0
		} else {
			node.healthySample++
			node.belowSample = 0
			if node.healthySample >= nodeCullEvaluations {
				node.slowStrikes, node.healthySample = 0, 0
				p.d.log.Debug("node restored after passive probe", "address", node.addr)
			}
			return false
		}
	}
	if healthy || rate.tailsOnly {
		node.belowSample = 0
		return false
	}
	node.belowSample++
	return node.belowSample >= nodeCullEvaluations
}

// cullLocked records the strike and parks or bans the address, then collects
// the cancels and transports of every worker on it. The healthy alternative
// must be live and eligible; otherwise there is nowhere to move the work.
func (p *nodePlacement) cullLocked(
	node, alternative *nodeAddress, now time.Time,
) (cancels []context.CancelCauseFunc, transports []*http.Transport, banned bool) {
	if node.conns == 0 || alternative == nil || alternative == node ||
		alternative.conns == 0 || alternative.slowStrikes != 0 ||
		!p.eligibleLocked(alternative, now) {
		return nil, nil, false
	}
	node.slowStrikes++
	node.belowSample, node.healthySample = 0, 0
	if !node.banned() {
		node.unavailableUntil = now.Add(nodeCullCooldown)
	}
	p.lastCull = now
	for id, worker := range p.workers {
		if worker.addr != node.addr {
			continue
		}
		worker.rotate.Store(true)
		if worker.cancel != nil {
			cancels = append(cancels, worker.cancel)
		}
		if tr := p.transports[id]; tr != nil {
			transports = append(transports, tr)
		}
	}
	return cancels, transports, node.banned()
}
