// Package observ: stream.go serves GET /api/stream (issue #161), a
// server-sent-events endpoint of live dashboard data: whole per-job objects
// for changed jobs; recent directional throughput samples; aggregate current
// download and upload speeds; and, when ?job=<id> is set, that job's whole
// detail body.
//
// Both the job-list half and the scoped detail half carry the FINISHED
// object rather than a partial live-only view (issue #258): built by the
// same toJobDTO/toJobDetailDTO the REST handlers call, so REST and the
// stream can never disagree about a given job, and the frontend replaces its
// cached copy outright instead of merging two partial views field by field —
// the merge that produced four separate regressions after #161. GET always
// carries the whole truth; the stream carries only what changed since a
// subscriber connected (or since ?jobs= last covered a given id — see
// registerStream), and a dropped frame self-heals on the next GET.
//
// The job list REQUIRES an explicit ?jobs=<id,id,...> array (issue #268): a
// subscriber that omits ?jobs= gets no job frames at all — every job-list
// surface (currently just Overview) now knows its own page and publishes it,
// so there is no remaining caller that needs an unbounded/derived set, and
// the endpoint no longer tries to guess one. That set is what makes
// whole-object frames affordable — see streamHub's viewByJob cache below.
// ?job=<id> detail scoping is a separate, independent axis: a connection can
// carry a detail scope, a jobs-list scope, both, or (heartbeats only)
// neither.
//
// Neither shape costs a query per tick: the correlation, the per-candidate
// bytes, the bounded job-view cache and the scoped details are all refreshed
// on streamCorrelationInterval (or sooner — see the event-driven refresh in
// tick), and the 1Hz broadcast reads only what they last stored plus live
// data from memory.
package observ

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// streamInterval is how often the shared broadcaster ticks and considers
// pushing a fresh live snapshot. It deliberately mirrors
// internal/soulseek's defaultThroughputInterval (unexported there, and
// intentionally not promoted to a shared constant: internal/observ does not
// import internal/soulseek — see CLAUDE.md's package boundary) rather than
// being read from config: internal/config rejects unknown keys at startup,
// and a new *required* config key must exist in production's config.toml
// before the PR merges, which a 1s polling cadence isn't worth.
const streamInterval = time.Second

// streamCorrelationInterval is how often the hub refreshes its own
// job<->candidate correlation cache (see jobCorrelation) from deps.Jobs,
// independent of whether any client happens to be polling GET /api/jobs —
// see the #161 review's finding that piggybacking the cache on that
// handler's side effect made GET /api/stream silently degrade whenever
// nobody was polling /api/jobs, and could serve a job's *wrong* candidate
// for a full poll interval after Selecting activates a replacement. It is
// deliberately slower than streamInterval: the correlation only needs to be
// as fresh as the pipeline modules that write it (Selecting activates a new
// candidate on its own multi-second tick), not per-second — except for the
// event-driven refresh in tick, which shortens that window when the set of
// live-matched candidate files itself changes (issue #258).
const streamCorrelationInterval = 5 * time.Second

// streamRetryInterval is the SSE `retry:` value the server suggests to
// EventSource for reconnect delay. It is its own constant, distinct from
// streamInterval, so a backend restart doesn't have every open tab
// reconnecting once a second — see registerStream.
const streamRetryInterval = 5 * time.Second

// streamHeartbeatInterval is how often a bare SSE comment line is sent, so a
// dead connection can't be mistaken for a live one and so intermediary
// proxies don't time out the connection. The heartbeat ticker runs
// unconditionally for the life of the connection — it is not reset by data
// frames — so a busy connection also emits it alongside its live events.
const streamHeartbeatInterval = 15 * time.Second

// streamFetchTimeout bounds each tick's calls into deps.LiveTransfers,
// deps.Throughput and deps.Jobs. Without it, a single hung call (the slskd
// backend is HTTP; a stalled request blocks until its own client timeout)
// would stall the shared broadcaster's one goroutine for every subscriber,
// while heartbeats kept flowing on already-open connections — the stream
// looks alive but is frozen. Well under streamInterval so a timed-out fetch
// still leaves room for the next tick to run on schedule.
const streamFetchTimeout = 500 * time.Millisecond

// streamMaxSubscribers caps concurrent open GET /api/stream connections.
// Reachable unauthenticated whenever cfg.Observ.AuthToken is empty (see
// cmd/slskdarr/main.go's authenticator wiring) — this is not primarily an
// abuse defense but a bound on the shared broadcaster's per-tick fan-out
// work and memory.
const streamMaxSubscribers = 200

// streamMaxJobScope caps the ?jobs= id array a subscriber can request (issue
// #258). Same rationale as streamMaxSubscribers: the array is free for a
// client to send and costly for the hub to serve — a subscriber could
// otherwise request the whole job table, defeating the bound that makes
// whole-object per-job frames affordable (see streamHub.viewByJob).
const streamMaxJobScope = 100

// livePayload is the JSON body of every `event: live` SSE frame.
//
// Jobs carries the whole `jobDTO` — the same shape GET /api/jobs serves —
// for every job that changed since this subscriber's last frame, never the
// full watched set: see buildJobsDelta. Detail, when present, is the whole
// `GET /api/jobs/{id}/detail` body for a ?job=<id> scoped subscriber, built
// by the very same toJobDetailDTO the REST handler calls. Serving finished
// objects rather than a bag of live fields is what lets the frontend replace
// its cached copy outright instead of merging two sources field by field —
// the merge that produced four separate regressions in #161 (see issue
// #258).
//
// Down/Up come from live data read fresh on every tick (deps.LiveTransfers,
// deps.Throughput); Jobs and Detail are built from a cache of persisted data
// refreshed on its own slower timer (streamCorrelationInterval, or sooner —
// see tick's event-driven refresh). The invariant is about *when*, not
// *what*: the broadcaster's per-tick loop never issues a query, though what
// it reads was last derived from one.
type livePayload struct {
	Jobs             []jobDTO              `json:"jobs,omitempty"`
	Detail           *jobDetailDTO         `json:"detail,omitempty"`
	Throughput       []throughputSampleDTO `json:"throughput,omitempty"`
	UploadThroughput []throughputSampleDTO `json:"uploadThroughput,omitempty"`
	Down             int64                 `json:"down"`
	Up               int64                 `json:"up"`
}

// jobCorrelation is the minimal per-job data the stream hub needs to notice
// that the set of live-matched candidate files has changed (see
// liveMatchedFileSet) and to decide which candidates' persisted bytes
// to fetch (see refreshCorrelation): a projection of core.JobView down to
// just those fields, so this cache — which covers every job with a
// candidate, unlike the bounded viewByJob below — doesn't retain a job's
// full attempt history (title, artist, state, every Attempt.Files down to
// metadata this endpoint never serves) for the life of the process. Treat
// every field read-only — files aliases the store's own candidate.Files
// slice.
type jobCorrelation struct {
	id          int64
	candidateID int64
	username    string
	files       []core.CandidateFile
}

// projectJobCorrelation converts the store's full JobView down to
// jobCorrelation, dropping jobs with no candidate — they have nothing live
// to report until Selecting activates one, at which point the next refresh
// (streamCorrelationInterval, or the event-driven one — see tick) picks it up.
func projectJobCorrelation(views []core.JobView) []jobCorrelation {
	out := make([]jobCorrelation, 0, len(views))
	for _, v := range views {
		if v.Attempt == nil {
			continue
		}
		out = append(out, jobCorrelation{
			id:          v.Job.ID,
			candidateID: v.Attempt.ID,
			username:    v.Attempt.Username,
			files:       v.Attempt.Files,
		})
	}
	return out
}

// liveMatchedFileKey identifies one candidate file for liveMatchedFileSet:
// the granularity the event-driven refresh trigger needs to compare tick to
// tick (issue #258 review finding B1).
type liveMatchedFileKey struct {
	candidateID int64
	filename    string
}

// liveMatchedFileSet is the set of (candidateID, filename) pairs among
// cachedJobs' candidate files that currently have a live transfer match, in
// ANY state (mirrors matchFile's use in anyLiveMatch, the same predicate that
// gates fetching a candidate's exact persisted per-file bytes). Comparing
// this set tick to tick — at FILE granularity, not candidate granularity —
// is how the hub notices the two events that invalidate its caches: a brand
// new live match (no viewByJob entry yet) and a single file completing and
// leaving the live list (its persisted bytes are now stale).
//
// File granularity, not candidate granularity, matters for a multi-file
// candidate: anyLiveMatch (and a candidate-level set built from it) stays
// true as long as ANY sibling file of that candidate is still live, so a
// candidate-level comparison would miss exactly the transition where one
// file among several finishes and is purged from ListDownloads while its
// siblings remain live — the common case for a multi-track album, not an
// edge case — and bytesByCandidate would keep serving that one file's stale
// pre-completion bytes for up to a full correlationInterval, reproducing the
// backwards step commit 99fc7aa's now-removed floor existed to paper over.
func liveMatchedFileSet(cachedJobs []jobCorrelation, idx liveTransferIndex) map[liveMatchedFileKey]struct{} {
	out := make(map[liveMatchedFileKey]struct{})
	for _, c := range cachedJobs {
		for _, f := range c.files {
			if _, ok := idx.matchFile(c.username, f.Filename); ok {
				out[liveMatchedFileKey{c.candidateID, f.Filename}] = struct{}{}
			}
		}
	}
	return out
}

// buildJobsDelta computes the whole jobDTOs to send one subscriber this
// tick: every job in the subscriber's own ?jobs= scope (sub.jobIDs) whose
// jobDTO differs from what it was last sent, built by the same toJobDTO
// REST uses so the two transports can never disagree. An id in sub.jobIDs
// with no viewByJob entry yet (the correlation hasn't caught up) is simply
// skipped for this tick, same as every other absence in this file degrades.
//
// sub.jobIDs never changes for the life of a connection (issue #268 — see
// streamSubscriber's doc comment), so unlike the pre-#268 design there is no
// "job left the relevant set" case to correct for: an id is either in
// sub.jobIDs for as long as the connection lives, or it was never in scope
// at all. That is what let the leaving-job correction, the
// previousViewByJob fallback it needed, and scopedJobIDs' trackedIDs all be
// deleted rather than maintained (issue #258 review findings B3/C2/C3
// existed only to correct a set that could shrink).
//
// FramedAt is only ever set to this tick's `now` for a job that is currently
// live-matched (anyLiveMatch on its candidate) — the case #285 actually
// needs: a stalled-but-live job whose other fields tick-to-tick are
// identical still needs a fresh FramedAt, or the client's freshness check
// would incorrectly fall back to REST despite the job still being live. A
// job NOT live-matched this tick (including one that never had a candidate)
// instead keeps whatever FramedAt it was last assigned — from sub.lastJobs,
// or `now` the first time it's ever framed for this subscriber — so once its
// other fields stop changing, reflect.DeepEqual correctly sees no change and
// this function correctly stops resending it, exactly like every other field
// (see issue #285 review: computing one shared `now` per tick regardless of
// live-match status made FramedAt differ every tick for EVERY scoped job,
// including terminal ones, defeating delta encoding's whole purpose by
// resending every job on every tick forever).
//
// Mutates sub.lastJobs to exactly this tick's computed set. Sorted by job id
// for deterministic output.
func buildJobsDelta(sub *streamSubscriber, viewByJob map[int64]core.JobView, idx liveTransferIndex, persisted map[int64]map[string]int64, failedRetryAfter time.Duration, maxCandidates int, now time.Time) []jobDTO {
	nextLast := make(map[int64]jobDTO, len(sub.jobIDs))
	var delta []jobDTO
	for id := range sub.jobIDs {
		view, ok := viewByJob[id]
		if !ok {
			continue
		}
		framedAt := now
		liveMatched := view.Attempt != nil && anyLiveMatch(view.Attempt.Username, view.Attempt.Files, idx)
		if !liveMatched {
			if prev, had := sub.lastJobs[id]; had {
				if t, err := time.Parse(timeFormat, prev.FramedAt); err == nil {
					framedAt = t
				}
			}
		}
		dto := toJobDTO(view, failedRetryAfter, maxCandidates, idx, persisted, framedAt)
		nextLast[id] = dto
		if prev, had := sub.lastJobs[id]; !had || !reflect.DeepEqual(prev, dto) {
			delta = append(delta, dto)
		}
	}
	sub.lastJobs = nextLast

	sort.Slice(delta, func(i, j int) bool { return delta[i].ID < delta[j].ID })
	return delta
}

// buildStreamDetail produces the scoped subscriber's whole job-detail body,
// merging the cached persisted detail and job view with the current live
// transfers through toJobDetailDTO — the same function GET
// /api/jobs/{id}/detail calls, so the two transports cannot describe the
// same job differently. Returns nil when the subscriber is unscoped, or
// either the detail or the view (issue #268 — toJobDetailDTO's embedded
// jobDTO header needs both) hasn't been cached for it yet, which omits the
// field rather than sending an incomplete object the frontend would mistake
// for "this job has no attempts".
func buildStreamDetail(view core.JobView, hasView bool, detail core.JobDetail, hasDetail bool, idx liveTransferIndex, persisted map[int64]map[string]int64, failedRetryAfter time.Duration, maxCandidates int, now time.Time) *jobDetailDTO {
	if !hasDetail || !hasView {
		return nil
	}
	dto := toJobDetailDTO(view, detail, idx, persisted, failedRetryAfter, maxCandidates, now)
	return &dto
}

// downSpeed is the stream's `down` field. It prefers the newest throughput
// sample, which is a MEASURED rate — internal/soulseek's throughputTick sums
// the actual bytesDone deltas over the actual elapsed time — and therefore
// reports 0 the instant transfers stall. sumDownSpeed sums each transfer's
// own speed estimate instead, and ListDownloads keeps serving those for up
// to speedStaleAfter (3s) after a stall, so the estimate overstates exactly
// when someone is most likely watching. Reading the same series the Overview
// sparkline draws also means the header and the graph directly beneath it
// cannot show different numbers for the same quantity.
//
// Falls back to the estimate when there is no series at all: the meter lives
// in the native soulseek client, so cmd/slskdarr/main.go leaves
// ServerDeps.Throughput nil on every other backend, and `down` would
// otherwise read 0 while downloads were plainly running.
//
// Whether the throughput series should survive at all is issue #254; if it
// goes, this reverts to the estimate and `down` gets less accurate, not more.
func downSpeed(samples []core.ThroughputSample, live []core.RemoteTransfer) int64 {
	if n := len(samples); n > 0 {
		// Oldest-first, per ThroughputFunc's doc comment.
		return samples[n-1].BytesPerSecond
	}
	return sumDownSpeed(live)
}

// upSpeed is the stream's global `up` field. Unlike downloads there is no
// non-native transfer estimate to fall back to, so an absent series reports
// explicit zero.
func upSpeed(samples []core.ThroughputSample) int64 {
	if n := len(samples); n > 0 {
		return samples[n-1].BytesPerSecond
	}
	return 0
}

// sumDownSpeed sums the per-transfer speed estimates across every
// non-terminal transfer. Non-terminal only, mirroring aggregateLiveAlbum: a
// lingering terminal transfer's stale speed reading must not inflate the
// total. Used only as downSpeed's fallback — prefer that.
func sumDownSpeed(live []core.RemoteTransfer) int64 {
	var total int64
	for _, lt := range live {
		if lt.State != core.TransferQueued && lt.State != core.TransferInProgress {
			continue
		}
		total += lt.Speed
	}
	return total
}

// buildLiveSnapshot computes one subscriber's non-jobs live fields — down,
// up, and (for a ?job=<id> scoped subscriber) the whole detail body — from
// already-fetched inputs. Jobs is deliberately not built here: unlike
// down/up/detail it is delta-encoded per subscriber rather than a shared
// snapshot (see buildJobsDelta). Pure and I/O-free by construction, so it is
// table-testable without a server.
func buildLiveSnapshot(live []core.RemoteTransfer, jobID int64, throughput core.ThroughputSeries, view core.JobView, hasView bool, detail core.JobDetail, hasDetail bool, idx liveTransferIndex, persisted map[int64]map[string]int64, failedRetryAfter time.Duration, maxCandidates int, now time.Time) livePayload {
	payload := livePayload{
		Down: downSpeed(throughput.Download, live),
		Up:   upSpeed(throughput.Upload),
	}
	if jobID > 0 {
		payload.Detail = buildStreamDetail(view, hasView, detail, hasDetail, idx, persisted, failedRetryAfter, maxCandidates, now)
	}
	return payload
}

// newThroughputSince returns the suffix of samples (oldest-first, per
// ThroughputFunc's doc comment) strictly newer than since. Throughput is the
// one exception to "send only what's changed" (see livePayload's doc
// comment): it's a growing time series, not a state snapshot, so only what a
// subscriber hasn't seen yet is worth sending. Pure — table-tested without a
// server.
func newThroughputSince(samples []core.ThroughputSample, since time.Time) []core.ThroughputSample {
	for i, s := range samples {
		if s.At.After(since) {
			return samples[i:]
		}
	}
	return nil
}

// changedSinceLast decides whether a subscriber needs a fresh live frame:
// a nonzero per-job delta, either directional series gaining a new sample,
// or the down/up/detail snapshot fields differing. newJobCount is a count
// rather than a slice comparison, mirroring newDownloadCount/newUploadCount:
// jobs is itself already a delta by the time this is called (see
// buildJobsDelta), so a length check is the right unit of "changed", not a
// snapshot equality check.
//
// Detail needs reflect.DeepEqual: it is a pointer to a struct of nested
// slices, so neither == nor an element-wise comparison reaches the
// transfers that actually change. This runs once per scoped subscriber per
// tick over one job's attempts, not over the job table.
func changedSinceLast(prev, next livePayload, newJobCount, newDownloadCount, newUploadCount int) bool {
	if newJobCount > 0 || newDownloadCount > 0 || newUploadCount > 0 {
		return true
	}
	if prev.Down != next.Down || prev.Up != next.Up {
		return true
	}
	return !reflect.DeepEqual(prev.Detail, next.Detail)
}

// streamSubscriber is one open GET /api/stream connection's mailbox. ch has
// capacity 1 and holds only the latest undelivered payload (see sendLatest),
// so a slow client can never block the shared broadcaster or build up a
// backlog of stale intermediate states — it just skips straight to whatever
// is current once it catches up.
type streamSubscriber struct {
	ch chan livePayload
	// jobID is the ?job=<id> detail scope (0 for none).
	jobID int64
	// jobIDs is the ?jobs=<id,id,...> job-list scope. nil/empty means no job
	// frames at all (issue #268) — every job-list surface now knows its own
	// page and publishes it, so there is no remaining caller that needs an
	// unbounded/derived set. Never mutated after subscribe(), so
	// buildJobsDelta can range over it directly without copying.
	jobIDs map[int64]struct{}
	// last remembers the down/up/detail fields this subscriber last had
	// queued; directional deltas use independent per-subscriber watermarks,
	// and per-job state uses lastJobs below rather than living here, since
	// jobs is delta- not snapshot-encoded.
	last livePayload
	// lastJobs remembers, per job id, the last whole jobDTO this subscriber
	// was sent (or computed as pending-send) — see buildJobsDelta.
	lastJobs                 map[int64]jobDTO
	lastDownloadThroughputAt time.Time
	lastUploadThroughputAt   time.Time
}

// streamHub is the shared broadcaster behind GET /api/stream: one ticking
// goroutine feeds every open connection, started on the first subscriber and
// stopped on the last (see subscribe/unsubscribe) rather than running for
// the life of the process regardless of whether anyone is watching. The same
// goroutine also owns the job<->candidate correlation cache (see
// refreshCorrelation), refreshed on its own slower timer.
type streamHub struct {
	jobs                JobsFunc
	liveTransfers       LiveTransfersFunc
	throughput          ThroughputFunc
	transferBytes       TransferBytesFunc
	jobDetail           JobDetailFunc
	failedRetryAfter    time.Duration
	maxCandidates       int
	tickInterval        time.Duration
	correlationInterval time.Duration

	corrMu           sync.RWMutex
	correlation      []jobCorrelation
	bytesByCandidate map[int64]map[string]int64
	// detailByJob caches the persisted half of each scoped subscriber's job
	// detail, refreshed on correlationInterval alongside the correlation
	// itself. Keyed by job id and holding only ids someone is watching right
	// now, so it is sized by open detail views rather than by the job table.
	// Treat the stored value as read-only; tick merges live data into a
	// freshly built DTO rather than mutating it.
	detailByJob map[int64]core.JobDetail
	// viewByJob caches each job's full core.JobView — unlike jobCorrelation's
	// deliberately minimal projection, this holds everything toJobDTO/
	// toJobDetailDTO's embedded header need (issue #258): title, artist,
	// state, retries, and the rest. Retaining that for every job in
	// jobCorrelation (every job with a candidate, an unbounded set) was
	// exactly what the original #161 review flagged, so this is bounded
	// instead to exactly what's requested: the union of every subscriber's
	// ?jobs= id set and every ?job=<id> detail scope (issue #268 — a job is
	// no longer added just for being live-matched; nothing reads an
	// unrequested view once the job list requires an explicit scope).
	//
	// The worst case is streamMaxSubscribers * streamMaxJobScope (200 * 100 =
	// 20,000) full core.JobView values with their Attempt.Files slices —
	// every subscriber can independently request a disjoint 100-id set. That
	// is additionally bounded by however many rows album_jobs actually has,
	// and refreshing this cache is not itself an extra query:
	// refreshCorrelation already fetches every job view per refresh
	// regardless of what's wanted, so the requested id sets only add
	// retention cost (which ids are kept afterward), never query cost.
	// Refreshed alongside the correlation.
	viewByJob map[int64]core.JobView
	// matchedFiles is the set of (candidateID, filename) pairs that were
	// live-matched as of the last refresh (see liveMatchedFileSet) —
	// compared tick to tick, at file granularity, to trigger the
	// event-driven refresh below.
	matchedFiles map[liveMatchedFileKey]struct{}

	mu     sync.Mutex
	subs   map[uint64]*streamSubscriber
	nextID uint64
	cancel context.CancelFunc
}

func newStreamHub(jobs JobsFunc, liveTransfers LiveTransfersFunc, throughput ThroughputFunc, transferBytes TransferBytesFunc, jobDetail JobDetailFunc, failedRetryAfter time.Duration, maxCandidates int, tickInterval, correlationInterval time.Duration) *streamHub {
	return &streamHub{
		jobs:                jobs,
		liveTransfers:       liveTransfers,
		throughput:          throughput,
		transferBytes:       transferBytes,
		jobDetail:           jobDetail,
		failedRetryAfter:    failedRetryAfter,
		maxCandidates:       maxCandidates,
		tickInterval:        tickInterval,
		correlationInterval: correlationInterval,
		subs:                make(map[uint64]*streamSubscriber),
	}
}

// fetchLive calls liveTransfers best-effort: nil func or an error both yield
// an empty slice rather than failing the connection — mirroring every other
// best-effort LiveTransfers use in this package (see e.g. the /api/jobs and
// /api/jobs/{id}/detail handlers in observ.go and jobdetail.go).
func (h *streamHub) fetchLive(ctx context.Context) []core.RemoteTransfer {
	if h.liveTransfers == nil {
		return nil
	}
	live, err := h.liveTransfers(ctx)
	if err != nil {
		return nil
	}
	return live
}

func (h *streamHub) fetchThroughput(ctx context.Context) core.ThroughputSeries {
	if h.throughput == nil {
		return core.ThroughputSeries{}
	}
	series, err := h.throughput(ctx)
	if err != nil {
		return core.ThroughputSeries{}
	}
	return series
}

// scopedJobIDs returns the ids used to bound the hub's per-job caches: every
// currently-registered subscriber's ?job=<id> detail scope (detailIDs) and
// ?jobs=<...> list scope (jobArrayIDs) — plus, optionally, the ones a
// subscriber not yet registered would add (extraDetail/extraJobArray), used
// by subscribe() to compute the very first refresh before the new
// subscriber is in h.subs.
//
// Read under h.mu and returned by value so the caller can query with it
// while holding no lock — refreshCorrelation must never hold h.mu and
// corrMu at once, since tick acquires them in the opposite order.
func (h *streamHub) scopedJobIDs(extraDetail int64, extraJobArray map[int64]struct{}) (detailIDs, jobArrayIDs []int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seenDetail := make(map[int64]struct{}, len(h.subs)+1)
	seenArray := make(map[int64]struct{}, len(h.subs)+len(extraJobArray))
	addDetail := func(id int64) {
		if id <= 0 {
			return
		}
		if _, dup := seenDetail[id]; dup {
			return
		}
		seenDetail[id] = struct{}{}
		detailIDs = append(detailIDs, id)
	}
	addArray := func(id int64) {
		if _, dup := seenArray[id]; dup {
			return
		}
		seenArray[id] = struct{}{}
		jobArrayIDs = append(jobArrayIDs, id)
	}
	addDetail(extraDetail)
	for id := range extraJobArray {
		addArray(id)
	}
	for _, sub := range h.subs {
		addDetail(sub.jobID)
		for id := range sub.jobIDs {
			addArray(id)
		}
	}
	return detailIDs, jobArrayIDs
}

// fetchDetails loads the persisted job detail for each scoped id, degrading
// per id rather than replacing the whole cache with a partial result on a
// mid-fetch failure (issue #258 review finding C1). Three outcomes per id:
// success replaces whatever was cached for it; a definite "not found"
// (found=false, no error) drops it — the job genuinely no longer exists; any
// other error (including a context deadline exceeded partway through the
// loop — a real risk now that the event-driven refresh in tick runs under
// tick's own tighter streamFetchTimeout budget, not an untimed context)
// retains PREVIOUS's entry for that id, if any, rather than silently losing
// Detail for a scoped subscriber that was being served fine a moment ago.
func (h *streamHub) fetchDetails(ctx context.Context, ids []int64, previous map[int64]core.JobDetail) map[int64]core.JobDetail {
	if h.jobDetail == nil || len(ids) == 0 {
		return nil
	}
	out := make(map[int64]core.JobDetail, len(ids))
	for _, id := range ids {
		d, found, err := h.jobDetail(ctx, id)
		switch {
		case err == nil && found:
			out[id] = d
		case err == nil && !found:
			// genuinely gone; drop it rather than falling back to a stale copy
		default:
			if prev, ok := previous[id]; ok {
				out[id] = prev
			}
		}
	}
	return out
}

// refreshCorrelation re-derives the hub's persisted-data caches from h.jobs
// (deps.Jobs) — the only place GET /api/stream ever queries Postgres, and
// only on correlationInterval (or the event-driven trigger in tick), never on
// the 1Hz tick itself.
//
// Every cache degrades to its PREVIOUS value on a failed or partial fetch,
// never to nil or a partial result replacing a full one (issue #258 review
// finding C1): a transient Postgres hiccup, or (now that the event-driven
// trigger in tick can call this under tick's own tighter streamFetchTimeout
// budget rather than an untimed context — a new hazard versus origin/main,
// where this ran only from subscribe and run's untimed ticker) a context
// deadline exceeded partway through, should degrade to slightly-stale data,
// not silently wipe out data that was fine a moment ago. h.jobs failing is
// the simplest case (early return leaves every cache untouched); transferBytes
// and jobDetail (via fetchDetails) each need their own fallback because a
// failed fetch there does not stop the rest of this function — see below.
//
// live is the caller's already-fetched ListDownloads snapshot rather than
// one refreshCorrelation fetches itself: tick() calls this only when its own
// live-matched-file comparison (liveMatchedFileSet) already found a mismatch
// against h.matchedFiles, and h.matchedFiles is set from THIS SAME idx below
// — if refreshCorrelation instead took its own fresh fetchLive snapshot, it
// would routinely differ from tick's (both are separate reads of a
// constantly-changing live list during active transfers), so the new
// h.matchedFiles would almost never match tick's next comparison and the
// "at most once per tick" event-driven refresh would fire on every tick,
// inverting the "no DB query on the 1Hz tick" invariant this file documents
// elsewhere.
//
// detailIDs/jobArrayIDs come from scopedJobIDs, and together bound viewByJob
// (issue #268): every job explicitly requested, by either scope, and
// nothing else — a job is no longer added just for being live-matched (see
// viewByJob's doc comment), since with the job list now requiring an
// explicit ?jobs= scope, nothing would ever read an unrequested entry. A job
// that starts a brand new live match between two refreshes, and is already
// in someone's requested scope, simply has no viewByJob entry until the
// next one picks it up (bounded staleness, same as the correlation cache
// itself) — except tick's event-driven refresh, which shortens that window
// to the next tick rather than the next correlationInterval.
//
// It also refreshes h.bytesByCandidate (issue #161's per-file byte overlay,
// see jobBytesDone): this is the ONLY place GET /api/stream fetches
// per-candidate persisted bytes, again only here — the 1Hz tick reads the
// cached map (bytesSnapshot) and never queries. The set of candidate ids
// fetched is restricted to those with a live match right now (anyLiveMatch),
// the same bound /api/jobs applies, so this stays sized by concurrent
// downloads rather than the full job list — independent of detailIDs/
// jobArrayIDs, since a requested job's bytes are worth fetching whether or
// not it happens to be live-matched right now.
func (h *streamHub) refreshCorrelation(ctx context.Context, live []core.RemoteTransfer, detailIDs, jobArrayIDs []int64) {
	if h.jobs == nil {
		return
	}
	views, err := h.jobs(ctx)
	if err != nil {
		return
	}
	corr := projectJobCorrelation(views)
	// Snapshotted before corrMu is (re-)acquired below, so a failed/partial
	// fetch below can fall back to what was cached a moment ago instead of
	// silently losing it (finding C1).
	previousBytes := h.bytesSnapshot()
	previousDetails := h.detailsSnapshot()
	details := h.fetchDetails(ctx, detailIDs, previousDetails)
	idx := newLiveTransferIndex(live)

	bytesByCandidate := previousBytes
	if h.transferBytes != nil {
		var matchedIDs []int64
		for _, c := range corr {
			if anyLiveMatch(c.username, c.files, idx) {
				matchedIDs = append(matchedIDs, c.candidateID)
			}
		}
		if len(matchedIDs) > 0 {
			if m, err := h.transferBytes(ctx, matchedIDs); err == nil {
				bytesByCandidate = m
			}
			// else: keep previousBytes — a fetch was attempted and failed
			// (possibly a context deadline exceeded mid-refresh), so the old
			// map is a better answer than nil or an empty one.
		}
		// else: no live-matched candidates at all right now. bytesByCandidate
		// stays whatever previousBytes was — any entries in it are for
		// candidates that are no longer live-matched, so jobBytesDone (which
		// only ever consults an entry for a candidate that IS live-matched)
		// will never read them; they're simply overwritten on the next
		// refresh that does have a live match to fetch for.
	}

	wanted := make(map[int64]struct{}, len(jobArrayIDs)+len(detailIDs))
	for _, id := range jobArrayIDs {
		wanted[id] = struct{}{}
	}
	for _, id := range detailIDs {
		wanted[id] = struct{}{}
	}
	viewByJob := make(map[int64]core.JobView, len(wanted))
	for _, v := range views {
		if _, requested := wanted[v.Job.ID]; requested {
			viewByJob[v.Job.ID] = v
		}
	}

	h.corrMu.Lock()
	h.correlation = corr
	h.bytesByCandidate = bytesByCandidate
	h.detailByJob = details
	h.viewByJob = viewByJob
	h.matchedFiles = liveMatchedFileSet(corr, idx)
	h.corrMu.Unlock()
}

// detailsSnapshot returns the whole cached detail map. tick takes it once per
// tick rather than calling detailSnapshot per subscriber, so corrMu is
// acquired exactly once and always before h.mu (see scopedJobIDs). The map is
// replaced wholesale by refreshCorrelation and never mutated in place, so
// returning it without copying is safe for readers.
func (h *streamHub) detailsSnapshot() map[int64]core.JobDetail {
	h.corrMu.RLock()
	defer h.corrMu.RUnlock()
	return h.detailByJob
}

// detailSnapshot returns the cached persisted detail for jobID. The bool is
// false for an unscoped subscriber, a job whose detail has not been fetched
// yet, or one that has since disappeared — all of which omit Detail from the
// frame (see buildStreamDetail).
func (h *streamHub) detailSnapshot(jobID int64) (core.JobDetail, bool) {
	if jobID <= 0 {
		return core.JobDetail{}, false
	}
	h.corrMu.RLock()
	defer h.corrMu.RUnlock()
	d, ok := h.detailByJob[jobID]
	return d, ok
}

func (h *streamHub) correlationSnapshot() []jobCorrelation {
	h.corrMu.RLock()
	defer h.corrMu.RUnlock()
	return h.correlation
}

func (h *streamHub) bytesSnapshot() map[int64]map[string]int64 {
	h.corrMu.RLock()
	defer h.corrMu.RUnlock()
	return h.bytesByCandidate
}

// viewsSnapshot returns the whole cached viewByJob map (see streamHub's doc
// comment) without copying — like detailsSnapshot, it's replaced wholesale
// by refreshCorrelation and never mutated in place.
func (h *streamHub) viewsSnapshot() map[int64]core.JobView {
	h.corrMu.RLock()
	defer h.corrMu.RUnlock()
	return h.viewByJob
}

// matchedFilesSnapshot returns the cached live-matched-file set (see
// liveMatchedFileSet) without copying — like detailsSnapshot/viewsSnapshot,
// it's replaced wholesale by refreshCorrelation and never mutated in place,
// so returning it directly is safe for readers.
func (h *streamHub) matchedFilesSnapshot() map[liveMatchedFileKey]struct{} {
	h.corrMu.RLock()
	defer h.corrMu.RUnlock()
	return h.matchedFiles
}

// atCapacity reports whether streamMaxSubscribers open connections are
// already registered. Checked by registerStream before committing to a 200
// response, so a rejection can still be a proper 503 rather than a stream
// that opens and is then immediately torn down.
func (h *streamHub) atCapacity() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) >= streamMaxSubscribers
}

// subscribe registers a new subscriber (jobID 0 for no ?job= detail scope,
// jobIDs nil/empty for no ?jobs= list scope) and starts the shared ticker if
// this is the first one. Returns the subscriber's id (for unsubscribe), its
// channel, and an immediate snapshot computed synchronously against ctx, so
// the connection's very first frame doesn't wait for the next tick —
// including a synchronous correlation refresh, so a subscriber connecting
// right after process start (or a long idle period with no subscribers,
// hence no correlation refresh — see run) doesn't have to wait up to
// correlationInterval for its first useful frame.
func (h *streamHub) subscribe(ctx context.Context, jobID int64, jobIDs map[int64]struct{}) (id uint64, ch chan livePayload, initial livePayload) {
	// jobID/jobIDs are passed explicitly: this subscriber is not in h.subs
	// yet, and without them a freshly opened detail or list view would wait a
	// whole correlationInterval for its first Detail/Jobs.
	live := h.fetchLive(ctx)
	detailIDs, jobArrayIDs := h.scopedJobIDs(jobID, jobIDs)
	h.refreshCorrelation(ctx, live, detailIDs, jobArrayIDs)
	throughput := h.fetchThroughput(ctx)
	liveIdx := newLiveTransferIndex(live)

	views := h.viewsSnapshot()
	persisted := h.bytesSnapshot()
	detail, hasDetail := h.detailSnapshot(jobID)
	view, hasView := views[jobID]

	// This runs outside tick() (a subscriber's first frame, on connect), so
	// it computes its own shared now — see jobDTO.FramedAt.
	now := time.Now()

	sub := &streamSubscriber{
		ch:     make(chan livePayload, 1),
		jobID:  jobID,
		jobIDs: jobIDs,
	}
	jobsDelta := buildJobsDelta(sub, views, liveIdx, persisted, h.failedRetryAfter, h.maxCandidates, now)

	initial = buildLiveSnapshot(live, jobID, throughput, view, hasView, detail, hasDetail, liveIdx, persisted, h.failedRetryAfter, h.maxCandidates, now)
	initial.Jobs = jobsDelta
	initial.Throughput = toThroughputDTO(throughput.Download)
	initial.UploadThroughput = toThroughputDTO(throughput.Upload)

	sub.last = livePayload{Detail: initial.Detail, Down: initial.Down, Up: initial.Up}
	if n := len(throughput.Download); n > 0 {
		sub.lastDownloadThroughputAt = throughput.Download[n-1].At
	}
	if n := len(throughput.Upload); n > 0 {
		sub.lastUploadThroughputAt = throughput.Upload[n-1].At
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	id = h.nextID
	h.nextID++
	h.subs[id] = sub
	if h.cancel == nil {
		tickCtx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.run(tickCtx)
	}
	return id, sub.ch, initial
}

// unsubscribe removes a subscriber and stops the shared ticker once none
// remain.
func (h *streamHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, id)
	if len(h.subs) == 0 && h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

// run is the shared broadcaster goroutine, alive only while at least one
// subscriber is registered (see subscribe/unsubscribe). It ticks two
// independent timers: the live-data tick (tickInterval) and the much slower
// correlation refresh (correlationInterval) — see refreshCorrelation.
func (h *streamHub) run(ctx context.Context) {
	ticker := time.NewTicker(h.tickInterval)
	defer ticker.Stop()
	corrTicker := time.NewTicker(h.correlationInterval)
	defer corrTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.tick(ctx)
		case <-corrTicker.C:
			// ctx here is untimed (unlike tick's fetchCtx below), so a hung
			// h.jobs/h.transferBytes/h.jobDetail call stalls this whole
			// goroutine — including tick's 1Hz broadcast — while heartbeats
			// on already-open connections keep flowing regardless. Tracked
			// as #266; not fixed here.
			live := h.fetchLive(ctx)
			detailIDs, jobArrayIDs := h.scopedJobIDs(0, nil)
			h.refreshCorrelation(ctx, live, detailIDs, jobArrayIDs)
		}
	}
}

// tick fetches the current live state exactly once (never a DB query — see
// livePayload's doc comment) and fans it out to every subscriber, each
// filtered to its own scope and diffed against its own last-known state.
func (h *streamHub) tick(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, streamFetchTimeout)
	defer cancel()
	live := h.fetchLive(fetchCtx)
	throughput := h.fetchThroughput(fetchCtx)
	liveIdx := newLiveTransferIndex(live)
	// One shared now for every subscriber's frame this tick — see
	// jobDTO.FramedAt.
	now := time.Now()

	cachedJobs := h.correlationSnapshot()
	// The set of live-matched candidate FILES changed since the last refresh:
	// either a brand new download started (its job has no viewByJob entry
	// yet) or one file finished and left ListDownloads while its candidate
	// may still have other live files (its persisted bytes are now stale —
	// see liveMatchedFileSet's doc comment for why this must be file-, not
	// candidate-, granular). Both are exactly the events that invalidate this
	// tick's caches, so refresh now instead of waiting out
	// correlationInterval — at most once per tick, whether the refresh
	// itself succeeds or not, so a persistently failing deps.Jobs degrades to
	// refreshing at tick cadence rather than spinning within a single tick.
	// Passes this tick's own already-fetched `live` into refreshCorrelation
	// (see that function's doc comment) rather than letting it fetch its own —
	// otherwise the freshly stored h.matchedFiles would be compared against a
	// *different* live snapshot next tick and rarely match, firing this
	// refresh on every tick during active transfers instead of only on
	// genuine change.
	if !maps.Equal(liveMatchedFileSet(cachedJobs, liveIdx), h.matchedFilesSnapshot()) {
		detailIDs, jobArrayIDs := h.scopedJobIDs(0, nil)
		h.refreshCorrelation(fetchCtx, live, detailIDs, jobArrayIDs)
		cachedJobs = h.correlationSnapshot()
	}
	persisted := h.bytesSnapshot()
	views := h.viewsSnapshot()
	// Snapshotted before h.mu is taken: detailsSnapshot needs corrMu, and
	// tick must never acquire corrMu while holding h.mu (see scopedJobIDs).
	details := h.detailsSnapshot()

	h.mu.Lock()
	defer h.mu.Unlock()
	// ctx is the tick-loop context started by subscribe and stopped by
	// unsubscribe (h.cancel). If the last subscriber left and a new one
	// registered while this tick was parked waiting for h.mu — subscribe
	// starts a new run goroutine on a fresh context the moment the old one's
	// stop overlaps a new subscribe — this tick belongs to the now-defunct
	// goroutine: ctx is already cancelled, fetchLive/fetchThroughput above
	// already returned nil/empty for it, and sending would hand the new
	// subscriber a spurious all-zero frame and clobber its watermarks. See
	// the #161 review's finding #4.
	if ctx.Err() != nil {
		return
	}
	for _, sub := range h.subs {
		detail, hasDetail := details[sub.jobID]
		view, hasView := views[sub.jobID]
		next := buildLiveSnapshot(live, sub.jobID, throughput, view, hasView, detail, hasDetail, liveIdx, persisted, h.failedRetryAfter, h.maxCandidates, now)
		jobsDelta := buildJobsDelta(sub, views, liveIdx, persisted, h.failedRetryAfter, h.maxCandidates, now)
		freshDownload := newThroughputSince(throughput.Download, sub.lastDownloadThroughputAt)
		freshUpload := newThroughputSince(throughput.Upload, sub.lastUploadThroughputAt)
		if !changedSinceLast(sub.last, next, len(jobsDelta), len(freshDownload), len(freshUpload)) {
			continue
		}
		payload := next
		payload.Jobs = jobsDelta
		if len(freshDownload) > 0 {
			payload.Throughput = toThroughputDTO(freshDownload)
		}
		if len(freshUpload) > 0 {
			payload.UploadThroughput = toThroughputDTO(freshUpload)
		}
		sub.last = next
		queued := sendLatest(sub.ch, payload)
		if n := len(queued.Throughput); n > 0 {
			if at, err := time.Parse(timeFormat, queued.Throughput[n-1].At); err == nil {
				sub.lastDownloadThroughputAt = at
			}
		}
		if n := len(queued.UploadThroughput); n > 0 {
			if at, err := time.Parse(timeFormat, queued.UploadThroughput[n-1].At); err == nil {
				sub.lastUploadThroughputAt = at
			}
		}
	}
}

// mergeJobsDelta unions two ticks' worth of per-job deltas by job id, the
// newer entry winning when both changed the same job. Once `jobs` carries
// only changed jobs (not a full snapshot — see livePayload's doc comment),
// sendLatest's ordinary "newest supersedes oldest" replacement would
// silently drop a job that changed on the superseded tick but not the newer
// one, losing an update outright rather than merely delaying it. Sorted by
// job id for deterministic output.
func mergeJobsDelta(old, next []jobDTO) []jobDTO {
	if len(old) == 0 {
		return next
	}
	byID := make(map[int64]jobDTO, len(old)+len(next))
	for _, dto := range old {
		byID[dto.ID] = dto
	}
	for _, dto := range next {
		byID[dto.ID] = dto // newer wins
	}
	merged := make([]jobDTO, 0, len(byID))
	for _, dto := range byID {
		merged = append(merged, dto)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged
}

// sendLatest delivers payload to ch without ever blocking. ch has capacity
// 1; when it already holds an undelivered payload, the fresh snapshot fields
// (Down/Up/Detail) supersede the old ones. Jobs and the two throughput
// fields are delta-encoded, though: Jobs is unioned by id (mergeJobsDelta,
// newer wins) and each old throughput delta is prepended to its matching
// fresh delta, rather than either being discarded. The caller advances each
// watermark from what this function actually leaves queued, so a slow reader
// loses neither direction nor any per-job update.
func sendLatest(ch chan livePayload, payload livePayload) livePayload {
	for {
		select {
		case ch <- payload:
			return payload
		default:
			select {
			case old := <-ch:
				payload.Jobs = mergeJobsDelta(old.Jobs, payload.Jobs)
				payload.Throughput = append(old.Throughput, payload.Throughput...)
				payload.UploadThroughput = append(old.UploadThroughput, payload.UploadThroughput...)
			default:
			}
		}
	}
}

// writeLiveEvent writes one `event: live` SSE frame and flushes it via rc
// (see registerStream's write-deadline comment — the same
// http.ResponseController clears both). Returns false on any write/flush
// error, signaling the caller to give up on this connection.
func writeLiveEvent(w http.ResponseWriter, rc *http.ResponseController, payload livePayload) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: live\ndata: %s\n\n", body); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// parseStreamJobIDs parses the ?jobs= comma-separated job-id list (issue
// #258). An absent or empty parameter is not an error — it means the
// connection carries no job-list scope, so it will simply never receive a
// `jobs` frame (issue #268 — see this file's package comment). Every entry
// must parse as a positive int64; duplicates are silently deduplicated (the
// returned
// set), but a non-numeric or non-positive entry (including a blank one, from
// e.g. a stray comma) is a 400, and so is more than streamMaxJobScope
// distinct ids — the same rationale streamMaxSubscribers has: an id array is
// free for a client to send and expensive for the hub to serve. The cap is
// enforced inside the loop, immediately after each insertion, rather than
// once at the end against the fully-built map: a well-formed request up to
// the server's header-size limit could otherwise force allocating and
// populating a map with hundreds of thousands of entries before being
// rejected.
func parseStreamJobIDs(raw string) (map[int64]struct{}, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make(map[int64]struct{}, min(len(parts), streamMaxJobScope+1))
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid jobs id %q", part)
		}
		out[id] = struct{}{}
		if len(out) > streamMaxJobScope {
			return nil, fmt.Errorf("too many job ids (max %d)", streamMaxJobScope)
		}
	}
	return out, nil
}

// registerStream wires GET /api/stream: named SSE events (`event: live`) of
// live, in-memory-only data. The event name is deliberately not hardcoded
// into a generic "message" frame — issue #129's search-result stream is
// expected to land as `event: search` on this same endpoint later, and named
// events let it do so without touching anything here.
//
// tickInterval/correlationInterval/heartbeatInterval are parameters rather
// than reading the streamInterval/streamCorrelationInterval/
// streamHeartbeatInterval constants directly so tests can use short
// durations instead of the real cadences; NewServer's call site passes the
// real constants.
func registerStream(mux *http.ServeMux, deps ServerDeps, tickInterval, correlationInterval, heartbeatInterval time.Duration) {
	hub := newStreamHub(deps.Jobs, deps.LiveTransfers, deps.Throughput, deps.TransferBytes, deps.JobDetail, deps.FailedRetryAfter, deps.MaxCandidates, tickInterval, correlationInterval)
	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		var jobID int64
		if raw := r.URL.Query().Get("job"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				http.Error(w, "invalid job id", http.StatusBadRequest)
				return
			}
			jobID = id
		}

		jobIDs, err := parseStreamJobIDs(r.URL.Query().Get("jobs"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if hub.atCapacity() {
			http.Error(w, "too many open streams", http.StatusServiceUnavailable)
			return
		}

		rc := http.NewResponseController(w)
		// The server's WriteTimeout (30s, see cmd/slskdarr/main.go) exists to
		// bound ordinary request handlers; an SSE connection is meant to stay
		// open indefinitely, so it must opt out of that deadline per-
		// connection rather than have the server kill it every 30s. If the
		// underlying ResponseWriter doesn't support write deadlines at all
		// (e.g. http.ErrNotSupported), fail the request cleanly now instead
		// of streaming into a connection that's about to be killed anyway.
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		// Without this a reverse proxy in front of slskdarr buffers the
		// stream until it has "enough" data, which for a mostly-idle SSE
		// connection means never.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintf(w, "retry: %d\n\n", streamRetryInterval.Milliseconds()); err != nil || rc.Flush() != nil {
			return
		}

		id, ch, initial := hub.subscribe(r.Context(), jobID, jobIDs)
		defer hub.unsubscribe(id)

		if !writeLiveEvent(w, rc, initial) {
			return
		}

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-deps.Shutdown:
				return
			case payload := <-ch:
				if !writeLiveEvent(w, rc, payload) {
					return
				}
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			}
		}
	})
}
