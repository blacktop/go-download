package download

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrderNodeAddressesUnionsElectionAndPrefersItsFamily(t *testing.T) {
	t.Parallel()
	election := netip.MustParseAddr("2001:db8::9")
	got := orderNodeAddresses(election, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("2001:db8::2"),
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("::ffff:192.0.2.1"),
	})
	want := []netip.Addr{
		election,
		netip.MustParseAddr("2001:db8::2"),
		netip.MustParseAddr("192.0.2.1"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ordered addresses = %v, want %v", got, want)
	}
}

func TestOrderNodeAddressesKeepsElectionMissingFromDNS(t *testing.T) {
	t.Parallel()
	election := netip.MustParseAddr("192.0.2.10")
	got := orderNodeAddresses(election, []netip.Addr{
		netip.MustParseAddr("192.0.2.20"),
		netip.MustParseAddr("2001:db8::20"),
	})
	if len(got) != 3 || got[0] != election {
		t.Fatalf("ordered addresses = %v, want missing election address first", got)
	}
}

type trackedConn struct {
	net.Conn
	closed atomic.Bool
}

type logicalRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c *logicalRemoteConn) RemoteAddr() net.Addr { return c.remote }

type nodeAddressReporter struct {
	NopReporter
	mu      sync.Mutex
	addrs   map[string]int
	retries int
}

type byteCountingResponseWriter struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w *byteCountingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes.Add(int64(n))
	return n, err
}

func (w *byteCountingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *nodeAddressReporter) Connected(_ int, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.addrs == nil {
		r.addrs = make(map[string]int)
	}
	r.addrs[addr]++
}

func (r *nodeAddressReporter) ChunkRetry(_ int, _ int, _ error) {
	r.mu.Lock()
	r.retries++
	r.mu.Unlock()
}

func (r *nodeAddressReporter) snapshot() (map[string]int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.addrs), r.retries
}

func (c *trackedConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func pipeConn(t *testing.T) (*trackedConn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	return &trackedConn{Conn: client}, server
}

func selfSignedCertificate(t *testing.T, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestDialPreferredStartsFallbackAfterPendingDelay(t *testing.T) {
	t.Parallel()
	if preferredFallbackDelay != 300*time.Millisecond {
		t.Fatalf("production fallback delay = %v, want 300ms", preferredFallbackDelay)
	}
	const testDelay = 25 * time.Millisecond
	fallbackConn, _ := pipeConn(t)
	started := time.Now()
	outcome := dialPreferred(t.Context(), "tcp", "[2001:db8::1]:443", "192.0.2.1:443",
		testDelay, func(ctx context.Context, _, addr string) (net.Conn, error) {
			if addr == "[2001:db8::1]:443" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return fallbackConn, nil
		})
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	defer outcome.conn.Close()
	if outcome.winner != "192.0.2.1:443" || !outcome.preferredLost {
		t.Fatalf("outcome = %+v, want fallback winner", outcome)
	}
	if elapsed := time.Since(started); elapsed < testDelay || elapsed > 10*testDelay {
		t.Fatalf("fallback elapsed = %v, want [%v,%v]", elapsed, testDelay, 10*testDelay)
	}
}

func TestDialPreferredHardFailureFallsBackImmediately(t *testing.T) {
	t.Parallel()
	fallbackConn, _ := pipeConn(t)
	started := time.Now()
	outcome := dialPreferred(t.Context(), "tcp", "192.0.2.1:443", "192.0.2.2:443",
		time.Second, func(_ context.Context, _, addr string) (net.Conn, error) {
			if addr == "192.0.2.1:443" {
				return nil, errors.New("hard failure")
			}
			return fallbackConn, nil
		})
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	defer outcome.conn.Close()
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("hard-failure fallback waited %v", elapsed)
	}
	if len(outcome.failed) != 1 || outcome.failed[0] != "192.0.2.1:443" {
		t.Fatalf("failed addresses = %v", outcome.failed)
	}
}

func TestDialPreferredClosesRaceLoser(t *testing.T) {
	t.Parallel()
	primaryConn, _ := pipeConn(t)
	fallbackConn, _ := pipeConn(t)
	releasePrimary := make(chan struct{})
	outcome := dialPreferred(t.Context(), "tcp", "192.0.2.1:443", "192.0.2.2:443",
		5*time.Millisecond, func(_ context.Context, _, addr string) (net.Conn, error) {
			if addr == "192.0.2.1:443" {
				<-releasePrimary // deliberately ignores cancellation
				return primaryConn, nil
			}
			return fallbackConn, nil
		})
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	defer outcome.conn.Close()
	close(releasePrimary)
	deadline := time.Now().Add(time.Second)
	for !primaryConn.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !primaryConn.closed.Load() {
		t.Fatal("race-losing preferred connection was not closed")
	}
}

func newPlacementForTest(t *testing.T, opt *Options, resolved []netip.Addr) (*Downloader, *nodePlacement) {
	t.Helper()
	return newPlacementForTestWithParts(t, opt, 0, resolved)
}

// newPlacementForTestWithParts is newPlacementForTest with an effective-Parts
// override (0 means Options.Parts), as a Policy would produce.
func newPlacementForTestWithParts(
	t *testing.T, opt *Options, parts int, resolved []netip.Addr,
) (*Downloader, *nodePlacement) {
	t.Helper()
	configured := *opt
	configured.EnableNodeSelection = true
	d, err := New(&configured)
	if err != nil {
		t.Fatal(err)
	}
	placeAddresses(d, resolved...)
	if parts == 0 {
		parts = d.opt.Parts
	}
	p := d.newNodePlacement(t.Context(), placementInput{
		url: "https://cdn.test/file", electionRemote: "192.0.2.1:443", canMultiply: true,
		parts: parts,
	})
	if p == nil {
		t.Fatal("eligible direct transport did not initialize placement")
	}
	t.Cleanup(p.close)
	return d, p
}

// placeAddresses disables environment proxies and pins the resolver to addrs,
// the two seams every placement fixture sets.
func placeAddresses(d *Downloader, addrs ...netip.Addr) {
	if d.base != nil {
		d.base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	d.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return slices.Clone(addrs), nil
	}
}

// logicalDialer maps a logical "addr:port" dial target to a real backend and
// labels the connection's remote address with the logical address so
// placement attributes it. Targets not in backends (the hostname election
// dial) use defaultLogical and its backend. onDial observes every target.
func logicalDialer(
	port uint16, defaultLogical netip.Addr, backends map[netip.Addr]string, onDial func(target string),
) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if onDial != nil {
			onDial(addr)
		}
		logical := defaultLogical
		if ap, err := netip.ParseAddrPort(addr); err == nil {
			if _, ok := backends[ap.Addr()]; ok {
				logical = ap.Addr()
			}
		}
		conn, err := dialer.DialContext(ctx, network, backends[logical])
		if err != nil {
			return nil, err
		}
		return &logicalRemoteConn{
			Conn: conn, remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(logical, port)),
		}, nil
	}
}

func TestFallbackWinnerOwnsReservationBytesAndGeneration(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	primary, err := p.reserve(7)
	if err != nil {
		t.Fatal(err)
	}
	if primary != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("primary = %v, want election address", primary)
	}
	before := workerCounter(p, 7)
	p.markDialWinner(7, netip.MustParseAddr("192.0.2.2"))
	canonical := p.gotConn(7, "192.0.2.2:443")
	after := workerCounter(p, 7)
	if canonical != "192.0.2.2:443" || after == before || after.generation <= before.generation {
		t.Fatalf("canonical=%q generation %d->%d", canonical, before.generation, after.generation)
	}
	before.bytes.Add(1 << 20) // a reader holding the stale counter: not credited to B
	after.bytes.Add(2 << 20)
	p.mu.Lock()
	w := p.workers[7]
	a := p.byAddr[netip.MustParseAddr("192.0.2.1")]
	b := p.byAddr[netip.MustParseAddr("192.0.2.2")]
	p.mu.Unlock()
	if w == nil || w.addr != b.addr || workerCounter(p, 7).bytes.Load() != 2<<20 {
		t.Fatalf("fallback accounting worker=%+v", w)
	}
	if a.conns != 0 || b.conns != 1 {
		t.Fatalf("loads after fallback: A=%d B=%d, want 0/1", a.conns, b.conns)
	}
}

func TestObservedReaderDoesNotTakePlacementLock(t *testing.T) {
	t.Parallel()
	d, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	placed := p.registerWorker(11)
	if _, err := p.reserve(11); err != nil {
		t.Fatal(err)
	}
	w := &worker{id: 11, r: &run{d: d}, place: p, placed: placed}
	reader := &observedReader{r: bytes.NewReader([]byte("payload")), w: w}
	done := make(chan error, 1)
	p.mu.Lock()
	go func() {
		buf := make([]byte, 7)
		_, err := reader.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		p.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		p.mu.Unlock()
		t.Fatal("observed read blocked on placement mutex")
	}
}

func TestStallRotatesWorkerWithoutCoolingAddress(t *testing.T) {
	t.Parallel()
	d, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	first, err := p.reserve(12)
	if err != nil {
		t.Fatal(err)
	}
	w := &worker{id: 12, r: &run{d: d}, place: p, placed: p.workers[12], timeout: d.opt.Timeout}
	actx, cancel := context.WithCancelCause(t.Context())
	cancel(errStall)
	if got := w.classify(errors.New("stalled read"), actx); got != errStall { //nolint:errorlint
		t.Fatalf("classified stall = %v, want errStall", got)
	}
	p.mu.Lock()
	cooled := p.byAddr[first].unavailableUntil
	strikes := p.byAddr[first].slowStrikes
	p.mu.Unlock()
	if !cooled.IsZero() || strikes != 0 {
		t.Fatalf("stall changed address policy: cooldown=%v strikes=%d", cooled, strikes)
	}
	second, err := p.reserve(12)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("first post-stall reservation stayed on %v", first)
	}
	p.releaseAddress(p.workers[12])
	third, err := p.reserve(12)
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("one-shot exclusion persisted: got %v, want %v", third, first)
	}
}

func TestPreferredRaceLossCreatesAvailabilityCooldownWithoutSlowStrike(t *testing.T) {
	d, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	oldDelay := nodeFallbackDelay
	nodeFallbackDelay = 5 * time.Millisecond
	t.Cleanup(func() { nodeFallbackDelay = oldDelay })
	d.dial = func(ctx context.Context, _, addr string) (net.Conn, error) {
		if addr == "192.0.2.1:443" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		conn, _ := pipeConn(t)
		return conn, nil
	}
	primary, err := p.reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := p.dialPreferred(t.Context(), 0, "tcp", primary)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p.mu.Lock()
	a := p.byAddr[netip.MustParseAddr("192.0.2.1")]
	b := p.byAddr[netip.MustParseAddr("192.0.2.2")]
	p.mu.Unlock()
	if !a.unavailableUntil.After(time.Now()) || a.slowStrikes != 0 {
		t.Fatalf("preferred state after race loss = %+v", a)
	}
	if b.conns != 1 || p.currentAddress(p.workers[0]) != b.addr {
		t.Fatalf("fallback did not own load: B=%+v current=%v", b, p.currentAddress(p.workers[0]))
	}
}

func TestNodePlacementSharesTLSCacheAndRegistersClones(t *testing.T) {
	t.Parallel()
	callerTLS := &tls.Config{MinVersion: tls.VersionTLS12}
	d, p := newPlacementForTest(t, &Options{Parts: 2, TLSConfig: callerTLS}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	if callerTLS.ClientSessionCache != nil {
		t.Fatal("caller TLS configuration was mutated")
	}
	a, err := p.reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	trA := p.createTransport(0, a)
	trB := p.createTransport(1, b)
	if trA.TLSClientConfig == callerTLS || trB.TLSClientConfig == callerTLS {
		t.Fatal("worker transport reused caller TLSConfig pointer")
	}
	if trA.TLSClientConfig.ClientSessionCache == nil ||
		trA.TLSClientConfig.ClientSessionCache != trB.TLSClientConfig.ClientSessionCache {
		t.Fatal("worker transports do not share a bounded session cache")
	}
	if active := pinnedTransports(d); active != 2 {
		t.Fatalf("active pinned transports = %d, want 2", active)
	}
	p.closeTransport(0)
	p.closeTransport(1)
	if active := pinnedTransports(d); active != 0 {
		t.Fatalf("active pinned transports after close = %d", active)
	}
}

func TestNodePlacementPreservesCallerSessionCache(t *testing.T) {
	t.Parallel()
	cache := tls.NewLRUClientSessionCache(4)
	_, p := newPlacementForTest(t, &Options{Parts: 2,
		TLSConfig: &tls.Config{ClientSessionCache: cache}}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	if p.tlsConfig.ClientSessionCache != cache {
		t.Fatal("existing ClientSessionCache was replaced")
	}
}

func TestInvalidCertificateAddressRotatesWithoutWeakeningVerification(t *testing.T) {
	t.Parallel()
	data := testData(8 << 20)
	var trustedStats, invalidStats stats
	trusted := httptest.NewTLSServer(throttledRangeHandler(data, `"v1"`, &trustedStats,
		2*time.Millisecond, 32<<10, func(*http.Request) bool { return true }))
	t.Cleanup(trusted.Close)
	invalid := httptest.NewUnstartedServer(throttledRangeHandler(data, `"v1"`, &invalidStats,
		2*time.Millisecond, 32<<10, func(*http.Request) bool { return true }))
	invalid.TLS = &tls.Config{
		Certificates: []tls.Certificate{selfSignedCertificate(t, "invalid.example")},
		MinVersion:   tls.VersionTLS12,
	}
	invalid.Config.ErrorLog = log.New(io.Discard, "", 0)
	invalid.StartTLS()
	t.Cleanup(invalid.Close)

	cert := trusted.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	serverName := "example.com"
	if len(cert.DNSNames) > 0 {
		serverName = cert.DNSNames[0]
	} else if len(cert.IPAddresses) > 0 {
		serverName = cert.IPAddresses[0].String()
	}
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")
	reporter := &nodeAddressReporter{}
	d := newDL(t, &Options{
		Parts: 2, MinParts: 2, MinPartSize: 64 << 10,
		EnableNodeSelection: true,
		TLSConfig:           &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12},
		Reporter:            reporter,
	})
	d.base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	d.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{a, b}, nil
	}
	trustedBackend := strings.TrimPrefix(trusted.URL, "https://")
	invalidBackend := strings.TrimPrefix(invalid.URL, "https://")
	var invalidDials atomic.Int64
	d.dial = logicalDialer(443, a,
		map[netip.Addr]string{a: trustedBackend, b: invalidBackend},
		func(target string) {
			if target == net.JoinHostPort(b.String(), "443") {
				invalidDials.Add(1)
			}
		})
	dest := filepath.Join(t.TempDir(), "file.bin")
	_, got := mustGet(t, d, "https://cdn.test/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("TLS rotation installed non-identical bytes")
	}
	if invalidDials.Load() == 0 {
		t.Fatal("invalid-certificate address was never exercised")
	}
	if len(invalidStats.rangeHeaders()) != 0 {
		t.Fatalf("invalid-certificate server received HTTP requests: %v",
			invalidStats.rangeHeaders())
	}
	_, retries := reporter.snapshot()
	if retries != 0 {
		t.Fatalf("certificate-address rotation emitted %d ChunkRetry events", retries)
	}
}

func TestPreGotConnEstablishmentFailureRotatesImmediately(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	if _, err := p.reserve(0); err != nil {
		t.Fatal(err)
	}
	w := &worker{id: 0, place: p, placed: p.workers[0]}
	err := w.classifyConnection(io.EOF, t.Context(), false)
	rotation, ok := errors.AsType[*nodeRotationError](err)
	if !ok || len(rotation.addrs) != 1 || rotation.addrs[0] != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("pre-GotConn failure = %T %+v, want rotation from first address", err, err)
	}
	p.mu.Lock()
	failed := p.byAddr[netip.MustParseAddr("192.0.2.1")]
	p.mu.Unlock()
	if !failed.unavailableUntil.After(time.Now()) || failed.slowStrikes != 0 {
		t.Fatalf("establishment failure state = %+v", failed)
	}
}

func TestNodePlacementInapplicablePaths(t *testing.T) {
	t.Parallel()
	two := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
	tests := []struct {
		name     string
		opt      Options
		rawURL   string
		multiply bool
		// proxied marks the election as having been routed through a proxy,
		// which get() determines on the real election request.
		proxied bool
		setup   func(*Downloader)
	}{
		{name: "zero-value opt out", opt: Options{Parts: 2}, rawURL: "https://cdn.test/file", multiply: true},
		{name: "single stream scheduler", opt: Options{Parts: 2, EnableNodeSelection: true}, rawURL: "https://cdn.test/file"},
		{name: "literal ip", opt: Options{Parts: 2, EnableNodeSelection: true}, rawURL: "https://192.0.2.10/file", multiply: true},
		{name: "opaque transport", opt: Options{Parts: 2, EnableNodeSelection: true, Transport: http.DefaultTransport}, rawURL: "https://cdn.test/file", multiply: true},
		{name: "proxied election", opt: Options{Parts: 2, EnableNodeSelection: true},
			rawURL: "https://cdn.test/file", multiply: true, proxied: true},
		{name: "resolver failure", opt: Options{Parts: 2, EnableNodeSelection: true}, rawURL: "https://cdn.test/file", multiply: true,
			setup: func(d *Downloader) {
				d.resolve = func(context.Context, string) ([]netip.Addr, error) {
					return nil, errors.New("resolver unavailable")
				}
			}},
		{name: "one address", opt: Options{Parts: 2, EnableNodeSelection: true}, rawURL: "https://cdn.test/file", multiply: true,
			setup: func(d *Downloader) {
				d.resolve = func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := New(&tt.opt)
			if err != nil {
				t.Fatal(err)
			}
			if d.base != nil {
				d.base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
			}
			d.resolve = func(context.Context, string) ([]netip.Addr, error) {
				return slices.Clone(two), nil
			}
			if tt.setup != nil {
				tt.setup(d)
			}
			if p := d.newNodePlacement(t.Context(), placementInput{
				url: tt.rawURL, electionRemote: "192.0.2.1:443", canMultiply: tt.multiply,
				electionProxied: tt.proxied, parts: d.opt.Parts,
			}); p != nil {
				p.close()
				t.Fatal("inapplicable path initialized node placement")
			}
		})
	}
}

// TestProxyDecisionBindsToElectionRequest: the transport's wrapper records
// the routing decision for the request it actually saw — headers included —
// into the attached proxyRoute. (The refusal itself is covered by the
// "proxied election" inapplicable-path case and the end-to-end proxy test.)
func TestProxyDecisionBindsToElectionRequest(t *testing.T) {
	t.Parallel()
	d := newDL(t, &Options{Parts: 2, EnableNodeSelection: true,
		Headers: http.Header{"Authorization": []string{"Bearer token"}},
		Proxy: func(r *http.Request) (*url.URL, error) {
			if r.Header.Get("Authorization") != "" {
				return url.Parse("http://proxy.test")
			}
			return nil, nil
		}})
	proxyDecision := func(authenticated bool) bool {
		t.Helper()
		route := &proxyRoute{}
		req, err := http.NewRequest(http.MethodGet, "https://cdn.test/file", nil)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(context.WithValue(req.Context(), proxyRouteKey{}, route))
		if authenticated {
			applyHeaders(req, d.opt.Headers, req.URL)
		}
		if _, err := d.base.Proxy(req); err != nil {
			t.Fatal(err)
		}
		return route.proxied
	}
	if proxyDecision(false) {
		t.Fatal("headerless request must be judged direct by this proxy function")
	}
	if !proxyDecision(true) {
		t.Fatal("the authenticated election request must be judged proxied")
	}
}

// TestProxiedElectionNeverResolvesForPlacement drives get(): the election is
// routed through a real forward proxy by a header-keyed proxy function, so
// the run must refuse placement without ever consulting the resolver.
func TestProxiedElectionNeverResolvesForPlacement(t *testing.T) {
	t.Parallel()
	data := testData(512 << 10)
	var st stats
	origin := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(origin.Close)
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		// The client asks for a hostname URL; only the proxy knows the origin.
		out, err := http.NewRequestWithContext(r.Context(), r.Method, origin.URL+r.URL.Path, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A second decision for the same request deliberately disagrees, proving
	// get reads the transport's recorded answer instead of calling Proxy again.
	var proxyDecisions sync.Map
	d := newDL(t, &Options{Parts: 4, MinParts: 4, MinPartSize: 64 << 10, EnableNodeSelection: true,
		Headers: http.Header{"Authorization": []string{"Bearer token"}},
		Proxy: func(r *http.Request) (*url.URL, error) {
			if r.Header.Get("Authorization") != "" {
				if _, loaded := proxyDecisions.LoadOrStore(r, struct{}{}); loaded {
					return nil, nil
				}
				return proxyURL, nil
			}
			return nil, nil
		}})
	var resolves atomic.Int32
	d.resolve = func(context.Context, string) ([]netip.Addr, error) {
		resolves.Add(1)
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
	}
	dest := filepath.Join(t.TempDir(), "file.bin")
	_, got := mustGet(t, d, "http://cdn.test/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if proxied.Load() == 0 {
		t.Fatal("fixture did not route the election through the proxy")
	}
	if n := resolves.Load(); n != 0 {
		t.Fatalf("proxied election consulted the resolver %d times; placement must be refused", n)
	}
}

// TestPolicyRaisedPartsReachPlacement: the session cache is sized from the
// run's effective Parts, not the Options value.
func TestPolicyRaisedPartsReachPlacement(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTestWithParts(t, &Options{Parts: 2,
		Policy: func(string) Concurrency { return Concurrency{Parts: 16, MinParts: 16} },
	}, 16, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")})
	// Fill beyond the Options-sized cache (2*2 -> clamped to 8) and verify the
	// policy-sized cache (2*16 = 32) retains the earliest session.
	cache := p.tlsConfig.ClientSessionCache
	for i := range 20 {
		cache.Put(fmt.Sprintf("k%d", i), &tls.ClientSessionState{})
	}
	if _, ok := cache.Get("k0"); !ok {
		t.Fatal("session cache sized from Options.Parts, not the policy's Parts")
	}
}

// TestForeignRemoteNeverJoinsOriginPool: net/http fires GotConn for a
// cross-host redirect (or a proxy hop) with the same hook. That remote must
// not become an origin candidate, and the worker must stop being credited to
// the pool address it is no longer using.
func TestForeignRemoteNeverJoinsOriginPool(t *testing.T) {
	t.Parallel()
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	_, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{a, b})
	primary, err := p.reserve(3)
	if err != nil {
		t.Fatal(err)
	}
	if primary != a && primary != b {
		t.Fatalf("reserved %v, want a pool member", primary)
	}
	foreign := "198.51.100.9:443"
	if got := p.gotConn(3, foreign); got != foreign {
		t.Fatalf("gotConn display = %q, want %q", got, foreign)
	}
	p.mu.Lock()
	_, admitted := p.byAddr[netip.MustParseAddr("198.51.100.9")]
	members := len(p.ordered)
	p.mu.Unlock()
	if admitted || members != 2 {
		t.Fatalf("foreign remote joined the origin pool (admitted=%t members=%d)", admitted, members)
	}
	if cur := p.currentAddress(p.workers[3]); cur.IsValid() {
		t.Fatalf("worker on a foreign remote still attributed to %v", cur)
	}
	for i := range 8 {
		addr, err := p.reserve(10 + i)
		if err != nil {
			t.Fatal(err)
		}
		if addr != a && addr != b {
			t.Fatalf("reservation %d handed out non-member %v", i, addr)
		}
	}
}

func TestReusedForeignRemoteClearsOriginAttribution(t *testing.T) {
	t.Parallel()
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	d, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{a, b})
	if _, err := p.reserve(3); err != nil {
		t.Fatal(err)
	}
	reporter := &nodeAddressReporter{}
	w := &worker{
		id: 3, r: &run{d: d, rep: reporter}, place: p, placed: p.workers[3],
	}
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	foreign := "198.51.100.9:443"
	w.recordGotConn(7, httptrace.GotConnInfo{
		Conn: &logicalRemoteConn{
			Conn: client, remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(foreign)),
		},
		Reused: true,
	})
	if cur := p.currentAddress(w.placed); cur.IsValid() {
		t.Fatalf("worker on reused foreign connection still attributed to %v", cur)
	}
	if !w.gotConn.Load() {
		t.Fatal("reused connection did not record GotConn")
	}
	addrs, _ := reporter.snapshot()
	if addrs[foreign] != 1 {
		t.Fatalf("reported addresses = %v, want reused foreign remote once", addrs)
	}
}

func TestNodeResolverReceivesBoundedContext(t *testing.T) {
	t.Parallel()
	d, err := New(&Options{Parts: 2, EnableNodeSelection: true})
	if err != nil {
		t.Fatal(err)
	}
	d.base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	var deadline time.Time
	d.resolve = func(ctx context.Context, _ string) ([]netip.Addr, error) {
		deadline, _ = ctx.Deadline()
		return nil, errors.New("fixture resolver failure")
	}
	if p := d.newNodePlacement(t.Context(), placementInput{
		url: "https://cdn.test/file", electionRemote: "192.0.2.1:443", canMultiply: true, parts: 2,
	}); p != nil {
		p.close()
		t.Fatal("failed resolver initialized placement")
	}
	remaining := time.Until(deadline)
	if deadline.IsZero() || remaining <= 0 || remaining > nodeResolutionTimeout {
		t.Fatalf("resolver deadline remaining = %v, want (0, %v]", remaining, nodeResolutionTimeout)
	}
}

func TestSmallAndSingleStreamRunsNeverResolveForPlacement(t *testing.T) {
	t.Parallel()
	data := testData(1 << 20)
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{name: "small ranged", handler: rangeHandler(data, `"v1"`, &stats{})},
		{name: "single stream", handler: plainHandler(data, &stats{})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)
			backend := strings.TrimPrefix(srv.URL, "http://")
			d := newDL(t, &Options{
				Parts: 8, MinPartSize: 16 << 20, EnableNodeSelection: true,
			})
			d.base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
			var resolves atomic.Int64
			d.resolve = func(context.Context, string) ([]netip.Addr, error) {
				resolves.Add(1)
				return []netip.Addr{
					netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
				}, nil
			}
			dialer := &net.Dialer{}
			d.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, backend)
			}
			dest := filepath.Join(t.TempDir(), "file.bin")
			_, got := mustGet(t, d, "http://cdn.test/file.bin", dest)
			if !bytes.Equal(got, data) {
				t.Fatal("inactive placement path changed downloaded bytes")
			}
			if resolves.Load() != 0 {
				t.Fatalf("inactive placement path performed %d resolver calls", resolves.Load())
			}
		})
	}
}

func TestNodePlacementLeastLoadedExploration(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTest(t, &Options{Parts: 3}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("192.0.2.2"),
		netip.MustParseAddr("2001:db8::1"),
	})
	var got []netip.Addr
	for id := range 3 {
		addr, err := p.reserve(id)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, addr)
	}
	want := []netip.Addr{
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("192.0.2.2"),
		netip.MustParseAddr("2001:db8::1"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("placement order = %v, want %v", got, want)
	}
}

func TestNodePlacementMatchesNormalizedDialTarget(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	for _, target := range []string{"cdn.test:443", "CDN.TEST:443", "cdn.test.:443"} {
		if !p.matchesTarget(target) {
			t.Errorf("matchesTarget(%q) = false", target)
		}
	}
	for _, target := range []string{"other.test:443", "cdn.test:80", "not-an-address"} {
		if p.matchesTarget(target) {
			t.Errorf("matchesTarget(%q) = true", target)
		}
	}
}

func TestMultipartPlacementUsesElectionAndDNSAddresses(t *testing.T) {
	t.Parallel()
	data := testData(8 << 20)
	var st stats
	secondaryStarted := make(chan struct{})
	var secondaryOnce sync.Once
	secondary := throttledRangeHandler(data, `"v1"`, &st,
		2*time.Millisecond, 32<<10, func(*http.Request) bool { return true })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-" {
			secondaryOnce.Do(func() { close(secondaryStarted) })
			secondary.ServeHTTP(w, r)
			return
		}
		// Keep the useful election body open until another worker starts its
		// explicit range. Without this gate a starved test goroutine can begin
		// after worker 0 has already released the election reservation, making
		// least-loaded placement correctly choose the election address again.
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-secondaryStarted:
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	backend := strings.TrimPrefix(srv.URL, "http://")
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")
	reporter := &nodeAddressReporter{}
	d := newDL(t, &Options{
		Parts: 2, MinParts: 2, MinPartSize: 64 << 10,
		Timeout: 5 * time.Second, Reporter: reporter, EnableNodeSelection: true,
	})
	// The election address is deliberately absent from the later DNS answer.
	placeAddresses(d, b)
	var dialMu sync.Mutex
	dialTargets := make(map[string]int)
	d.dial = logicalDialer(80, a, map[netip.Addr]string{a: backend, b: backend},
		func(target string) {
			dialMu.Lock()
			dialTargets[target]++
			dialMu.Unlock()
		})
	dest := filepath.Join(t.TempDir(), "file.bin")
	_, got := mustGet(t, d, "http://cdn.test/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("placed multipart bytes differ from source")
	}
	addrs, retries := reporter.snapshot()
	if addrs["192.0.2.1:80"] == 0 || addrs["192.0.2.2:80"] == 0 {
		dialMu.Lock()
		targets := maps.Clone(dialTargets)
		dialMu.Unlock()
		t.Fatalf("Connected addresses = %v from dial targets %v, want election and later-DNS addresses",
			addrs, targets)
	}
	if retries != 0 {
		t.Fatalf("placement emitted %d unexpected ChunkRetry events", retries)
	}
	if active := pinnedTransports(d); active != 0 {
		t.Fatalf("run left %d registered pinned transports", active)
	}
}

func TestSlowNodeCullingMigratesLosslesslyAtHundredToOne(t *testing.T) {
	oldInterval, oldWindow := nodeSampleInterval, nodeSampleWindow
	oldWarmup, oldCooldown := nodeCullWarmupBytes, nodeCullCooldown
	nodeSampleInterval = 25 * time.Millisecond
	nodeSampleWindow = 200 * time.Millisecond
	nodeCullWarmupBytes = 8 << 20
	nodeCullCooldown = 50 * time.Millisecond
	t.Cleanup(func() {
		nodeSampleInterval, nodeSampleWindow = oldInterval, oldWindow
		nodeCullWarmupBytes, nodeCullCooldown = oldWarmup, oldCooldown
	})

	// Keep transfer time dominant over fixed staging, Sync, and rename cost so
	// the wall-time gate measures node migration rather than filesystem noise.
	data := testData(128 << 20)
	var fastStats, slowStats stats
	var fastBytes, slowBytes atomic.Int64
	fastHandler := throttledRangeHandler(data, `"v1"`, &fastStats,
		time.Millisecond, 64, func(*http.Request) bool { return true })
	slowHandler := throttledRangeHandler(data, `"v1"`, &slowStats,
		100*time.Millisecond, 64, func(*http.Request) bool { return true })
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastHandler.ServeHTTP(&byteCountingResponseWriter{ResponseWriter: w, bytes: &fastBytes}, r)
	}))
	t.Cleanup(fastServer.Close)
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowHandler.ServeHTTP(&byteCountingResponseWriter{ResponseWriter: w, bytes: &slowBytes}, r)
	}))
	t.Cleanup(slowServer.Close)
	fastBackend := strings.TrimPrefix(fastServer.URL, "http://")
	slowBackend := strings.TrimPrefix(slowServer.URL, "http://")
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")

	type armResult struct {
		elapsed   time.Duration
		slowBytes int64
		retries   int
		addrs     map[string]int
	}
	runArm := func(cullingDisabled bool) armResult {
		if cullingDisabled {
			nodeCullWarmupBytes = 1 << 60
		} else {
			nodeCullWarmupBytes = 8 << 20
		}
		beforeSlow := slowBytes.Load()
		reporter := &nodeAddressReporter{}
		d := newDL(t, &Options{
			Parts: 8, MinParts: 8, MinPartSize: 64 << 10,
			Timeout: 15 * time.Second, Reporter: reporter, EnableNodeSelection: true,
		})
		placeAddresses(d, a, b)
		d.dial = logicalDialer(80, a,
			map[netip.Addr]string{a: fastBackend, b: slowBackend}, nil)
		start := time.Now()
		dest := filepath.Join(t.TempDir(), "file.bin")
		_, got := mustGet(t, d, "http://cdn.test/file.bin", dest)
		elapsed := time.Since(start)
		d.CloseIdleConnections()
		if !bytes.Equal(got, data) {
			t.Fatal("culling fixture installed non-identical bytes")
		}
		addrs, retries := reporter.snapshot()
		return armResult{
			elapsed: elapsed, slowBytes: slowBytes.Load() - beforeSlow,
			retries: retries, addrs: addrs,
		}
	}

	disabled := runArm(true)
	enabled := runArm(false)
	t.Logf("culling bytes from slow address: enabled=%d disabled=%d",
		enabled.slowBytes, disabled.slowBytes)
	if disabled.slowBytes == 0 || enabled.slowBytes == 0 {
		t.Fatalf("fixture did not exercise slow address: disabled=%d enabled=%d",
			disabled.slowBytes, enabled.slowBytes)
	}
	if enabled.slowBytes >= disabled.slowBytes {
		t.Fatalf("slow bytes were not migrated: enabled=%d disabled=%d",
			enabled.slowBytes, disabled.slowBytes)
	}
	if enabled.retries != 0 {
		t.Fatalf("slow-node migration emitted %d ChunkRetry events", enabled.retries)
	}
	if enabled.addrs["192.0.2.1:80"] == 0 || enabled.addrs["192.0.2.2:80"] == 0 {
		t.Fatalf("enabled fixture addresses = %v, want both", enabled.addrs)
	}
	ratio := float64(enabled.elapsed) / float64(disabled.elapsed)
	// Wall time is diagnostic here: real sleeps, socket buffers, and scheduler
	// tail stealing make it unsuitable as a go-test release gate. Dedicated
	// repeated performance runs own the 1.40x acceptance threshold.
	t.Logf("diagnostic culling wall-time ratio %.3f (%v/%v), speedup %.2fx",
		ratio, enabled.elapsed, disabled.elapsed, 1/ratio)
}

func TestNodePlacementConcurrentReservationsStayBalanced(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTest(t, &Options{Parts: 8}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	var wg sync.WaitGroup
	for id := range 8 {
		wg.Go(func() {
			if _, err := p.reserve(id); err != nil {
				t.Errorf("reserve worker %d: %v", id, err)
			}
		})
	}
	wg.Wait()
	p.mu.Lock()
	a := p.byAddr[netip.MustParseAddr("192.0.2.1")].conns
	b := p.byAddr[netip.MustParseAddr("192.0.2.2")].conns
	p.mu.Unlock()
	if a != 4 || b != 4 {
		t.Fatalf("least-loaded reservations = %d/%d, want 4/4", a, b)
	}
}

// snapshotAt takes a production snapshot at a fixed instant with the given
// tail oracle.
func snapshotAt(p *nodePlacement, at time.Time, tails map[int]bool) placementSnapshot {
	p.mu.Lock()
	p.now = func() time.Time { return at }
	p.mu.Unlock()
	return p.snapshot(func(id int) bool { return tails[id] })
}

func workerCounter(p *nodePlacement, id int) *nodeByteCounter {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w := p.workers[id]; w != nil {
		return w.counter.Load()
	}
	return nil
}

func pinnedTransports(d *Downloader) int {
	d.placementsMu.Lock()
	defer d.placementsMu.Unlock()
	n := 0
	for p := range d.placements {
		p.mu.Lock()
		n += len(p.transports)
		p.mu.Unlock()
	}
	return n
}

func prepareCullFixture(t *testing.T) (*nodePlacement, context.Context, context.CancelCauseFunc) {
	t.Helper()
	_, p := newPlacementForTest(t, &Options{Parts: 2}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	if _, err := p.reserve(0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.reserve(1); err != nil {
		t.Fatal(err)
	}
	attemptCtx, cancel := context.WithCancelCause(t.Context())
	p.beginAttempt(p.workers[1], cancel)
	return p, attemptCtx, cancel
}

func addWindowBytes(p *nodePlacement, fast, slow int64) {
	workerCounter(p, 0).bytes.Add(fast)
	workerCounter(p, 1).bytes.Add(slow)
}

func TestCullingRequiresTwoStableCompleteWindows(t *testing.T) {
	t.Parallel()
	p, slowAttempt, _ := prepareCullFixture(t)
	base := time.Now()
	tails := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, tails)
	addWindowBytes(p, 80<<20, 8<<20)
	first := snapshotAt(p, base.Add(10*time.Second), tails)
	p.evaluateWindow(start, first, true)
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("one slow evaluation canceled attempt: %v", cause)
	}
	addWindowBytes(p, 40<<20, 4<<20)
	second := snapshotAt(p, base.Add(15*time.Second), tails)
	p.evaluateWindow(start, second, true)
	if cause := context.Cause(slowAttempt); cause != errSlowNode { //nolint:errorlint // exact policy sentinel
		t.Fatalf("slow attempt cause = %v, want errSlowNode", cause)
	}
	p.mu.Lock()
	slow := p.byAddr[netip.MustParseAddr("192.0.2.2")]
	rotating := p.workers[1].rotate.Load()
	p.mu.Unlock()
	if slow.slowStrikes != 1 || slow.banned() || !slow.unavailableUntil.After(base) || !rotating {
		t.Fatalf("slow node state after first cull = %+v rotate=%v", slow, rotating)
	}
}

func TestHundredToOneNodeCullsWithProductionEvidencePolicy(t *testing.T) {
	t.Parallel()
	if nodeCullWarmupBytes != 8<<20 || nodeCullRatio != 0.25 ||
		nodeCullEvaluations != 2 || nodeSampleWindow != 10*time.Second {
		t.Fatal("test requires production culling policy constants")
	}
	p, slowAttempt, _ := prepareCullFixture(t)
	base := time.Now()
	tails := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, tails)
	addWindowBytes(p, 16<<20, 160<<10)
	first := snapshotAt(p, base.Add(10*time.Second), tails)
	p.evaluateWindow(start, first, true)
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("one 100:1 sample canceled attempt: %v", cause)
	}
	start = first
	addWindowBytes(p, 16<<20, 160<<10)
	second := snapshotAt(p, base.Add(20*time.Second), tails)
	p.evaluateWindow(start, second, true)
	if cause := context.Cause(slowAttempt); cause != errSlowNode { //nolint:errorlint
		t.Fatalf("100:1 slow attempt cause = %v, want errSlowNode", cause)
	}
}

func TestPassiveProbeAdmitsOnlyOneWorker(t *testing.T) {
	t.Parallel()
	_, p := newPlacementForTest(t, &Options{Parts: 4}, []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	})
	fast, err := p.reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	slow := netip.MustParseAddr("192.0.2.2")
	p.mu.Lock()
	p.byAddr[slow].slowStrikes = 1
	p.mu.Unlock()
	probe, err := p.reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	next, err := p.reserve(2)
	if err != nil {
		t.Fatal(err)
	}
	if probe != slow || next != fast {
		t.Fatalf("post-cull reservations = probe %v, next %v; want %v, %v",
			probe, next, slow, fast)
	}
}

func TestCullKeepsLastEligibleAlternative(t *testing.T) {
	t.Parallel()
	p, slowAttempt, _ := prepareCullFixture(t)
	fast := netip.MustParseAddr("192.0.2.1")
	slow := netip.MustParseAddr("192.0.2.2")
	p.markAvailabilityFailure(fast)
	p.mu.Lock()
	cancels, _, _ := p.cullLocked(p.byAddr[slow], p.byAddr[fast], time.Now())
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel(errSlowNode)
	}
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("cull without eligible alternative canceled attempt: %v", cause)
	}
	p.mu.Lock()
	strikes := p.byAddr[slow].slowStrikes
	p.mu.Unlock()
	if strikes != 0 {
		t.Fatalf("cull without eligible alternative added %d strikes", strikes)
	}
}

func TestCullingFrozenDuringRampAndTailDrain(t *testing.T) {
	t.Parallel()
	p, slowAttempt, _ := prepareCullFixture(t)
	base := time.Now()
	active := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, active)
	addWindowBytes(p, 80<<20, 8<<20)
	first := snapshotAt(p, base.Add(10*time.Second), active)
	p.evaluateWindow(start, first, false)
	addWindowBytes(p, 40<<20, 4<<20)
	second := snapshotAt(p, base.Add(15*time.Second), active)
	p.evaluateWindow(start, second, false)
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("ramp-time sample canceled attempt: %v", cause)
	}

	tails := map[int]bool{0: false, 1: true}
	start = snapshotAt(p, base.Add(20*time.Second), tails)
	addWindowBytes(p, 80<<20, 8<<20)
	first = snapshotAt(p, base.Add(30*time.Second), tails)
	p.evaluateWindow(start, first, true)
	addWindowBytes(p, 40<<20, 4<<20)
	second = snapshotAt(p, base.Add(35*time.Second), tails)
	p.evaluateWindow(start, second, true)
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("tail-drain sample canceled attempt: %v", cause)
	}
}

func TestEqualNodesWithThirtyPercentJitterNeverCull(t *testing.T) {
	t.Parallel()
	p, slowAttempt, _ := prepareCullFixture(t)
	base := time.Now()
	tails := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, tails)
	addWindowBytes(p, 13<<20, 9<<20)
	first := snapshotAt(p, base.Add(10*time.Second), tails)
	p.evaluateWindow(start, first, true)
	start = first
	addWindowBytes(p, 9<<20, 13<<20)
	second := snapshotAt(p, base.Add(20*time.Second), tails)
	p.evaluateWindow(start, second, true)
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("jitter fixture canceled equal node: %v", cause)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, node := range p.ordered {
		if node.slowStrikes != 0 || node.banned() || node.belowSample != 0 {
			t.Fatalf("equal-node state = %+v", node)
		}
	}
}

func TestPassiveProbeTwoHealthySamplesRestoreEligibility(t *testing.T) {
	t.Parallel()
	p, _, _ := prepareCullFixture(t)
	base := time.Now()
	tails := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, tails)
	addWindowBytes(p, 80<<20, 8<<20)
	first := snapshotAt(p, base.Add(10*time.Second), tails)
	p.evaluateWindow(start, first, true)
	addWindowBytes(p, 40<<20, 4<<20)
	second := snapshotAt(p, base.Add(15*time.Second), tails)
	p.evaluateWindow(start, second, true)

	p.mu.Lock()
	slow := p.byAddr[netip.MustParseAddr("192.0.2.2")]
	slow.unavailableUntil = time.Time{}
	slow.belowSample = 0
	p.lastCull = time.Time{}
	p.workers[1].rotate.Store(false)
	p.mu.Unlock()
	start = snapshotAt(p, base.Add(20*time.Second), tails)
	addWindowBytes(p, 16<<20, 16<<20)
	first = snapshotAt(p, base.Add(30*time.Second), tails)
	p.evaluateWindow(start, first, true)
	start = first
	addWindowBytes(p, 16<<20, 16<<20)
	second = snapshotAt(p, base.Add(40*time.Second), tails)
	p.evaluateWindow(start, second, true)
	p.mu.Lock()
	restored := *slow
	p.mu.Unlock()
	if restored.slowStrikes != 0 || restored.probing() || restored.banned() {
		t.Fatalf("node not restored after two healthy probes: %+v", restored)
	}
}

func TestSecondConfirmedSlowCullBansForRun(t *testing.T) {
	t.Parallel()
	p, _, _ := prepareCullFixture(t)
	base := time.Now()
	tails := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, tails)
	addWindowBytes(p, 80<<20, 8<<20)
	first := snapshotAt(p, base.Add(10*time.Second), tails)
	p.evaluateWindow(start, first, true)
	addWindowBytes(p, 40<<20, 4<<20)
	second := snapshotAt(p, base.Add(15*time.Second), tails)
	p.evaluateWindow(start, second, true)

	p.mu.Lock()
	slow := p.byAddr[netip.MustParseAddr("192.0.2.2")]
	slow.unavailableUntil = time.Time{}
	slow.belowSample = 0
	p.lastCull = time.Time{}
	p.workers[1].rotate.Store(false)
	probeCtx, probeCancel := context.WithCancelCause(t.Context())
	p.workers[1].cancel = probeCancel
	p.mu.Unlock()
	start = snapshotAt(p, base.Add(20*time.Second), tails)
	addWindowBytes(p, 80<<20, 8<<20)
	first = snapshotAt(p, base.Add(30*time.Second), tails)
	p.evaluateWindow(start, first, true)
	addWindowBytes(p, 40<<20, 4<<20)
	second = snapshotAt(p, base.Add(35*time.Second), tails)
	p.evaluateWindow(start, second, true)
	if cause := context.Cause(probeCtx); cause != errSlowNode { //nolint:errorlint // exact policy sentinel
		t.Fatalf("second slow probe cause = %v", cause)
	}
	p.mu.Lock()
	banned := slow.banned()
	strikes := slow.slowStrikes
	p.mu.Unlock()
	if !banned || strikes != 2 {
		t.Fatalf("second confirmed cull: banned=%v strikes=%d", banned, strikes)
	}
}

func TestAddressGenerationChangeInvalidatesWindow(t *testing.T) {
	t.Parallel()
	p, slowAttempt, _ := prepareCullFixture(t)
	base := time.Now()
	tails := map[int]bool{0: false, 1: false}
	start := snapshotAt(p, base, tails)
	addWindowBytes(p, 80<<20, 8<<20)
	p.markDialWinner(1, netip.MustParseAddr("192.0.2.1"))
	p.markDialWinner(1, netip.MustParseAddr("192.0.2.2"))
	end := snapshotAt(p, base.Add(10*time.Second), tails)
	p.evaluateWindow(start, end, true)
	if cause := context.Cause(slowAttempt); cause != nil {
		t.Fatalf("generation-spanning sample canceled attempt: %v", cause)
	}
}
