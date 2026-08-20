package download

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPicker(addrs ...string) *picker {
	p := newPicker("cdn.test", "80", slog.New(slog.DiscardHandler))
	p.resolve = func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, len(addrs))
		for i, a := range addrs {
			out[i] = netip.MustParseAddr(a)
		}
		return out, nil
	}
	return p
}

func TestPickerExploresUnsampledNodes(t *testing.T) {
	t.Parallel()
	p := testPicker("192.0.2.1", "192.0.2.2")
	a, err := p.pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Both unsampled: the conns tiebreak must spread the two workers.
	if a == b {
		t.Errorf("both workers pinned to %v; want spread across nodes", a.addr)
	}
}

func TestPickerPrefersFasterNode(t *testing.T) {
	t.Parallel()
	p := testPicker("192.0.2.1", "192.0.2.2")
	if err := p.refreshUnderTest(t); err != nil {
		t.Fatal(err)
	}
	fast, slow := p.nodes[0], p.nodes[1]
	for range 40 {
		p.observe(fast, 1<<20, 10*time.Millisecond) // ~100 MB/s
		p.observe(slow, 1<<10, 10*time.Millisecond) // ~100 KB/s
	}
	wins := 0
	for range 50 {
		n, err := p.pick(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == fast {
			wins++
		}
		p.release(n)
	}
	if wins < 45 {
		t.Errorf("fast node picked %d/50 times; power-of-two should almost always find it", wins)
	}
}

// refreshUnderTest exposes refresh for direct node access in tests.
func (p *picker) refreshUnderTest(t *testing.T) error {
	t.Helper()
	return p.refresh(t.Context())
}

func TestPickerCullSemantics(t *testing.T) {
	t.Parallel()
	p := testPicker("192.0.2.1", "192.0.2.2")
	if err := p.refreshUnderTest(t); err != nil {
		t.Fatal(err)
	}
	fast, slow := p.nodes[0], p.nodes[1]

	if p.shouldCull(slow) {
		t.Error("unsampled node must not be culled")
	}
	p.observe(fast, cullWarmupBytes, time.Second)
	p.observe(slow, cullWarmupBytes, time.Second)
	if p.shouldCull(slow) {
		t.Error("equal-speed node must not be culled")
	}
	// Make slow's EWMA collapse below 25% of fast's.
	for range 60 {
		p.observe(slow, 1<<10, time.Second)
		p.observe(fast, 10<<20, time.Second)
	}
	if !p.shouldCull(slow) {
		t.Error("node at <25%% of best throughput must be culled")
	}
	if p.shouldCull(fast) {
		t.Error("best node must never be culled")
	}
}

func TestPickerSingleNodeNeverCulled(t *testing.T) {
	t.Parallel()
	p := testPicker("192.0.2.1")
	if err := p.refreshUnderTest(t); err != nil {
		t.Fatal(err)
	}
	n := p.nodes[0]
	p.observe(n, 2*cullWarmupBytes, time.Hour) // absurdly slow
	if p.shouldCull(n) {
		t.Error("only node must never be culled: there is nowhere better")
	}
}

func TestPickerStrikeBanAndRecovery(t *testing.T) {
	t.Parallel()
	p := testPicker("192.0.2.1", "192.0.2.2")
	if err := p.refreshUnderTest(t); err != nil {
		t.Fatal(err)
	}
	bad := p.nodes[0]
	p.strike(bad)
	if !bad.banUntil.IsZero() {
		t.Fatal("one strike must not ban")
	}
	p.strike(bad)
	p.strike(bad)
	if bad.banUntil.IsZero() {
		t.Fatal("repeat strikes must ban")
	}
	for range 10 {
		n, err := p.pick(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == bad {
			t.Fatal("banned node was picked while an alternative exists")
		}
		p.release(n)
	}

	// Ban everyone: pick must still return something (least-bad unban).
	for _, n := range p.nodes {
		p.mu.Lock()
		n.banUntil = time.Now().Add(time.Hour)
		p.mu.Unlock()
	}
	if _, err := p.pick(t.Context()); err != nil {
		t.Fatalf("pick with all nodes banned must recover, got %v", err)
	}
}

// TestNodeCulling is the end-to-end M5 test: the host "resolves" to a fast
// and a pathologically slow node; culling must abandon the slow one so the
// download finishes at fast-node speed. Not parallel: it tunes package vars.
func TestNodeCulling(t *testing.T) {
	data := testData(1 << 20)
	oldWarmup := cullWarmupBytes
	cullWarmupBytes = 16 << 10
	t.Cleanup(func() { cullWarmupBytes = oldWarmup })

	var stFast, stSlow stats
	fast := httptest.NewServer(rangeHandler(data, `"v1"`, &stFast))
	defer fast.Close()
	slow := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &stSlow,
		30*time.Millisecond, 1, func(*http.Request) bool { return true }))
	defer slow.Close()

	backends := map[string]string{
		"192.0.2.1:80": strings.TrimPrefix(fast.URL, "http://"),
		"192.0.2.2:80": strings.TrimPrefix(slow.URL, "http://"),
		"cdn.test:80":  strings.TrimPrefix(fast.URL, "http://"),
	}

	rep := &recordingReporter{}
	d := newDL(t, &Options{Parts: 2, MinPartSize: 8 << 10,
		Timeout: 60 * time.Second, Reporter: rep})
	d.resolveHook = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("192.0.2.1"),
			netip.MustParseAddr("192.0.2.2"),
		}, nil
	}
	d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		backend, ok := backends[addr]
		if !ok {
			t.Errorf("unexpected dial %q", addr)
			return nil, &net.AddrError{Err: "unexpected", Addr: addr}
		}
		return (&net.Dialer{}).DialContext(ctx, network, backend)
	}

	dest := filepath.Join(t.TempDir(), "file.bin")
	start := time.Now()
	res, err := d.Get(t.Context(), "http://cdn.test/file.bin", dest)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	// Unculled, the slow node would serve ~512 KiB at ~34 KiB/s (≈15s).
	if elapsed > 10*time.Second {
		t.Errorf("download took %v; culling should have abandoned the slow node", elapsed)
	}
	if len(stSlow.rangeHeaders()) == 0 {
		t.Error("slow node was never tried: exploration is broken")
	}
	if len(stFast.rangeHeaders()) == 0 {
		t.Error("fast node never used")
	}
	// GotConn reports the real backend addresses the pinned dials reached.
	if len(rep.addrList()) < 2 {
		t.Errorf("Connected events saw only %v; want both nodes", rep.addrList())
	}
}

// recordingReporter captures Connected events.
type recordingReporter struct {
	NopReporter
	mu    sync.Mutex
	addrs map[string]bool
}

func (r *recordingReporter) Connected(_ int, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.addrs == nil {
		r.addrs = make(map[string]bool)
	}
	r.addrs[addr] = true
}

func (r *recordingReporter) addrList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for a := range r.addrs {
		out = append(out, a)
	}
	return out
}
