// Package observ provides structured logging, Prometheus metrics, and a
// read-only JSON status API. It receives simple counters/values and does not
// depend back on engine or store.
package observ

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the Prometheus collectors slskdarr exports.
type Metrics struct {
	ReconcileTotal   prometheus.Counter
	UnknownTransfers prometheus.Gauge
	DownloadsActive  prometheus.Gauge
}

// NewMetrics constructs and registers the collectors on reg.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		ReconcileTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slskdarr_reconcile_total", Help: "Total reconciliation passes run.",
		}),
		UnknownTransfers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "slskdarr_unknown_transfers", Help: "slskd transfers not tracked by slskdarr.",
		}),
		DownloadsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "slskdarr_downloads_active", Help: "Currently active downloads.",
		}),
	}
	reg.MustRegister(m.ReconcileTotal, m.UnknownTransfers, m.DownloadsActive)
	return m
}

// StatusReport is the read-only snapshot served at /status.
type StatusReport struct {
	Queued   int `json:"queued"`
	Active   int `json:"active"`
	Stalled  int `json:"stalled"`
	Orphaned int `json:"orphaned"`
}

// StatusFunc produces a current StatusReport (typically backed by the store).
type StatusFunc func(ctx context.Context) (StatusReport, error)

// NewServer returns an http.Handler exposing /metrics and /status.
func NewServer(reg *prometheus.Registry, status StatusFunc) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		report, err := status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	})
	return mux
}
