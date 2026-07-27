// Package observ provides Prometheus metrics and a read-only JSON status API.
// It receives simple counters/values and does not depend back on engine or
// store. Structured logging is configured by the daemon entrypoint (cmd), not
// here.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// Metrics holds the Prometheus collectors slskdarr exports.
type Metrics struct {
	ReconcileTotal      prometheus.Counter
	UnknownTransfers    prometheus.Gauge
	DownloadsActive     prometheus.Gauge
	AlbumReleasesErrors prometheus.Counter
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
		AlbumReleasesErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slskdarr_album_releases_errors_total", Help: "Total failed Lidarr AlbumReleases calls during discovery.",
		}),
	}
	reg.MustRegister(m.ReconcileTotal, m.UnknownTransfers, m.DownloadsActive, m.AlbumReleasesErrors)
	return m
}

// IncReconcile counts one reconciliation pass.
func (m *Metrics) IncReconcile() { m.ReconcileTotal.Inc() }

// IncAlbumReleasesError counts one failed Lidarr AlbumReleases call in Discovery.
func (m *Metrics) IncAlbumReleasesError() { m.AlbumReleasesErrors.Inc() }

// SetUnknownTransfers records the current count of slskd transfers not tracked by slskdarr.
func (m *Metrics) SetUnknownTransfers(n int) { m.UnknownTransfers.Set(float64(n)) }

// SetDownloadsActive records the current count of active downloads.
func (m *Metrics) SetDownloadsActive(n int) { m.DownloadsActive.Set(float64(n)) }

// StatusReport is the read-only snapshot served at /status.
type StatusReport struct {
	Queued  int `json:"queued"`
	Active  int `json:"active"`
	Stalled int `json:"stalled"`
	Parked  int `json:"parked"`
}

// StatusFunc produces a current StatusReport (typically backed by the store).
type StatusFunc func(ctx context.Context) (StatusReport, error)

// jobDTO is the JSON shape served at /api/jobs — a flattened, display-ready
// view of core.JobView so the frontend never needs to know about the
// engine's internal state machine.
type jobDTO struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Status string `json:"status"`
	Peer   string `json:"peer"`
	// BytesDone and BytesTotal are album totals (v.AlbumBytesDone/Total, see
	// core.JobView and issue #174), summed across every file of the job's
	// current candidate — not just the most recently updated transfer — so
	// the frontend's progress bar doesn't jump backwards each time a new
	// file in a multi-track album starts. Zero when the job has no
	// candidate, matching AlbumBytesDone/Total's own zero value.
	//
	// BytesDone is computed as a per-file sum (see jobBytesDone, albumlive.go)
	// rather than served straight from AlbumBytesDone whenever the candidate
	// has at least one live match (issue #161): AlbumBytesDone is only as
	// fresh as the last Downloading reconcile (default 15s), so without this
	// overlay the number visibly jumps once every 15s instead of moving
	// continuously. BytesTotal is deliberately NOT overlaid — it is written
	// ahead from candidate file sizes at activation and does not drift, and
	// overlaying the live counterpart's Size would risk the progress bar's
	// denominator moving.
	BytesDone  int64 `json:"bytesDone"`
	BytesTotal int64 `json:"bytesTotal"`
	// CreatedAt is when the job was first inserted — unlike UpdatedAt it never
	// changes on progress/state updates, so the frontend uses it (not
	// UpdatedAt) to sort the TRANSFERS panel by start order (#233): sorting by
	// UpdatedAt reorders the panel on every progress tick.
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
	State           string  `json:"state"`
	CandidatesTried int     `json:"candidatesTried"`
	MaxCandidates   int     `json:"maxCandidates"`
	FailReason      string  `json:"failReason"`
	NextAttemptAt   string  `json:"nextAttemptAt"`
	Retries         int     `json:"retries"`
	NotBefore       string  `json:"notBefore"`
	Source          string  `json:"source"`
	Year            *int    `json:"year"`
	Tracks          *int    `json:"tracks"`
	Format          *string `json:"format"`
	// QueuePosition and Speed are live, non-persisted values aggregated
	// across every live transfer belonging to the job's current candidate
	// (see aggregateLiveAlbum, issue #157) — album-level analogues of
	// transferDetailDTO's per-file QueuePosition/Speed. omitempty is
	// deliberate for the same reason: a job with no candidate yet, or no
	// currently in-flight transfer, has nothing live to report, so the field
	// is simply absent rather than a misleading zero. ETASeconds divides the
	// store's album-wide AlbumBytesRemaining (see core.JobView, issue #174)
	// by speedAvg — the summed per-transfer SpeedAverage from
	// aggregateLiveAlbum, an EWMA-smoothed rate, NOT the instantaneous Speed
	// field above — so etaSeconds is not simply AlbumBytesRemaining / Speed.
	// Using the store's remaining bytes rather than a live remaining-bytes
	// sum means it accounts for files the per-peer throttle (#20) hasn't
	// released to the peer backend yet. ETASeconds is named with the unit
	// suffix rather than "eta" so it isn't misread as a timestamp; the
	// frontend formats the duration.
	QueuePosition uint32 `json:"queuePosition,omitempty"`
	Speed         int64  `json:"speed,omitempty"`
	ETASeconds    int64  `json:"etaSeconds,omitempty"`
}

// toJobDTO flattens a core.JobView into the dashboard's display-ready shape.
// failedRetryAfter and maxCandidates are engine config values threaded in from
// NewServer, needed to compute nextAttemptAt for FAILED jobs and maxCandidates.
// live supplies the peer backend's current ListDownloads snapshot, indexed
// for album-level aggregation (see aggregateLiveAlbum); its zero value
// (liveTransferIndex{}) is a valid "no live data" index for callers with
// none. persisted supplies each live-matched candidate's exact per-file
// persisted bytes (see jobBytesDone, Store.TransferBytesByCandidate); nil is
// a valid "not fetched" map — every job then simply falls back to its own
// AlbumBytesDone.
func toJobDTO(v core.JobView, failedRetryAfter time.Duration, maxCandidates int, live liveTransferIndex, persisted map[int64]map[string]int64) jobDTO {
	d := jobDTO{
		ID:              v.Job.ID,
		Title:           v.Job.Title,
		Artist:          v.Job.ArtistName,
		Status:          dashboardStatus(v),
		Peer:            v.Peer,
		CreatedAt:       v.Job.CreatedAt.Format(timeFormat),
		UpdatedAt:       v.Job.UpdatedAt.Format(timeFormat),
		State:           string(v.Job.State),
		CandidatesTried: v.Job.CandidatesTried,
		MaxCandidates:   maxCandidates,
		Retries:         v.Job.Retries,
		Source:          string(v.Job.Source),
		Year:            v.Job.Year,
		Tracks:          v.Job.Tracks,
		Format:          v.Job.Format,
		BytesDone:       v.AlbumBytesDone,
		BytesTotal:      v.AlbumBytesTotal,
	}
	if v.Attempt != nil {
		d.FailReason = v.Attempt.FailReason
		// matched (whether any file has a live counterpart at all) is a
		// stream-only concern — see aggregateLiveAlbum's doc comment. REST
		// always reports every job regardless of live data, so it's ignored
		// here.
		speed, speedAvg, queuePosition, hasQueuePosition, _ := aggregateLiveAlbum(v.Attempt, live)
		d.Speed = speed
		if hasQueuePosition {
			d.QueuePosition = queuePosition
		}
		d.ETASeconds = etaSeconds(v.AlbumBytesRemaining, speedAvg)
		d.BytesDone = jobBytesDone(v.Attempt.Username, v.Attempt.Files, v.Attempt.ID, v.AlbumBytesDone, live, persisted)
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

// JobsFunc produces the current complete list of job views (typically backed
// by the store's ListJobsWithTransfer). The stream and legacy all-jobs REST
// endpoint deliberately retain this unpaged dependency.
type JobsFunc func(ctx context.Context) ([]core.JobView, error)

// PagedJobsQuery is the validated persisted-only query for GET /api/jobs.
type PagedJobsQuery struct {
	Page   int64
	Sort   string
	Dir    string
	Filter string
	Source string
	Query  string
}

// JobStatusFacets contains counts for every dashboard status. All ignores the
// selected status while respecting the selected source and search query.
type JobStatusFacets struct {
	All       int64 `json:"all"`
	Active    int64 `json:"active"`
	Importing int64 `json:"importing"`
	Queued    int64 `json:"queued"`
	Stalled   int64 `json:"stalled"`
	Failed    int64 `json:"failed"`
	Parked    int64 `json:"parked"`
	Done      int64 `json:"done"`
}

// JobSourceFacets contains counts for every persisted source. All ignores the
// selected source while respecting the selected status and search query.
type JobSourceFacets struct {
	All    int64 `json:"all"`
	Manual int64 `json:"manual"`
	Lidarr int64 `json:"lidarr"`
}

// JobFacets groups the independent status and source facets.
type JobFacets struct {
	Status JobStatusFacets `json:"status"`
	Source JobSourceFacets `json:"source"`
}

// PagedJobsResult is the persisted store result consumed by GET /api/jobs.
type PagedJobsResult struct {
	Jobs   []core.JobView
	Total  int64
	Facets JobFacets
}

// PagedJobsFunc produces one persisted job page and its consistent counts.
type PagedJobsFunc func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error)

// CancelFunc cancels a job by id (typically backed by app.Jobs.Cancel).
// Errors are mapped to a status code by the /api/jobs/{id}/cancel handler:
// errors.Is(err, app.ErrJobNotFound) -> 404, anything else -> 502.
type CancelFunc func(ctx context.Context, jobID int64) error

// RetryFunc manually revives one FAILED, PARKED, or legacy ORPHANED job by id
// (typically backed by app.Jobs.Retry). Errors are mapped to a status code by the
// /api/jobs/{id}/retry handler: errors.Is(err, app.ErrJobNotFound) -> 404,
// errors.Is(err, app.ErrJobNotRetryable) -> 409, anything else -> 500.
type RetryFunc func(ctx context.Context, jobID int64) error

// SearchJobFunc manually re-queues one job for an immediate re-search
// (typically backed by app.Jobs.ForceSearch; see issue #159). Errors are
// mapped to a status code by the POST /api/jobs/{id}/search handler:
// errors.Is(err, app.ErrJobNotFound) -> 404, errors.Is(err, app.ErrJobActive)
// -> 409, anything else -> 500.
type SearchJobFunc func(ctx context.Context, jobID int64) error

// DeleteJobFunc permanently removes one job and its children (typically
// backed by app.Jobs.Delete; see issue #159). Errors are mapped to a status
// code by the DELETE /api/jobs/{id} handler: errors.Is(err,
// app.ErrJobNotFound) -> 404, errors.Is(err, app.ErrJobImporting) -> 409,
// anything else -> 500.
type DeleteJobFunc func(ctx context.Context, jobID int64) error

// CreateJobFunc manually creates a job that downloads a known peer's files
// directly (typically backed by app.Jobs.Create; see issue #155). Errors are
// mapped to a status code by the POST /api/jobs handler:
// errors.Is(err, app.ErrRemoteFileBusy) -> 409, anything else -> 500.
type CreateJobFunc func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error)

// HealthyFunc reports either liveness or readiness. /healthz uses liveness
// (modules continue attempting work), while /readyz additionally requires
// successful work and tolerates only a short run of consecutive failures.
type HealthyFunc func() bool

// ModuleStatus is the diagnostic runtime state surfaced for one module.
type ModuleStatus struct {
	LastAttempt         time.Time
	LastCompleted       time.Time
	LastSuccess         time.Time
	LastErrorAt         time.Time
	LastError           string
	ConsecutiveFailures int
	StaleDeadline       time.Time
	Live                bool
	Ready               bool
}

// ModulesFunc reports each pipeline module's runtime state for /status.
type ModulesFunc func() map[string]ModuleStatus

// ServerDeps carries everything NewServer needs to wire up the observability
// and dashboard endpoints. Fields are named so a new endpoint adds one line
// here and one line at the call site instead of shifting a positional argument
// list — see issue #169 for why that mattered.
//
// All function fields are required unless documented otherwise; a nil field
// panics on the first request that reaches its handler rather than at
// construction time.
type ServerDeps struct {
	// Registry backs /metrics.
	Registry *prometheus.Registry
	// Version is the build's identity, echoed at GET /status and shown beside
	// the product name in the UI (issue #229). Empty in tests and in any
	// caller that has nothing to report; the UI treats empty as "say nothing"
	// rather than rendering a blank slot.
	Version string
	// Status reports the pipeline snapshot served at /status.
	Status StatusFunc
	// Jobs lists all job views for GET /api/jobs/all and GET /api/stream.
	Jobs JobsFunc
	// PagedJobs backs GET /api/jobs without changing the all-jobs dependency.
	PagedJobs PagedJobsFunc
	// Cancel, Retry, SearchJob and DeleteJob back the per-job actions under
	// /api/jobs/{id}.
	Cancel    CancelFunc
	Retry     RetryFunc
	SearchJob SearchJobFunc
	DeleteJob DeleteJobFunc
	// CreateJob backs POST /api/jobs (manual jobs, see issue #155).
	CreateJob CreateJobFunc
	// JobDetail, JobEvents and RecentEvents back /api/jobs/{id}/detail,
	// /api/jobs/{id}/events and /api/events.
	JobDetail    JobDetailFunc
	JobEvents    JobEventsFunc
	RecentEvents RecentEventsFunc
	// Peers backs /api/peers.
	Peers PeersFunc
	// Live reports liveness for /healthz. Ready reports readiness for
	// /readyz; when nil, Live answers both checks.
	Live  HealthyFunc
	Ready HealthyFunc
	// Modules reports each pipeline module's runtime state for /status.
	Modules ModulesFunc
	// FailedRetryAfter and MaxCandidates are engine values surfaced by the job
	// API: the former computes nextAttemptAt for FAILED jobs, the latter is
	// echoed as maxCandidates.
	FailedRetryAfter time.Duration
	MaxCandidates    int
	// Config supplies the display view of the running configuration served at
	// GET /api/config; it never carries secrets — see AppConfig. ConfigWriter
	// applies a validated settings update from the same path's POST, and
	// Restart is invoked afterward to reload the process with the new config
	// (see cmd/slskdarr/main.go). ConnectionTester backs the settings view's
	// connection checks.
	Config           ConfigFunc
	ConfigWriter     ConfigWriter
	Restart          func()
	ConnectionTester ConnectionTester
	// LiveTransfers enriches /api/jobs/{id}/detail with queue position and
	// speed; failures there degrade to unenriched detail, never an error.
	LiveTransfers LiveTransfersFunc
	// TransferBytes supplies each live-matched candidate's exact per-file
	// persisted bytes for GET /api/jobs and GET /api/stream's live-bytes
	// overlay (issue #161, see jobBytesDone). Only ever called with the
	// candidate ids that actually have a live match, never the whole job
	// list. nil (as in every existing test's ServerDeps) degrades every job
	// to its already-correct, if up-to-15s-stale, AlbumBytesDone.
	TransferBytes TransferBytesFunc
	// Charts supplies the Overview view's chart data served at /api/charts
	// (see ChartsData).
	Charts ChartsFunc
	// Shares reports the native Soulseek share index served at GET
	// /api/shares, and RescanShares backs POST /api/shares/rescan. Both are
	// nil when native Soulseek sharing is not enabled, and their endpoints
	// then answer "not enabled" instead of a misleading zero/failure (see
	// registerShares).
	Shares       SharesFunc
	RescanShares RescanSharesFunc
	// Uploads reports the native Soulseek upload manager's live activity,
	// served at GET /api/uploads (issue #179). nil when native Soulseek
	// sharing is not enabled, mirroring Shares.
	Uploads UploadsFunc
	// Throughput supplies the Overview view's live download-throughput series
	// served at /api/charts (issue #157). nil (the non-native backends, or
	// tests that don't care) yields an empty series rather than omitting the
	// field.
	Throughput ThroughputFunc
	// Conversations and Thread back GET /api/messages and GET
	// /api/messages/{username} (issue #183). Unlike Shares/RescanShares these
	// are wired unconditionally — message history stays readable even when
	// the Soulseek backend that would send new messages is disabled. Send is
	// nil when nothing can send (POST /api/messages/{username} then answers
	// 503); MarkRead backs POST /api/messages/{username}/read.
	Conversations ConversationsFunc
	// ConversationPresence optionally enriches conversation rows with known
	// native Soulseek presence. Nil means unsupported or unknown, so the JSON
	// field is omitted.
	ConversationPresence ConversationPresenceFunc
	Thread               ThreadFunc
	Send                 SendMessageFunc
	MarkRead             MarkReadFunc
	// Shutdown closes GET /api/stream's open SSE connections when the server
	// is stopping (issue #161): without it an open stream keeps its request
	// context alive until the client disconnects, which can block graceful
	// shutdown until lifecycleShutdownTimeout expires. nil (as in every
	// existing test's ServerDeps) simply means "never signal shutdown" — the
	// connection then only ends when the client disconnects, matching
	// today's behavior for every other endpoint.
	Shutdown <-chan struct{}
}

// NewServer returns an http.Handler exposing /metrics, /status, /healthz,
// /readyz, the dashboard APIs, and the dashboard UI. See ServerDeps for what
// each dependency serves.
func NewServer(deps ServerDeps) http.Handler {
	live, ready := deps.Live, deps.Ready
	if ready == nil {
		ready = live
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(deps.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /debug/pprof", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/debug/pprof/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	// Keep non-GET pprof requests from falling through to the path-only UI
	// handler below. GET patterns also accept HEAD, matching net/http semantics.
	methodNotAllowed := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	mux.HandleFunc("/debug/pprof", methodNotAllowed)
	mux.HandleFunc("/debug/pprof/", methodNotAllowed)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !live() {
			// /healthz is public; keep the body generic so it leaks no
			// internal pipeline detail (see ProtectPrivateEndpoints).
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			http.Error(w, "pipeline not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		report, err := deps.Status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type moduleStatusDTO struct {
			LastAttempt         string `json:"lastAttempt"`
			LastCompleted       string `json:"lastCompleted"`
			LastSuccess         string `json:"lastSuccess"`
			LastErrorAt         string `json:"lastErrorAt"`
			LastError           string `json:"lastError"`
			ConsecutiveFailures int    `json:"consecutiveFailures"`
			StaleDeadline       string `json:"staleDeadline"`
			Live                bool   `json:"live"`
			Ready               bool   `json:"ready"`
		}
		moduleTicks := make(map[string]string)
		moduleDetails := make(map[string]moduleStatusDTO)
		for name, module := range deps.Modules() {
			formatted := moduleStatusDTO{
				LastError:           module.LastError,
				ConsecutiveFailures: module.ConsecutiveFailures,
				Live:                module.Live,
				Ready:               module.Ready,
			}
			if !module.LastAttempt.IsZero() {
				formatted.LastAttempt = module.LastAttempt.Format(timeFormat)
			}
			if !module.LastCompleted.IsZero() {
				formatted.LastCompleted = module.LastCompleted.Format(timeFormat)
				moduleTicks[name] = formatted.LastCompleted
			} else {
				moduleTicks[name] = ""
			}
			if !module.LastSuccess.IsZero() {
				formatted.LastSuccess = module.LastSuccess.Format(timeFormat)
			}
			if !module.LastErrorAt.IsZero() {
				formatted.LastErrorAt = module.LastErrorAt.Format(timeFormat)
			}
			if !module.StaleDeadline.IsZero() {
				formatted.StaleDeadline = module.StaleDeadline.Format(timeFormat)
			}
			moduleDetails[name] = formatted
		}
		resp := struct {
			StatusReport
			// Orphaned is a deprecated response alias derived from Parked so the
			// two fields cannot disagree.
			Orphaned      int                        `json:"orphaned"`
			Modules       map[string]string          `json:"modules"`
			ModuleDetails map[string]moduleStatusDTO `json:"moduleDetails"`
			Version       string                     `json:"version"`
		}{
			StatusReport:  report,
			Orphaned:      report.Parked,
			Modules:       moduleTicks,
			ModuleDetails: moduleDetails,
			Version:       deps.Version,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/jobs/all", func(w http.ResponseWriter, r *http.Request) {
		views, err := deps.Jobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(enrichJobDTOs(r.Context(), views, deps))
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			query, err := parsePagedJobsQuery(r.URL)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			result, err := deps.PagedJobs(r.Context(), query)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp := struct {
				Jobs   []jobDTO  `json:"jobs"`
				Total  int64     `json:"total"`
				Facets JobFacets `json:"facets"`
			}{
				Jobs:   enrichJobDTOs(r.Context(), result.Jobs, deps),
				Total:  result.Total,
				Facets: result.Facets,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case http.MethodPost:
			serveCreateJob(w, r, deps.CreateJob, deps.FailedRetryAfter, deps.MaxCandidates)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
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
		err = deps.Cancel(r.Context(), jobID)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, app.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusBadGateway)
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
		err = deps.Retry(r.Context(), jobID)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, app.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, app.ErrJobNotRetryable):
			http.Error(w, "job is not FAILED or PARKED", http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/jobs/{id}/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		err = deps.SearchJob(r.Context(), jobID)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, app.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, app.ErrJobActive):
			http.Error(w, "job is actively transferring", http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("DELETE /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		err = deps.DeleteJob(r.Context(), jobID)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, app.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, app.ErrJobImporting):
			http.Error(w, "job is importing", http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/jobs/{id}/detail", func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		d, found, err := deps.JobDetail(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		// Live queue-position/speed is best-effort cosmetic enrichment: if
		// ListDownloads fails, serve the persisted detail unenriched rather than
		// failing the whole request. Fetched only after the job is found so a 404
		// costs no backend call.
		var live []core.RemoteTransfer
		if lt, liveErr := deps.LiveTransfers(r.Context()); liveErr == nil {
			live = lt
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toJobDetailDTO(d, newLiveTransferIndex(live)))
	})
	mux.HandleFunc("/api/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		events, err := deps.JobEvents(r.Context(), jobID)
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
		events, err := deps.RecentEvents(r.Context(), limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toEventDTOs(events))
	})
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		rows, err := deps.Peers(r.Context())
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
	registerConfig(mux, deps.Config, deps.ConnectionTester, deps.ConfigWriter, deps.Restart)
	registerCharts(mux, deps.Charts, deps.Throughput)
	registerShares(mux, deps.Shares, deps.RescanShares)
	registerUploads(mux, deps.Uploads)
	registerMessages(mux, deps.Conversations, deps.ConversationPresence, deps.Thread, deps.Send, deps.MarkRead)
	registerStream(mux, deps, streamInterval, streamCorrelationInterval, streamHeartbeatInterval)
	mux.Handle("/", newAssetHandler())
	return mux
}

const jobsPageSize int64 = 12

func parsePagedJobsQuery(u *url.URL) (PagedJobsQuery, error) {
	query := PagedJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all"}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return PagedJobsQuery{}, errors.New("invalid query parameters")
	}
	allowed := map[string]struct{}{"page": {}, "sort": {}, "dir": {}, "filter": {}, "source": {}, "q": {}}
	for key, value := range values {
		if _, ok := allowed[key]; !ok {
			return PagedJobsQuery{}, fmt.Errorf("unknown query parameter %q", key)
		}
		if len(value) != 1 {
			return PagedJobsQuery{}, fmt.Errorf("duplicate query parameter %q", key)
		}
	}
	if raw, ok := values["page"]; ok {
		page, parseErr := strconv.ParseInt(raw[0], 10, 64)
		if parseErr != nil || page < 0 || page > (int64(^uint64(0)>>1)/jobsPageSize) {
			return PagedJobsQuery{}, errors.New("invalid page")
		}
		query.Page = page
	}
	if raw, ok := values["sort"]; ok {
		query.Sort = raw[0]
	}
	if raw, ok := values["dir"]; ok {
		query.Dir = raw[0]
	}
	if raw, ok := values["filter"]; ok {
		query.Filter = raw[0]
	}
	if raw, ok := values["source"]; ok {
		query.Source = raw[0]
	}
	if raw, ok := values["q"]; ok {
		query.Query = strings.TrimSpace(raw[0])
	}
	if !oneOf(query.Sort, "st", "album", "peer", "try") {
		return PagedJobsQuery{}, errors.New("invalid sort")
	}
	if !oneOf(query.Dir, "asc", "desc") {
		return PagedJobsQuery{}, errors.New("invalid dir")
	}
	if !oneOf(query.Filter, "all", "active", "importing", "queued", "stalled", "failed", "parked", "done") {
		return PagedJobsQuery{}, errors.New("invalid filter")
	}
	if !oneOf(query.Source, "all", "manual", "lidarr") {
		return PagedJobsQuery{}, errors.New("invalid source")
	}
	return query, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func enrichJobDTOs(ctx context.Context, views []core.JobView, deps ServerDeps) []jobDTO {
	// Live album speed/ETA is best-effort cosmetic enrichment, exactly like
	// the job detail endpoint: a ListDownloads failure degrades to persisted
	// values rather than failing the whole request.
	var live []core.RemoteTransfer
	if deps.LiveTransfers != nil {
		if transfers, err := deps.LiveTransfers(ctx); err == nil {
			live = transfers
		}
	}
	liveIdx := newLiveTransferIndex(live)
	// Exact per-file persisted bytes are fetched only for candidates that have
	// a live match, bounded by concurrent downloads rather than result size.
	var matchedIDs []int64
	for _, view := range views {
		if view.Attempt != nil && anyLiveMatch(view.Attempt.Username, view.Attempt.Files, liveIdx) {
			matchedIDs = append(matchedIDs, view.Attempt.ID)
		}
	}
	var persisted map[int64]map[string]int64
	if len(matchedIDs) > 0 && deps.TransferBytes != nil {
		if bytes, err := deps.TransferBytes(ctx, matchedIDs); err == nil {
			persisted = bytes
		}
	}
	dtos := make([]jobDTO, len(views))
	for i, view := range views {
		dtos[i] = toJobDTO(view, deps.FailedRetryAfter, deps.MaxCandidates, liveIdx, persisted)
	}
	return dtos
}

// createJobFileRequest is one file of a POST /api/jobs request body.
type createJobFileRequest struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// createJobRequest is the POST /api/jobs request body: a manual job download
// directly from a known peer (issue #155). Title/Artist are optional
// free-text display fields; Peer and at least one File are required.
type createJobRequest struct {
	Title  string                 `json:"title"`
	Artist string                 `json:"artist"`
	Peer   string                 `json:"peer"`
	Files  []createJobFileRequest `json:"files"`
}

// validateCreateJobRequest checks a decoded createJobRequest, returning field
// errors keyed the same way as validateConfigUpdate (see errorResponse):
// peer must be non-blank, at least one file is required, and every file must
// have a non-blank, unique filename and a non-negative size.
func validateCreateJobRequest(req createJobRequest) (peer string, files []core.CandidateFile, fieldErrors map[string]string) {
	fieldErrors = make(map[string]string)
	if strings.TrimSpace(req.Peer) == "" {
		fieldErrors["peer"] = "is required"
	}
	if len(req.Files) == 0 {
		fieldErrors["files"] = "at least one file is required"
		return req.Peer, nil, fieldErrors
	}
	seen := make(map[string]struct{}, len(req.Files))
	files = make([]core.CandidateFile, len(req.Files))
	for i, f := range req.Files {
		key := fmt.Sprintf("files[%d]", i)
		if strings.TrimSpace(f.Filename) == "" {
			fieldErrors[key+".filename"] = "is required"
		} else if _, dup := seen[f.Filename]; dup {
			fieldErrors[key+".filename"] = "duplicate filename"
		} else {
			seen[f.Filename] = struct{}{}
		}
		if f.Size < 0 {
			fieldErrors[key+".size"] = "must be >= 0"
		}
		files[i] = core.CandidateFile{Filename: f.Filename, Size: f.Size}
	}
	return req.Peer, files, fieldErrors
}

// serveCreateJob decodes, validates, and creates a manual job (POST
// /api/jobs, issue #155): 400 on malformed JSON, 422 on validation failure,
// 409 if create reports the peer's files are already claimed by another live
// candidate, 500 on any other error, 201 with the created job on success.
func serveCreateJob(w http.ResponseWriter, r *http.Request, create CreateJobFunc, failedRetryAfter time.Duration, maxCandidates int) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConfigError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	peer, files, fieldErrors := validateCreateJobRequest(req)
	if len(fieldErrors) > 0 {
		writeConfigError(w, http.StatusUnprocessableEntity, "validation failed", fieldErrors)
		return
	}

	view, err := create(r.Context(), req.Title, req.Artist, peer, files)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(toJobDTO(view, failedRetryAfter, maxCandidates, liveTransferIndex{}, nil))
	case errors.Is(err, app.ErrRemoteFileBusy):
		writeConfigError(w, http.StatusConflict, err.Error(), nil)
	default:
		writeConfigError(w, http.StatusInternalServerError, "failed to create job", nil)
	}
}
