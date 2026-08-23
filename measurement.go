package download

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// runMeasurement is allocated only when the configured logger accepts debug
// records. It is deliberately internal: benchmark campaigns consume the
// structured log without turning research telemetry into a public API.
type runMeasurement struct {
	host              string
	protocol          string
	conc              Concurrency
	checksumRequested bool
	retries           atomic.Int64
	http429s          atomic.Int64
	placement         atomic.Bool
	addressesMu       sync.Mutex
	addresses         map[string]struct{}
}

func newRunMeasurement(ctx context.Context, logger *slog.Logger) *runMeasurement {
	if logger == nil || !logger.Enabled(ctx, slog.LevelDebug) {
		return nil
	}
	return &runMeasurement{addresses: make(map[string]struct{})}
}

func (m *runMeasurement) configure(finalURL *url.URL, protocol string, conc Concurrency, checksum bool) {
	if m == nil {
		return
	}
	if finalURL != nil {
		m.host = NormalizeHost(finalURL.Hostname())
	}
	m.protocol = protocol
	m.conc = conc
	m.checksumRequested = checksum
}

func (m *runMeasurement) connected(addr string) {
	if m == nil || addr == "" {
		return
	}
	m.addressesMu.Lock()
	m.addresses[addr] = struct{}{}
	m.addressesMu.Unlock()
}

func (m *runMeasurement) retry() {
	if m == nil {
		return
	}
	m.retries.Add(1)
}

func (m *runMeasurement) throttle() {
	if m != nil {
		m.http429s.Add(1)
	}
}

func (m *runMeasurement) setPlacement(enabled bool) {
	if m != nil {
		m.placement.Store(enabled)
	}
}

func (m *runMeasurement) log(
	ctx context.Context, logger *slog.Logger, elapsed time.Duration, res *Result, err error,
) {
	if m == nil {
		return
	}
	m.addressesMu.Lock()
	addresses := slices.Sorted(maps.Keys(m.addresses))
	m.addressesMu.Unlock()

	integrity := "not_reached"
	if err == nil {
		integrity = "passed"
	} else {
		var checksumErr *ChecksumError
		var sizeErr *SizeError
		if errors.As(err, &checksumErr) || errors.As(err, &sizeErr) {
			integrity = "failed"
		}
	}
	errorKind := ""
	if err != nil {
		// Do not log peer-controlled error text or signed URLs. The concrete
		// type is enough to group failed benchmark arms.
		errorKind = fmt.Sprintf("%T", err)
	}
	var size int64
	var resumed bool
	if res != nil {
		size = res.Size
		resumed = res.Resumed
	}
	logger.DebugContext(ctx, "download measurement",
		"host", m.host,
		"parts", m.conc.Parts,
		"min_parts", m.conc.MinParts,
		"min_part_size", m.conc.MinPartSize,
		"protocol", m.protocol,
		"elapsed", elapsed,
		"retries", m.retries.Load(),
		"http_429s", m.http429s.Load(),
		"connected_addresses", addresses,
		"placement", m.placement.Load(),
		"resumed", resumed,
		"size", size,
		"checksum_requested", m.checksumRequested,
		"integrity", integrity,
		"success", err == nil,
		"error_kind", errorKind,
	)
}
