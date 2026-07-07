// Package observ provides Prometheus metrics and a read-only JSON status API.
// It receives simple counters/values and does not depend back on engine or
// store. Structured logging is configured by the daemon entrypoint (cmd), not
// here.
package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samuelenocsson/slskdarr/internal/core"
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

// IncReconcile counts one reconciliation pass.
func (m *Metrics) IncReconcile() { m.ReconcileTotal.Inc() }

// SetUnknownTransfers records the current count of slskd transfers not tracked by slskdarr.
func (m *Metrics) SetUnknownTransfers(n int) { m.UnknownTransfers.Set(float64(n)) }

// SetDownloadsActive records the current count of active downloads.
func (m *Metrics) SetDownloadsActive(n int) { m.DownloadsActive.Set(float64(n)) }

// StatusReport is the read-only snapshot served at /status.
type StatusReport struct {
	Queued   int `json:"queued"`
	Active   int `json:"active"`
	Stalled  int `json:"stalled"`
	Orphaned int `json:"orphaned"`
}

// StatusFunc produces a current StatusReport (typically backed by the store).
type StatusFunc func(ctx context.Context) (StatusReport, error)

// jobDTO is the JSON shape served at /api/jobs — a flattened, display-ready
// view of core.JobView so the frontend never needs to know about the
// engine's internal state machine.
type jobDTO struct {
	ID              int64  `json:"id"`
	Title           string `json:"title"`
	Artist          string `json:"artist"`
	Status          string `json:"status"`
	Peer            string `json:"peer"`
	BytesDone       int64  `json:"bytesDone"`
	BytesTotal      int64  `json:"bytesTotal"`
	UpdatedAt       string `json:"updatedAt"`
	State           string `json:"state"`
	CandidatesTried int    `json:"candidatesTried"`
	MaxCandidates   int    `json:"maxCandidates"`
	FailReason      string `json:"failReason"`
	NextAttemptAt   string `json:"nextAttemptAt"`
	Retries         int    `json:"retries"`
	NotBefore       string `json:"notBefore"`
}

// toJobDTO flattens a core.JobView into the dashboard's display-ready shape.
// failedRetryAfter and maxCandidates are engine config values threaded in from
// NewServer, needed to compute nextAttemptAt for FAILED jobs and maxCandidates.
func toJobDTO(v core.JobView, failedRetryAfter time.Duration, maxCandidates int) jobDTO {
	d := jobDTO{
		ID:              v.Job.ID,
		Title:           v.Job.Title,
		Artist:          v.Job.ArtistName,
		Status:          dashboardStatus(v),
		Peer:            v.Peer,
		UpdatedAt:       v.Job.UpdatedAt.Format(timeFormat),
		State:           string(v.Job.State),
		CandidatesTried: v.Job.CandidatesTried,
		MaxCandidates:   maxCandidates,
		Retries:         v.Job.Retries,
	}
	if v.Transfer != nil {
		d.BytesDone = v.Transfer.BytesDone
		d.BytesTotal = v.Transfer.BytesTotal
	}
	if v.Attempt != nil {
		d.FailReason = v.Attempt.FailReason
	}
	if v.Job.NotBefore != nil {
		d.NotBefore = v.Job.NotBefore.Format(timeFormat)
	}
	if v.Job.State == core.StateFailed {
		d.NextAttemptAt = v.Job.UpdatedAt.Add(failedRetryAfter).Format(timeFormat)
	}
	return d
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

// JobsFunc produces the current list of job views (typically backed by the
// store's ListJobsWithTransfer).
type JobsFunc func(ctx context.Context) ([]core.JobView, error)

// CancelResult is the outcome of a CancelFunc call.
type CancelResult int

const (
	CancelResultOK CancelResult = iota
	CancelResultNotFound
	CancelResultFailed
)

// CancelFunc cancels a job by id, returning which outcome occurred.
type CancelFunc func(ctx context.Context, jobID int64) (CancelResult, error)

// RetryResult is the outcome of a RetryFunc call.
type RetryResult int

const (
	RetryResultOK RetryResult = iota
	RetryResultNotFound
	// RetryResultConflict means the job exists but is not FAILED — the
	// dashboard button raced a state change (e.g. WantedSync revived it, or a
	// module already advanced it).
	RetryResultConflict
)

// RetryFunc manually revives one FAILED job by id, returning which outcome
// occurred (typically backed by the store's RetryFailedJob).
type RetryFunc func(ctx context.Context, jobID int64) (RetryResult, error)

// HealthyFunc reports whether the pipeline's modules are still making
// progress. Unlike /status (a plain DB read that stays up even if a module
// goroutine deadlocks), this is the liveness signal Docker/Swarm should poll.
type HealthyFunc func() bool

// ModulesFunc reports each pipeline module's last completed tick (see
// pipeline.Runner.Health), surfaced at /status so an operator can see which
// module (if any) has gone stale without needing metrics/log access. A zero
// time.Time means the module has never completed a tick.
type ModulesFunc func() map[string]time.Time

// NewServer returns an http.Handler exposing /metrics, /status, /healthz,
// /api/jobs, /api/jobs/{id}/cancel, /api/jobs/{id}/retry, /api/jobs/{id}/detail,
// /api/jobs/{id}/events, /api/events, /api/peers, and the dashboard UI at /.
// failedRetryAfter and maxCandidates are engine config values surfaced in
// /api/jobs so the dashboard can show a job's retry ETA and candidate budget.
func NewServer(reg *prometheus.Registry, status StatusFunc, jobs JobsFunc, cancel CancelFunc,
	jobDetail JobDetailFunc, jobEvents JobEventsFunc, recentEvents RecentEventsFunc, peers PeersFunc,
	healthy HealthyFunc, modules ModulesFunc, retry RetryFunc, failedRetryAfter time.Duration, maxCandidates int) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !healthy() {
			http.Error(w, "pipeline module stalled", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		report, err := status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		moduleTicks := map[string]string{}
		for name, t := range modules() {
			if !t.IsZero() {
				moduleTicks[name] = t.Format(timeFormat)
			} else {
				moduleTicks[name] = ""
			}
		}
		resp := struct {
			StatusReport
			Modules map[string]string `json:"modules"`
		}{report, moduleTicks}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		views, err := jobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dtos := make([]jobDTO, len(views))
		for i, v := range views {
			dtos[i] = toJobDTO(v, failedRetryAfter, maxCandidates)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dtos)
	})
	mux.HandleFunc("/api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		result, err := cancel(r.Context(), jobID)
		switch result {
		case CancelResultNotFound:
			http.Error(w, "job not found", http.StatusNotFound)
		case CancelResultFailed:
			msg := "cancel failed"
			if err != nil {
				msg = err.Error()
			}
			http.Error(w, msg, http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		result, err := retry(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		switch result {
		case RetryResultNotFound:
			http.Error(w, "job not found", http.StatusNotFound)
		case RetryResultConflict:
			http.Error(w, "job is not FAILED", http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/jobs/{id}/detail", func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		d, found, err := jobDetail(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toJobDetailDTO(d))
	})
	mux.HandleFunc("/api/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		events, err := jobEvents(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toEventDTOs(events))
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		limit := eventsLimitDefault
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > eventsLimitMax {
			limit = eventsLimitMax
		}
		events, err := recentEvents(r.Context(), limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toEventDTOs(events))
	})
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		rows, err := peers(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		dtos := make([]peerDTO, len(rows))
		for i, row := range rows {
			dtos[i] = toPeerDTO(row, now)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dtos)
	})
	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/dashboard.js", dashboardJSHandler)
	return mux
}
