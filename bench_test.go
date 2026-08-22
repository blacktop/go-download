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

// BenchmarkSmallFileDefault guards the chosen small-file policy: at default
// options an object below Parts*MinPartSize stays on its useful initial
// response with no probe, follow-up request, or extra dial while retaining
// resume. A wall-time or allocation regression here means the small-file
// lifecycle grew real overhead.
func BenchmarkSmallFileDefault(b *testing.B) {
	benchmarkDefaultDownload(b, 4<<20)
}

// BenchmarkSmallFileDurableStdlib is the semantics-matched baseline for
// BenchmarkSmallFileDefault: it uses the same useful range response, stages
// to a temporary file, syncs it, and installs it with a rename.
func BenchmarkSmallFileDurableStdlib(b *testing.B) {
	benchmarkDurableStdlib(b, 4<<20)
}

// BenchmarkLargeFileDefault covers the first default-policy size eligible for
// adaptive expansion (8 parts * 16 MiB). It protects the large-file path
// without forcing aggressive multipart settings into a small fixture.
func BenchmarkLargeFileDefault(b *testing.B) {
	benchmarkDefaultDownload(b, 128<<20)
}

func BenchmarkLargeFileDurableStdlib(b *testing.B) {
	benchmarkDurableStdlib(b, 128<<20)
}

func benchmarkDefaultDownload(b *testing.B, size int) {
	b.Helper()
	data := testData(size)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	d, err := New(&Options{Logger: slog.New(slog.DiscardHandler)})
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

func benchmarkDurableStdlib(b *testing.B, size int) {
	b.Helper()
	data := testData(size)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	b.Cleanup(srv.Close)

	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		dest := filepath.Join(dir, "bench.bin")
		part := dest + ".part"
		req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, srv.URL+"/file.bin", nil)
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		file, err := os.Create(part)
		if err != nil {
			resp.Body.Close()
			b.Fatal(err)
		}
		_, copyErr := io.Copy(file, resp.Body)
		closeBodyErr := resp.Body.Close()
		syncErr := file.Sync()
		closeFileErr := file.Close()
		if copyErr != nil || closeBodyErr != nil || syncErr != nil || closeFileErr != nil {
			b.Fatalf("durable copy: copy=%v body=%v sync=%v close=%v", copyErr, closeBodyErr, syncErr, closeFileErr)
		}
		if err := os.Rename(part, dest); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(dest); err != nil {
			b.Fatal(err)
		}
	}
}
