package download

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkGetMultipart tracks per-download wall time and allocations over
// loopback (the ipsw integration flagged allocated bytes/op as the engine's
// main cost; the buffer pool exists because of it).
func BenchmarkGetMultipart(b *testing.B) {
	data := testData(8 << 20)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	d, err := New(&Options{Parts: 4, MinPartSize: 1 << 20, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if _, err := d.Get(b.Context(), srv.URL+"/file.bin", dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStdlibGet is the http.Get + io.Copy baseline against the identical
// loopback server, for an apples-to-apples overhead comparison with
// BenchmarkGetMultipart.
func BenchmarkStdlibGet(b *testing.B) {
	data := testData(8 << 20)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if err := stdlibDownload(srv.URL+"/file.bin", dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}

func stdlibDownload(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

// The constrained pair models the network where parallel parts pay off:
// every connection is individually throttled (per-flow shaping, long fat
// networks, per-connection CDN limits), so aggregate throughput scales with
// the number of connections.

const (
	// Large enough that the adaptive concurrency ramp amortizes, as it
	// does on the multi-GB downloads this scenario models.
	constrainedSize  = 64 << 20
	constrainedDelay = 2 * time.Millisecond // per 64 KiB per connection
)

func BenchmarkConstrainedStdlib(b *testing.B) {
	data := testData(constrainedSize)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		constrainedDelay, 64, func(*http.Request) bool { return true }))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if err := stdlibDownload(srv.URL+"/file.bin", dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConstrainedMultipart(b *testing.B) {
	data := testData(constrainedSize)
	var st stats
	srv := httptest.NewServer(throttledRangeHandler(data, `"v1"`, &st,
		constrainedDelay, 64, func(*http.Request) bool { return true }))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	d, err := New(&Options{Parts: 4, MinPartSize: 1 << 20, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if _, err := d.Get(b.Context(), srv.URL+"/file.bin", dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}

// Real-network benchmarks, opt-in so CI never touches the internet:
//
//	env DL_BENCH_URL=https://ash-speed.hetzner.com/100MB.bin \
//	    go test -bench 'BenchmarkReal' -benchtime 3x .
//
// Compare stdlib single-stream against the parallel engine over the same
// WAN path (this is where parallel ranges pay off: a single TCP connection
// is limited by the bandwidth-delay product and per-flow shaping).
func realBenchURL(b *testing.B) string {
	b.Helper()
	url := os.Getenv("DL_BENCH_URL")
	if url == "" {
		b.Skip("set DL_BENCH_URL to run real-network benchmarks")
	}
	return url
}

func BenchmarkRealStdlib(b *testing.B) {
	url := realBenchURL(b)
	dir := b.TempDir()
	var size int64
	b.ReportAllocs()
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if err := stdlibDownload(url, dest); err != nil {
			b.Fatal(err)
		}
		fi, err := os.Stat(dest)
		if err != nil {
			b.Fatal(err)
		}
		size = fi.Size()
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(size)
}

func BenchmarkRealMultipart(b *testing.B) {
	url := realBenchURL(b)
	dir := b.TempDir()
	d, err := New(&Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	var size int64
	b.ReportAllocs()
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		res, err := d.Get(b.Context(), url, dest)
		if err != nil {
			b.Fatal(err)
		}
		size = res.Size
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(size)
}

// The shared-cap pair models the saturated access link: one limiter caps the
// AGGREGATE rate across all connections, so extra flows cannot add
// throughput and the engine should converge to a single flow.

const (
	sharedCapSize = 32 << 20
	sharedCapRate = 32 << 20 // bytes/sec, aggregate
)

func BenchmarkSharedCapStdlib(b *testing.B) {
	data := testData(sharedCapSize)
	var st stats
	srv := httptest.NewServer(sharedCapHandler(data, `"v1"`, &st,
		newSharedLimiter(sharedCapRate), nil))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if err := stdlibDownload(srv.URL+"/file.bin", dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSharedCapMultipart(b *testing.B) {
	data := testData(sharedCapSize)
	var st stats
	srv := httptest.NewServer(sharedCapHandler(data, `"v1"`, &st,
		newSharedLimiter(sharedCapRate), nil))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	d, err := New(&Options{Parts: 4, MinPartSize: 1 << 20, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		if _, err := d.Get(b.Context(), srv.URL+"/file.bin", dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}
