package api

import (
	"runtime"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type requestMetrics struct {
	active        atomic.Int64
	total         atomic.Uint64
	panics        atomic.Uint64
	durationNanos atomic.Uint64
	maxNanos      atomic.Uint64
	statusClasses [6]atomic.Uint64
}

type requestStatusSnapshot struct {
	Unknown       uint64 `json:"unknown"`
	Informational uint64 `json:"informational"`
	Successful    uint64 `json:"successful"`
	Redirection   uint64 `json:"redirection"`
	ClientError   uint64 `json:"client_error"`
	ServerError   uint64 `json:"server_error"`
}

type requestMetricsSnapshot struct {
	Active               int64                 `json:"active"`
	Total                uint64                `json:"total"`
	Errors               uint64                `json:"errors"`
	Panics               uint64                `json:"panics"`
	AverageLatencyMillis float64               `json:"average_latency_ms"`
	MaximumLatencyMillis float64               `json:"maximum_latency_ms"`
	Status               requestStatusSnapshot `json:"status"`
}

func (m *requestMetrics) begin() {
	if m == nil {
		return
	}
	m.total.Add(1)
	m.active.Add(1)
}

func (m *requestMetrics) finish(status int, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.active.Add(-1)
	class := 0
	if status >= 100 && status <= 599 {
		class = status / 100
	}
	m.statusClasses[class].Add(1)

	nanos := uint64(max(elapsed.Nanoseconds(), 0))
	m.durationNanos.Add(nanos)
	for {
		current := m.maxNanos.Load()
		if nanos <= current || m.maxNanos.CompareAndSwap(current, nanos) {
			break
		}
	}
}

func (m *requestMetrics) recordPanic() {
	if m != nil {
		m.panics.Add(1)
	}
}

func (m *requestMetrics) snapshot() requestMetricsSnapshot {
	if m == nil {
		return requestMetricsSnapshot{}
	}
	total := m.total.Load()
	status := requestStatusSnapshot{
		Unknown:       m.statusClasses[0].Load(),
		Informational: m.statusClasses[1].Load(),
		Successful:    m.statusClasses[2].Load(),
		Redirection:   m.statusClasses[3].Load(),
		ClientError:   m.statusClasses[4].Load(),
		ServerError:   m.statusClasses[5].Load(),
	}
	averageMillis := 0.0
	if total > 0 {
		averageMillis = float64(m.durationNanos.Load()) / float64(total) / float64(time.Millisecond)
	}
	return requestMetricsSnapshot{
		Active:               m.active.Load(),
		Total:                total,
		Errors:               status.ClientError + status.ServerError,
		Panics:               m.panics.Load(),
		AverageLatencyMillis: averageMillis,
		MaximumLatencyMillis: float64(m.maxNanos.Load()) / float64(time.Millisecond),
		Status:               status,
	}
}

func metricsBody(vc *vertex.VertexAIClient, requests *requestMetrics) map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return map[string]any{
		"status": "ok",
		"memory": map[string]any{
			"alloc_mb":   bToMb(m.Alloc),
			"heap_inuse": bToMb(m.HeapInuse),
			"num_gc":     m.NumGC,
			"goroutines": runtime.NumGoroutine(),
			"spilled_mb": bToMb(uint64(spool.SpilledBytes())),
		},
		"token_count": vc.CountTokenStats(),
		"requests":    requests.snapshot(),
	}
}

func bToMb(b uint64) float64 {
	return float64(b) / 1024 / 1024
}
