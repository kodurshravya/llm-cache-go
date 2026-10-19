// Package metrics defines the proxy's Prometheus collectors.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics uses its own registry so tests can create many servers.
type Metrics struct {
	reg               *prometheus.Registry
	Requests          *prometheus.CounterVec
	UpstreamRequests  *prometheus.CounterVec
	UpstreamSeconds   prometheus.Histogram
	OverheadSeconds   prometheus.Histogram
	EntryBytes        prometheus.Histogram
	InflightCoalesced prometheus.Gauge
}

// New creates and registers all collectors.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmc_requests_total",
			Help: "Requests by cache result (hit, miss, coalesced, bypass).",
		}, []string{"result"}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmc_upstream_requests_total",
			Help: "Upstream calls by HTTP status, or \"error\" for transport failures.",
		}, []string{"status"}),
		UpstreamSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "llmc_upstream_seconds",
			Help:    "Time spent waiting on upstream, including reading the body.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms .. ~82s
		}),
		OverheadSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "llmc_overhead_seconds",
			Help:    "Time spent in the proxy, excluding upstream.",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 16), // 0.1ms .. ~3.3s
		}),
		EntryBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "llmc_entry_bytes",
			Help:    "Size of stored cache entries.",
			Buckets: prometheus.ExponentialBuckets(256, 4, 9), // 256B .. 16MB
		}),
		InflightCoalesced: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llmc_inflight_coalesced",
			Help: "Requests currently waiting on another request's upstream call.",
		}),
	}
	m.reg.MustRegister(
		m.Requests, m.UpstreamRequests, m.UpstreamSeconds, m.OverheadSeconds,
		m.EntryBytes, m.InflightCoalesced,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
