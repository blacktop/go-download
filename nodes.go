package download

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	preferredFallbackDelay   = 300 * time.Millisecond
	nodeResolutionTimeout    = 2 * time.Second
	nodeAvailabilityCooldown = 30 * time.Second
	nodeCullRatio            = 0.25
	nodeCullEvaluations      = 2
)

// Test seams retain fixed production defaults without exposing tuning knobs.
var (
	nodeFallbackDelay         = preferredFallbackDelay
	nodeSampleInterval        = 5 * time.Second
	nodeSampleWindow          = 10 * time.Second
	nodeCullWarmupBytes int64 = 8 << 20
	nodeCullCooldown          = 30 * time.Second
)

type nodeAddress struct {
	addr  netip.Addr
	order int
	conns int
	// unavailableUntil is the single eligibility clock: dial/TLS failures and
	// slow-node culls both park the address here. Only a confirmed slow cull
	// increments slowStrikes, so availability failures never count as evidence.
	unavailableUntil time.Time
	slowStrikes      int
	healthySample    int
	belowSample      int
}

// banned reports a second confirmed slow cull: the address is out for the run.
func (n *nodeAddress) banned() bool { return n.slowStrikes >= 2 }

// probing reports a once-culled address carrying its single passive probe.
func (n *nodeAddress) probing() bool { return n.slowStrikes > 0 && n.conns > 0 }

type placedWorker struct {
	addr netip.Addr
	// counter is replaced whenever the actual connection generation changes.
	// Readers retain one pointer across Read, so a sample that races a rotation
	// is discarded without taking placement.mu or contaminating the new node.
	counter   atomic.Pointer[nodeByteCounter]
	nextGen   uint64
	avoidNext netip.Addr

	rotate atomic.Bool
	// cancel aborts the worker's in-flight attempt; attempts are sequential
	// per worker, so one slot suffices. Guarded by nodePlacement.mu.
	cancel context.CancelCauseFunc
}

type nodeByteCounter struct {
	generation uint64
	bytes      atomic.Int64
}

// nodePlacement is one multipart run's final-host address set. All selection
// and connection ownership fields are guarded by mu; byte counters and address
// generations are atomic so the five-second sampler does not serialize reads.
type nodePlacement struct {
	mu sync.Mutex

	d        *Downloader
	host     string
	port     string
	election netip.Addr
	// electionPending accounts for the already-open useful election connection
	// until its byte-zero owner attaches it to a concrete worker.
	electionPending bool
	ordered         []*nodeAddress
	byAddr          map[netip.Addr]*nodeAddress
	workers         map[int]*placedWorker

	tlsConfig  *tls.Config
	transports map[int]*http.Transport

	now    func() time.Time
	closed bool

	membershipGen uint64
	lastCull      time.Time

	samplerCancel context.CancelFunc
	samplerDone   chan struct{}
}

// placementInput is what a run knows when it decides whether to place
// connections: the byte-serving URL, how its election was routed and where
// it landed, and the effective concurrency.
type placementInput struct {
	url             string
	electionRemote  string
	electionProxied bool
	electionInUse   bool // the election body is still open for the byte-zero worker
	canMultiply     bool // the scheduler can feed more than one connection
	parts           int  // effective Parts after Options.Policy
}

// newNodePlacement enables placement only for an owned, direct, multipart
// HTTP/1.1 transport whose final redirected hostname has at least two usable
// addresses. Every inapplicable or discovery-failure path deliberately falls
// back to the existing base transport.
func (d *Downloader) newNodePlacement(ctx context.Context, in placementInput) *nodePlacement {
	if !d.opt.EnableNodeSelection || !in.canMultiply || d.base == nil {
		return nil
	}
	if in.electionProxied {
		// Decided on the real election request (headers, cookies, redirects),
		// never a synthetic preflight: a proxied election's remote is the
		// proxy, which must not seed the origin pool.
		d.log.Debug("node selection disabled for proxy route", "url", redactURL(in.url))
		return nil
	}
	u, err := url.Parse(in.url)
	if err != nil {
		return nil
	}
	host := NormalizeHost(u.Hostname())
	if host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, nodeResolutionTimeout)
	addrs, err := d.resolve(resolveCtx, host)
	cancelResolve()
	if err != nil {
		d.log.Debug("node selection disabled after resolver failure", "host", host, "err", err)
		return nil
	}
	election, _ := canonicalRemoteAddr(in.electionRemote)
	ordered := orderNodeAddresses(election, addrs)
	if len(ordered) < 2 {
		return nil
	}

	tlsConfig := &tls.Config{} // #nosec G402 -- defaults retain normal certificate verification
	if d.opt.TLSConfig != nil {
		tlsConfig = d.opt.TLSConfig.Clone()
	}
	if tlsConfig.ClientSessionCache == nil {
		tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(max(8, 2*in.parts))
	}

	p := &nodePlacement{
		d:               d,
		host:            host,
		port:            portOf(u),
		election:        election,
		electionPending: in.electionInUse && election.IsValid(),
		byAddr:          make(map[netip.Addr]*nodeAddress, len(ordered)),
		workers:         make(map[int]*placedWorker),
		tlsConfig:       tlsConfig,
		transports:      make(map[int]*http.Transport),
		now:             time.Now,
	}
	p.installOrderLocked(ordered)
	if p.electionPending {
		p.byAddr[election].conns++
	}
	d.registerPlacement(p)
	d.log.Debug("node selection enabled", "host", host, "addresses", addressStrings(ordered))
	return p
}

// NormalizeHost canonicalizes a hostname for comparison: lower-cased, with
// surrounding whitespace and one trailing dot removed. It is the form node
// selection uses to match dial targets and the one callers should use when
// classifying hosts for Options.Policy.
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func portOf(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return "443"
}

// canonicalRemoteAddr extracts and unmaps the IP from a net.Addr string
// ("ip:port" or "[ip]:port"). The returned display value keeps the port.
func canonicalRemoteAddr(remote string) (netip.Addr, string) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}, remote
	}
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	return ap.Addr(), ap.String()
}

// orderNodeAddresses puts the election address first, then its family, then
// the other family. A/AAAA answers are deduplicated and IPv4-mapped IPv6 is
// canonicalized to IPv4. The election address is always unioned even when it
// disappeared from the later DNS answer.
func orderNodeAddresses(election netip.Addr, resolved []netip.Addr) []netip.Addr {
	election = election.Unmap()
	seen := make(map[netip.Addr]struct{}, len(resolved)+1)
	ordered := make([]netip.Addr, 0, len(resolved)+1)
	appendAddr := func(addr netip.Addr) {
		addr = addr.Unmap()
		if !addr.IsValid() {
			return
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		ordered = append(ordered, addr)
	}
	appendAddr(election)
	for _, sameFamily := range []bool{true, false} {
		for _, addr := range resolved {
			addr = addr.Unmap()
			if election.IsValid() && (addr.Is4() == election.Is4()) != sameFamily {
				continue
			}
			appendAddr(addr)
		}
	}
	return ordered
}

func addressStrings(addrs []netip.Addr) []string {
	out := make([]string, len(addrs))
	for i, addr := range addrs {
		out[i] = addr.String()
	}
	return out
}

func (p *nodePlacement) installOrderLocked(addrs []netip.Addr) {
	ordered := make([]*nodeAddress, 0, len(addrs))
	for i, addr := range addrs {
		n := p.byAddr[addr]
		if n == nil {
			n = &nodeAddress{addr: addr}
			p.byAddr[addr] = n
		}
		n.order = i
		ordered = append(ordered, n)
	}
	p.ordered = ordered
}

func (p *nodePlacement) workerLocked(id int) *placedWorker {
	w := p.workers[id]
	if w == nil {
		w = &placedWorker{}
		w.counter.Store(&nodeByteCounter{})
		p.workers[id] = w
		p.membershipGen++
	}
	return w
}

func (p *nodePlacement) registerWorker(id int) *placedWorker {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workerLocked(id)
}

func (p *nodePlacement) eligibleLocked(n *nodeAddress, now time.Time) bool {
	return n != nil && !n.banned() && !now.Before(n.unavailableUntil)
}

func (p *nodePlacement) bestLocked(exclude netip.Addr) *nodeAddress {
	now := p.now()
	var chosen *nodeAddress
	for _, n := range p.ordered {
		if n.addr == exclude || !p.eligibleLocked(n, now) {
			continue
		}
		// A post-cull address receives one passive probe, not a batch.
		if n.probing() {
			continue
		}
		if chosen == nil || n.conns < chosen.conns ||
			(n.conns == chosen.conns && n.order < chosen.order) {
			chosen = n
		}
	}
	return chosen
}

func (p *nodePlacement) assignLocked(w *placedWorker, addr netip.Addr, forceGeneration bool) {
	if w.addr == addr && !forceGeneration {
		return
	}
	if w.addr != addr {
		if old := p.byAddr[w.addr]; old != nil && old.conns > 0 {
			old.conns--
		}
		// Pool membership is established only by resolution (installOrderLocked);
		// assignment never admits a new address.
		if n := p.byAddr[addr]; n != nil {
			n.conns++
		}
	}
	w.addr = addr
	w.nextGen++
	if addr.IsValid() {
		w.counter.Store(&nodeByteCounter{generation: w.nextGen})
	} else {
		w.counter.Store(nil) // nothing to credit while unplaced
	}
	p.membershipGen++
}

func (p *nodePlacement) reserve(id int) (netip.Addr, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return netip.Addr{}, errors.New("node placement closed")
	}
	w := p.workerLocked(id)
	if w.addr.IsValid() {
		return w.addr, nil
	}
	n := p.bestLocked(w.avoidNext)
	if n == nil && w.avoidNext.IsValid() {
		// A single-address run must still make progress. The exclusion is only a
		// per-worker preference for the next reservation, never a global ban.
		n = p.bestLocked(netip.Addr{})
	}
	w.avoidNext = netip.Addr{}
	if n == nil {
		return netip.Addr{}, fmt.Errorf("no eligible address for %s", p.host)
	}
	p.assignLocked(w, n.addr, false)
	return n.addr, nil
}

func (p *nodePlacement) avoidCurrentOnNextReservation(w *placedWorker) {
	if p == nil || w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w.avoidNext = w.addr
}

func (p *nodePlacement) fallback(primary netip.Addr) netip.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return netip.Addr{}
	}
	if n := p.bestLocked(primary); n != nil {
		return n.addr
	}
	return netip.Addr{}
}

func (p *nodePlacement) markAvailabilityFailure(addr netip.Addr) {
	if p == nil || !addr.IsValid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.byAddr[addr]
	if n == nil {
		return
	}
	n.unavailableUntil = p.now().Add(nodeAvailabilityCooldown)
}

func (p *nodePlacement) markDialWinner(id int, addr netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !addr.IsValid() {
		return
	}
	// A successful dial is a new actual connection even when it reaches the
	// same logical address as the reservation it replaces.
	p.assignLocked(p.workerLocked(id), addr, true)
}

// gotConn attributes a worker's live connection to its actual remote
// address. Only members of the origin pool are attributed: net/http fires the
// same hook for a cross-host redirect, and that remote must never become a
// candidate the origin's Host and SNI are later dialed against. A worker on a
// foreign remote releases its pool address instead, so its bytes are not
// credited to an address it is not using.
func (p *nodePlacement) gotConn(id int, remote string) string {
	addr, canonical := canonicalRemoteAddr(remote)
	if !addr.IsValid() {
		return canonical
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return canonical
	}
	w := p.workerLocked(id)
	if p.byAddr[addr] == nil {
		p.d.log.Debug("connection remote outside origin pool; not attributed",
			"worker", id, "remote", canonical, "host", p.host)
		p.assignLocked(w, netip.Addr{}, false)
		return canonical
	}
	p.assignLocked(w, addr, false)
	return canonical
}

func (p *nodePlacement) attachInitial(id int, remote string) string {
	addr, canonical := canonicalRemoteAddr(remote)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.electionPending {
		if election := p.byAddr[p.election]; election != nil && election.conns > 0 {
			election.conns--
		}
		p.electionPending = false
	}
	// Transfer the provisional election reservation to its actual worker
	// atomically. Otherwise another worker can observe both the election node
	// and an unused node at zero load in the gap and select the election node.
	if !p.closed && addr.IsValid() {
		p.assignLocked(p.workerLocked(id), addr, false)
	}
	return canonical
}

func (p *nodePlacement) currentAddress(w *placedWorker) netip.Addr {
	if p == nil || w == nil {
		return netip.Addr{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return w.addr
}

// beginAttempt registers the attempt's cancel so a cull can abort it, and
// honours a rotation flagged before registration by cancelling immediately.
// Both happen under one lock hold, so a concurrent cull either sees the
// rotation consumed here or finds the cancel it needs. The returned func ends
// the attempt.
func (p *nodePlacement) beginAttempt(w *placedWorker, cancel context.CancelCauseFunc) func() {
	if p == nil || w == nil {
		return func() {}
	}
	p.mu.Lock()
	w.cancel = cancel
	rotate := w.rotate.Swap(false)
	p.mu.Unlock()
	if rotate {
		cancel(errSlowNode)
	}
	return func() {
		p.mu.Lock()
		w.cancel = nil
		p.mu.Unlock()
	}
}

func (p *nodePlacement) releaseAddress(w *placedWorker) {
	if p == nil || w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if w.addr.IsValid() {
		p.assignLocked(w, netip.Addr{}, false)
	}
}

func (p *nodePlacement) removeWorker(id int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.workers[id]
	if w == nil {
		return
	}
	if n := p.byAddr[w.addr]; n != nil && n.conns > 0 {
		n.conns--
	}
	delete(p.workers, id)
	p.membershipGen++
}

func (p *nodePlacement) createTransport(id int, primary netip.Addr) *http.Transport {
	tr := p.d.base.Clone()
	// One shared config: the transport clones it per connection and an
	// HTTP/1.1-only transport never mutates it, so the session cache is shared
	// without a copy per worker.
	tr.TLSClientConfig = p.tlsConfig
	tr.MaxConnsPerHost = 1
	tr.MaxIdleConnsPerHost = 1
	tr.MaxIdleConns = 1
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !p.matchesTarget(addr) {
			return p.d.dial(ctx, network, addr)
		}
		return p.dialPreferred(ctx, id, network, primary)
	}
	p.closeTransport(id)
	p.mu.Lock()
	p.transports[id] = tr
	p.mu.Unlock()
	return tr
}

func (p *nodePlacement) matchesTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	return err == nil && NormalizeHost(host) == p.host && port == p.port
}

func (p *nodePlacement) closeTransport(id int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	tr := p.transports[id]
	delete(p.transports, id)
	p.mu.Unlock()
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

// closeIdleConnections drops idle connections on every live pinned transport
// (Downloader.CloseIdleConnections reaches in-flight runs through it).
func (p *nodePlacement) closeIdleConnections() {
	p.mu.Lock()
	transports := slices.Collect(maps.Values(p.transports))
	p.mu.Unlock()
	for _, tr := range transports {
		tr.CloseIdleConnections()
	}
}

func (p *nodePlacement) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	samplerCancel, samplerDone := p.samplerCancel, p.samplerDone
	transports := slices.Collect(maps.Values(p.transports))
	clear(p.transports)
	p.mu.Unlock()
	if samplerCancel != nil {
		samplerCancel()
	}
	if samplerDone != nil {
		<-samplerDone
	}
	for _, tr := range transports {
		tr.CloseIdleConnections()
	}
	p.d.unregisterPlacement(p)
}

func (p *nodePlacement) refresh(ctx context.Context) {
	resolveCtx, cancelResolve := context.WithTimeout(ctx, nodeResolutionTimeout)
	addrs, err := p.d.resolve(resolveCtx, p.host)
	cancelResolve()
	if err != nil {
		p.d.log.Debug("node re-resolution failed", "host", p.host, "err", err)
		return
	}
	ordered := orderNodeAddresses(p.election, addrs)
	if len(ordered) == 0 {
		return
	}
	p.mu.Lock()
	p.installOrderLocked(ordered)
	p.mu.Unlock()
}

func (p *nodePlacement) dialPreferred(
	ctx context.Context, id int, network string, primary netip.Addr,
) (net.Conn, error) {
	fallback := p.fallback(primary)
	outcome := dialPreferred(ctx, network,
		joinAddrPort(primary, p.port), joinAddrPort(fallback, p.port), nodeFallbackDelay, p.d.dial)
	for _, failed := range outcome.failed {
		if addr, _ := canonicalRemoteAddr(failed); addr.IsValid() {
			p.markAvailabilityFailure(addr)
		}
	}
	if outcome.preferredLost {
		p.markAvailabilityFailure(primary)
	}
	if outcome.err != nil {
		attempted := make([]netip.Addr, 0, len(outcome.failed))
		for _, failed := range outcome.failed {
			if addr, _ := canonicalRemoteAddr(failed); addr.IsValid() && !slices.Contains(attempted, addr) {
				attempted = append(attempted, addr)
			}
		}
		return nil, &nodeRotationError{addrs: attempted,
			err: fmt.Errorf("%w: %w", errNodeUnavailable, outcome.err)}
	}
	winner, _ := canonicalRemoteAddr(outcome.winner)
	p.markDialWinner(id, winner)
	return outcome.conn, nil
}

func joinAddrPort(addr netip.Addr, port string) string {
	if !addr.IsValid() {
		return ""
	}
	return net.JoinHostPort(addr.String(), port)
}
