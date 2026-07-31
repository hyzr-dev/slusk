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
	AlbumTracksErrors   prometheus.Counter
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
		AlbumTracksErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slskdarr_album_tracks_errors_total", Help: "Total failed Lidarr AlbumTracks calls during discovery.",
		}),
	}
	reg.MustRegister(m.ReconcileTotal, m.UnknownTransfers, m.DownloadsActive, m.AlbumReleasesErrors, m.AlbumTracksErrors)
	return m
}

// IncReconcile counts one reconciliation pass.
func (m *Metrics) IncReconcile() { m.ReconcileTotal.Inc() }

// IncAlbumReleasesError counts one failed Lidarr AlbumReleases call in Discovery.
func (m *Metrics) IncAlbumReleasesError() { m.AlbumReleasesErrors.Inc() }

// IncAlbumTracksError counts one failed Lidarr AlbumTracks call in Discovery.
func (m *Metrics) IncAlbumTracksError() { m.AlbumTracksErrors.Inc() }

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
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	// FramedAt is when this DTO instance's live data was last genuinely
	// current — not the DB row's UpdatedAt, and not copied from any cache.
	// On REST every job in one response shares one value (see
	// enrichJobDTOs). On the stream path it's per-job: a live-matched job
	// gets the current tick's timestamp (see buildJobsDelta), while a job
	// that isn't live-matched this tick keeps whatever FramedAt it last had,
	// so it correctly ages once it stops changing instead of being
	// perpetually refreshed. The frontend uses it, not UpdatedAt, to decide
	// whether a job's streamed
	// values are still worth trusting over REST's — see replaceLiveJobs in
	// web/src/api/queries.ts and issue #285 for why UpdatedAt could not do
	// this job: REST's and the stream's UpdatedAt values are read from two
	// independently-cached copies of the DB row with different staleness
	// windows, so comparing them measured which cache last happened to read
	// the DB, not which side's data was actually fresher.
	FramedAt        string `json:"framedAt"`
	State           string `json:"state"`
	CandidatesTried int    `json:"candidatesTried"`
	MaxCandidates   int    `json:"maxCandidates"`
	FailReason      string `json:"failReason"`
	// FailDetail is the pipeline's own last recorded failure explanation from
	// job_events (see store.LatestFailureDetails), typically carrying Lidarr's
	// verbatim rejection text. It differs from FailReason above: FailReason is
	// the CURRENT candidate's generic core.Candidate.FailReason (a short,
	// engine-assigned category), while FailDetail is whatever detail string
	// the pipeline actually wrote to the audit trail for the failure — often
	// far more specific. It is populated only on the REST list path (see
	// enrichJobDTOs) and is therefore absent from stream frames.
	FailDetail    string  `json:"failDetail,omitempty"`
	NextAttemptAt string  `json:"nextAttemptAt"`
	Retries       int     `json:"retries"`
	NotBefore     string  `json:"notBefore"`
	Source        string  `json:"source"`
	Year          *int    `json:"year"`
	Tracks        *int    `json:"tracks"`
	Format        *string `json:"format"`
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
// AlbumBytesDone. now is when this DTO instance is being computed — callers
// share one value across every job in the same response/frame so the
// frontend's freshness check compares like with like (see jobDTO.FramedAt).
func toJobDTO(v core.JobView, failedRetryAfter time.Duration, maxCandidates int, live liveTransferIndex, persisted map[int64]map[string]int64, now time.Time) jobDTO {
	d := jobDTO{
		ID:              v.Job.ID,
		Title:           v.Job.Title,
		Artist:          v.Job.ArtistName,
		Status:          v.Status,
		Peer:            v.Peer,
		CreatedAt:       v.Job.CreatedAt.Format(timeFormat),
		UpdatedAt:       v.Job.UpdatedAt.Format(timeFormat),
		FramedAt:        now.Format(timeFormat),
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
// by the store's ListJobsWithTransfer). The stream hub deliberately retains
// this unpaged dependency (issue #268 removed its only other consumer, the
// GET /api/jobs/all REST endpoint).
type JobsFunc func(ctx context.Context) ([]core.JobView, error)

// PagedJobsQuery is the validated persisted-only query for GET /api/jobs.
type PagedJobsQuery struct {
	Page   int64
	Sort   string
	Dir    string
	Filter string
	Source string
	Query  string
	// PageSize is how many jobs one page holds — a bounded parameter (issue
	// #268) so a caller with a smaller, fixed layout (Overview's TRANSFERS
	// panel, 8 rows) can ask for exactly that instead of receiving the
	// dashboard's own page size and truncating client-side. Defaults to
	// jobsPageSize when the request omits it; see parsePagedJobsQuery.
	PageSize int64
	// SkipFacets asks the store to omit the total and facet counts (see
	// store.DashboardJobsQuery.SkipFacets — the facet query is the expensive
	// part of the request and runs regardless of filter). Set by facets=0.
	// A caller that renders a total or facet chips must leave this false.
	SkipFacets bool
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

// FailureDetailsFunc looks up the newest failure-explaining job_events detail
// for each of the given job ids (typically backed by store.LatestFailureDetails),
// keyed by job id. Ids with no matching event are absent from the result.
type FailureDetailsFunc func(ctx context.Context, jobIDs []int64) (map[int64]string, error)

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
	// Jobs lists every job view, unpaged, for GET /api/stream's hub (issue
	// #268 removed its other consumer, GET /api/jobs/all — every REST job
	// list is paged now).
	Jobs JobsFunc
	// PagedJobs backs GET /api/jobs.
	PagedJobs PagedJobsFunc
	// FailureDetails enriches GET /api/jobs' failed rows with jobDTO.FailDetail
	// (issue #310, Overview's Failed panel). A nil func is tolerated — see
	// enrichJobDTOs.
	FailureDetails FailureDetailsFunc
	// Cancel, Retry, SearchJob and DeleteJob back the per-job actions under
	// /api/jobs/{id}.
	Cancel    CancelFunc
	Retry     RetryFunc
	SearchJob SearchJobFunc
	DeleteJob DeleteJobFunc
	// CreateJob backs POST /api/jobs (manual jobs, see issue #155).
	CreateJob CreateJobFunc
	// JobDetail, JobEvents and RecentEvents back /api/jobs/{id}/detail,
	// /api/jobs/{id}/events and /api/events. JobDetail supplies the
	// attempt/transfer history; JobView (below) supplies the same
	// endpoint's job-level header fields.
	JobDetail    JobDetailFunc
	JobEvents    JobEventsFunc
	RecentEvents RecentEventsFunc
	// JobView looks up a single job's live-computed view (typically backed by
	// the store's JobWithTransfer) for /api/jobs/{id}/detail's embedded jobDTO
	// header (issue #268, see jobDetailDTO) — the same shape toJobDTO builds
	// for GET /api/jobs, fetched by id since the detail endpoint no longer has
	// a whole-table result to pick a row out of (GET /api/jobs/all is gone).
	JobView JobViewFunc
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
	// UploadHistory pages the persisted history of finished uploads at GET
	// /api/uploads/history (issue #325). Unlike Uploads it is wired
	// unconditionally: the rows are already in the database, so they stay
	// readable with the native backend switched off. nil answers 503.
	UploadHistory UploadHistoryFunc
	// Throughput supplies the Overview view's live directional throughput
	// series served at /api/charts. nil (the non-native backends, or tests that
	// don't care) yields empty arrays rather than omitting either field.
	Throughput ThroughputFunc
	// StartSearch, SearchSnapshot, SearchDelta and StopSearch back manual
	// Soulseek search (issue #58): POST/GET/DELETE /api/search[/{id}] and the
	// SSE stream's ?search= scope. nil-safe, mirroring Shares/RescanShares —
	// see registerSearch.
	StartSearch    StartSearchFunc
	SearchSnapshot SearchSnapshotFunc
	SearchDelta    SearchDeltaFunc
	StopSearch     StopSearchFunc
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
	// TokenAuth is the TOKEN-ONLY authenticator (observ.NewTokenAuthenticator,
	// NOT the AnyOf-combined instance cmd/slskdarr/main.go wraps the whole
	// handler in via ProtectPrivateEndpoints), threaded in separately so GET
	// /api/auth/session (itself public, see isPrivatePath) can specifically
	// report whether the request carries a valid bearer/Basic token, as
	// opposed to a valid session cookie - see auth.go's registerAuth doc
	// comment for why blurring that distinction would be wrong. nil means no
	// token is configured.
	TokenAuth Authenticator
	// SetupRequired, SessionUser, Setup, Login and Logout back the four
	// public /api/auth/* endpoints (issue #279); see auth.go.
	SetupRequired SetupRequiredFunc
	SessionUser   SessionUserFunc
	Setup         SetupFunc
	Login         LoginFunc
	Logout        LogoutFunc
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
		// view supplies the embedded jobDTO header (issue #268, see
		// jobDetailDTO) — a second, separate lookup since GET /api/jobs/all
		// is gone and there is no whole-table result left to pick a row out
		// of. Fetched only after JobDetail confirms the job exists so an
		// unknown id still costs one query, not two.
		view, viewFound, err := deps.JobView(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !viewFound {
			// The job existed a moment ago (JobDetail found it) but is gone
			// by the time JobView runs — e.g. deleted between the two
			// queries. Treat it the same as never having existed rather
			// than serving a detail body with no header.
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
		idx := newLiveTransferIndex(live)
		persisted := fetchPersistedBytes(r.Context(), []core.JobView{view}, idx, deps.TransferBytes)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toJobDetailDTO(view, d, idx, persisted, deps.FailedRetryAfter, deps.MaxCandidates, time.Now()))
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
	registerAuth(mux, deps.TokenAuth, deps.SetupRequired, deps.SessionUser, deps.Setup, deps.Login, deps.Logout)
	registerConfig(mux, deps.Config, deps.ConnectionTester, deps.ConfigWriter, deps.Restart)
	registerCharts(mux, deps.Charts, deps.Throughput)
	registerShares(mux, deps.Shares, deps.RescanShares)
	registerSearch(mux, deps.StartSearch, deps.SearchSnapshot, deps.StopSearch)
	registerUploads(mux, deps.Uploads, deps.UploadHistory)
	registerMessages(mux, deps.Conversations, deps.ConversationPresence, deps.Thread, deps.Send, deps.MarkRead)
	registerStream(mux, deps, streamInterval, streamCorrelationInterval, streamHeartbeatInterval, streamInvalidateInterval)
	mux.Handle("/", newAssetHandler())
	return mux
}

// jobsPageSize is the default PageSize when a request omits ?pageSize=, and
// the dashboard jobs list's own page size. jobsPageSizeMin/Max bound the
// explicit parameter (issue #268): 1 so a page can never be empty by
// construction, 50 so a caller can't turn a paginated endpoint back into an
// unbounded one by asking for a page the size of the whole table.
const jobsPageSize int64 = 12
const jobsPageSizeMin int64 = 1
const jobsPageSizeMax int64 = 50

func parsePagedJobsQuery(u *url.URL) (PagedJobsQuery, error) {
	query := PagedJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all", PageSize: jobsPageSize}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return PagedJobsQuery{}, errors.New("invalid query parameters")
	}
	allowed := map[string]struct{}{"page": {}, "sort": {}, "dir": {}, "filter": {}, "source": {}, "q": {}, "pageSize": {}, "facets": {}}
	for key, value := range values {
		if _, ok := allowed[key]; !ok {
			return PagedJobsQuery{}, fmt.Errorf("unknown query parameter %q", key)
		}
		if len(value) != 1 {
			return PagedJobsQuery{}, fmt.Errorf("duplicate query parameter %q", key)
		}
	}
	// pageSize is parsed before page: page's own overflow guard below divides
	// by the actual page size in effect, so it must already be resolved.
	if raw, ok := values["pageSize"]; ok {
		pageSize, parseErr := strconv.ParseInt(raw[0], 10, 64)
		if parseErr != nil || pageSize < jobsPageSizeMin || pageSize > jobsPageSizeMax {
			return PagedJobsQuery{}, errors.New("invalid pageSize")
		}
		query.PageSize = pageSize
	}
	if raw, ok := values["page"]; ok {
		page, parseErr := strconv.ParseInt(raw[0], 10, 64)
		if parseErr != nil || page < 0 || page > (int64(^uint64(0)>>1)/query.PageSize) {
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
	// facets=0 opts out of the total and the facet counts; 1 is the default and
	// the only other accepted value. Anything else is rejected rather than
	// coerced, so a typo can't silently drop counts the caller meant to render.
	if raw, ok := values["facets"]; ok {
		switch raw[0] {
		case "0":
			query.SkipFacets = true
		case "1":
			query.SkipFacets = false
		default:
			return PagedJobsQuery{}, errors.New("invalid facets")
		}
	}
	if !oneOf(query.Sort, "st", "album", "peer", "try", "transfer", "recent") {
		return PagedJobsQuery{}, errors.New("invalid sort")
	}
	if !oneOf(query.Dir, "asc", "desc") {
		return PagedJobsQuery{}, errors.New("invalid dir")
	}
	// sort=transfer's whole purpose is a stable status-group-then-age
	// ranking (see dashboardJobsOrder); reversing it with dir=desc would
	// undermine that purpose rather than express a meaningful alternative
	// order, so it is rejected outright rather than silently reinterpreted.
	if query.Sort == "transfer" && query.Dir == "desc" {
		return PagedJobsQuery{}, errors.New("dir=desc is not supported for sort=transfer")
	}
	// sort=recent is newest-first by definition (see store.dashboardJobsOrder);
	// ascending would invert Overview's recently-finished panel. Note that Dir
	// defaults to "asc", so a caller asking for sort=recent must pass dir=desc
	// explicitly rather than relying on the default.
	if query.Sort == "recent" && query.Dir == "asc" {
		return PagedJobsQuery{}, errors.New("dir=asc is not supported for sort=recent")
	}
	// This list is a second, independent copy of the one in
	// store.validateDashboardJobsQuery, and the two must be kept in step: the
	// store's copy is never reached for a value rejected here, so a filter
	// added there but not here is a 400 the store-level tests cannot see
	// (issue #310 shipped exactly that until a lab run caught it).
	// "failures" and "failed" are deliberately both present and deliberately
	// different — see the case comments in store.dashboardJobsWhere.
	if !oneOf(query.Filter, "all", "active", "importing", "queued", "stalled", "failed", "failures", "parked", "done", "inflight", "finished") {
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
	persisted := fetchPersistedBytes(ctx, views, liveIdx, deps.TransferBytes)
	// One shared now for the whole response — see jobDTO.FramedAt.
	now := time.Now()
	dtos := make([]jobDTO, len(views))
	for i, view := range views {
		dtos[i] = toJobDTO(view, deps.FailedRetryAfter, deps.MaxCandidates, liveIdx, persisted, now)
	}
	// FailDetail enrichment is best-effort, exactly like live album speed/ETA
	// above: a lookup failure degrades to no detail rather than failing the
	// whole request. It is REST-only — internal/observ/stream.go calls
	// toJobDTO directly and deliberately does not do this, since terminal jobs
	// are out of the stream's scope — so a job that just failed but is still
	// rendered from a stale live frame shows no reason for up to
	// LIVE_JOB_FRESH_MS; the next REST poll (this path) corrects it.
	if deps.FailureDetails != nil {
		var failedIDs []int64
		for _, view := range views {
			if view.Status == "failed" {
				failedIDs = append(failedIDs, view.Job.ID)
			}
		}
		if len(failedIDs) > 0 {
			if details, err := deps.FailureDetails(ctx, failedIDs); err == nil {
				for i, view := range views {
					if detail, ok := details[view.Job.ID]; ok {
						dtos[i].FailDetail = detail
					}
				}
			}
		}
	}
	return dtos
}

// fetchPersistedBytes fetches per-file persisted bytes-done for exactly the
// candidates among views that have at least one live match, in ANY state
// (see anyLiveMatch) — the same bound jobBytesDone's overlay always used
// (issue #161), factored out so the paged/all-jobs list (enrichJobDTOs) and
// the single-job detail header (/api/jobs/{id}/detail, issue #268) fetch it
// identically instead of drifting apart. Best-effort: a nil TransferBytes,
// no live-matched candidates, or a failed fetch all yield a nil map, which
// jobBytesDone/toJobDTO already treat as "fall back to AlbumBytesDone".
func fetchPersistedBytes(ctx context.Context, views []core.JobView, idx liveTransferIndex, transferBytes TransferBytesFunc) map[int64]map[string]int64 {
	if transferBytes == nil {
		return nil
	}
	var matchedIDs []int64
	for _, view := range views {
		if view.Attempt != nil && anyLiveMatch(view.Attempt.Username, view.Attempt.Files, idx) {
			matchedIDs = append(matchedIDs, view.Attempt.ID)
		}
	}
	if len(matchedIDs) == 0 {
		return nil
	}
	bytes, err := transferBytes(ctx, matchedIDs)
	if err != nil {
		return nil
	}
	return bytes
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
		_ = json.NewEncoder(w).Encode(toJobDTO(view, failedRetryAfter, maxCandidates, liveTransferIndex{}, nil, time.Now()))
	case errors.Is(err, app.ErrRemoteFileBusy):
		writeConfigError(w, http.StatusConflict, err.Error(), nil)
	default:
		writeConfigError(w, http.StatusInternalServerError, "failed to create job", nil)
	}
}
