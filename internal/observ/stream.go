// Package observ: stream.go serves GET /api/stream (issue #161), a
// server-sent-events endpoint of live dashboard data: bytes done/total,
// speed, queue position and ETA per job; recent directional throughput
// samples; aggregate current download and upload speeds; and, when ?job=<id>
// is set, that job's
// whole detail body.
//
// The job list keeps the #161 split — live fields only, everything
// Postgres-backed served by REST. The scoped detail deliberately does not
// (issue #258): it carries the finished object, persisted fields and all,
// built by the same toJobDetailDTO the REST handler calls, because a frontend
// that merges two partial views of the same object field by field cannot get
// it right — which source is fresher differs per field and per moment.
//
// Neither shape costs a query per tick. See streamHub's caches below: the
// correlation, the per-candidate bytes and the scoped details are all
// refreshed on streamCorrelationInterval, and the 1Hz broadcast reads only
// what they last stored plus live data from memory.
package observ

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
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
// candidate on its own multi-second tick), not per-second.
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

// streamJobDTO is one job's live aggregate, served in livePayload.Jobs.
// Fields mirror jobDTO's own live fields (observ.go) exactly, computed via
// the same aggregateLiveAlbum/jobBytesDone so the REST snapshot and the
// stream can never disagree about a given tick.
type streamJobDTO struct {
	ID            int64  `json:"id"`
	BytesDone     int64  `json:"bytesDone"`
	BytesTotal    int64  `json:"bytesTotal"`
	Speed         int64  `json:"speed,omitempty"`
	QueuePosition uint32 `json:"queuePosition,omitempty"`
	ETASeconds    int64  `json:"etaSeconds,omitempty"`
}

// livePayload is the JSON body of every `event: live` SSE frame.
//
// Its fields come from two places: live data read fresh on every tick
// (deps.LiveTransfers, deps.Throughput) merged with a cache of persisted data
// refreshed on its own slow timer (streamCorrelationInterval) — the
// job<->candidate correlation, per-candidate bytes, and each scoped
// subscriber's job detail. The invariant is about *when*, not *what*: the
// broadcaster's per-tick loop never issues a query, though what it reads was
// last derived from one. See refreshCorrelation.
//
// Detail is the whole `GET /api/jobs/{id}/detail` body, built by the very same
// toJobDetailDTO the REST handler calls, and only for a ?job=<id> scoped
// subscriber. Serving the finished object rather than a bag of live fields is
// what lets the frontend replace its cached detail outright instead of merging
// two sources field by field — the merge that produced four separate
// regressions in #161 (see issue #258).
//
// There is deliberately no status/state/events/peers field at the job level:
// that is exactly the REST/stream split issue #161 draws (see this file's
// package comment).
type livePayload struct {
	Jobs             []streamJobDTO        `json:"jobs"`
	Detail           *jobDetailDTO         `json:"detail,omitempty"`
	Throughput       []throughputSampleDTO `json:"throughput,omitempty"`
	UploadThroughput []throughputSampleDTO `json:"uploadThroughput,omitempty"`
	Down             int64                 `json:"down"`
	Up               int64                 `json:"up"`
}

// jobCorrelation is the minimal per-job data the stream hub needs to derive
// live per-job numbers from deps.LiveTransfers: a projection of core.JobView
// down to just what buildStreamJobs reads, so the correlation cache doesn't
// retain a job's full attempt history (title, artist, state, every
// Attempt.Files down to metadata this endpoint never serves) for the life of
// the process. The cache covers every job with a candidate, so this projection
// is what keeps it from growing with the job table — contrast detailByJob,
// which does hold whole core.JobDetail values but only for the handful of jobs
// a ?job=<id> subscriber is watching right now. Treat every field read-only —
// files aliases the store's own candidate.Files slice.
type jobCorrelation struct {
	id                  int64
	candidateID         int64
	username            string
	files               []core.CandidateFile
	albumBytesDone      int64
	albumBytesTotal     int64
	albumBytesRemaining int64
}

// projectJobCorrelation converts the store's full JobView down to
// jobCorrelation, dropping jobs with no candidate — they have nothing live
// to report until Selecting activates one, at which point the next refresh
// (streamCorrelationInterval) picks it up.
func projectJobCorrelation(views []core.JobView) []jobCorrelation {
	out := make([]jobCorrelation, 0, len(views))
	for _, v := range views {
		if v.Attempt == nil {
			continue
		}
		out = append(out, jobCorrelation{
			id:                  v.Job.ID,
			candidateID:         v.Attempt.ID,
			username:            v.Attempt.Username,
			files:               v.Attempt.Files,
			albumBytesDone:      v.AlbumBytesDone,
			albumBytesTotal:     v.AlbumBytesTotal,
			albumBytesRemaining: v.AlbumBytesRemaining,
		})
	}
	return out
}

// buildStreamJobs computes one streamJobDTO per job that currently has at
// least one matched, non-terminal live transfer (aggregateLiveAlbum's
// matched return) — a job with none has nothing live to report right now,
// and is simply omitted from the set rather than sent with all-zero live
// fields. That omission is a contract with the frontend: absent from `jobs`
// means "no live data currently" and the client must fall back to its
// REST-cached (persisted) bytes/speed/etc for that job, exactly as it would
// before the job's first live tick or after its last one — see livePayload's
// "absence = removed" semantics, and the #161 review's finding #1 for why
// (the unfiltered set was unbounded by the number of jobs ever processed,
// not the number of jobs with something to report). Sorted by job ID so
// tick-to-tick comparison in changedSinceLast is independent of the
// correlation cache's own ordering. persisted supplies each live-matched
// candidate's exact per-file persisted bytes for jobBytesDone (issue #161);
// see streamHub.bytesByCandidate for how/when it's refreshed.
func buildStreamJobs(corr []jobCorrelation, idx liveTransferIndex, persisted map[int64]map[string]int64) []streamJobDTO {
	out := make([]streamJobDTO, 0, len(corr))
	for _, c := range corr {
		candidate := &core.Candidate{Username: c.username, Files: c.files}
		speed, speedAvg, queuePosition, hasQueuePosition, matched := aggregateLiveAlbum(candidate, idx)
		if !matched {
			continue
		}
		dto := streamJobDTO{
			ID:         c.id,
			BytesDone:  jobBytesDone(c.username, c.files, c.candidateID, c.albumBytesDone, idx, persisted),
			BytesTotal: c.albumBytesTotal,
			Speed:      speed,
			ETASeconds: etaSeconds(c.albumBytesRemaining, speedAvg),
		}
		if hasQueuePosition {
			dto.QueuePosition = queuePosition
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildStreamDetail produces the scoped subscriber's whole job-detail body,
// merging the cached persisted detail with the current live transfers through
// toJobDetailDTO — the same function GET /api/jobs/{id}/detail calls, so the
// two transports cannot describe the same job differently. Returns nil when
// the subscriber is unscoped or no detail has been cached for it yet, which
// omits the field rather than sending an empty object the frontend would
// mistake for "this job has no attempts".
func buildStreamDetail(detail core.JobDetail, cached bool, idx liveTransferIndex) *jobDetailDTO {
	if !cached {
		return nil
	}
	dto := toJobDetailDTO(detail, idx)
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

// buildLivePayload computes one subscriber's live snapshot (everything but
// the delta series, which are merged in by the caller — see
// newThroughputSince) from already-fetched inputs. Throughput is always the
// full global series here, never scoped by jobID, because Down and Up read
// their direction's newest entry. Pure and I/O-free by construction, so it
// is table-testable without a server.
func buildLivePayload(cachedJobs []jobCorrelation, live []core.RemoteTransfer, jobID int64, persisted map[int64]map[string]int64, throughput core.ThroughputSeries, detail core.JobDetail, hasDetail bool) livePayload {
	idx := newLiveTransferIndex(live)
	payload := livePayload{
		Jobs: buildStreamJobs(cachedJobs, idx, persisted),
		Down: downSpeed(throughput.Download, live),
		Up:   upSpeed(throughput.Upload),
	}
	if jobID > 0 {
		payload.Detail = buildStreamDetail(detail, hasDetail, idx)
	}
	return payload
}

// newThroughputSince returns the suffix of samples (oldest-first, per
// ThroughputFunc's doc comment) strictly newer than since. Throughput is the
// one exception to "send the whole live set every tick" (see livePayload's
// doc comment): it's a growing time series, not a state snapshot, so only
// what a subscriber hasn't seen yet is worth sending. Pure — table-tested
// without a server.
func newThroughputSince(samples []core.ThroughputSample, since time.Time) []core.ThroughputSample {
	for i, s := range samples {
		if s.At.After(since) {
			return samples[i:]
		}
	}
	return nil
}

// changedSinceLast decides whether a subscriber needs a fresh live frame:
// either its snapshot fields differ or either directional series has a new
// sample. The two counts stay independent so an upload-only sample can
// trigger delivery even when download throughput is unchanged.
//
// Detail needs reflect.DeepEqual rather than slices.Equal: it is a pointer to
// a struct of nested slices, so neither == nor an element-wise comparison
// reaches the transfers that actually change. This runs once per scoped
// subscriber per tick over one job's attempts, not over the job table.
func changedSinceLast(prev, next livePayload, newDownloadCount, newUploadCount int) bool {
	if newDownloadCount > 0 || newUploadCount > 0 {
		return true
	}
	if prev.Down != next.Down || prev.Up != next.Up {
		return true
	}
	if !slices.Equal(prev.Jobs, next.Jobs) {
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
	ch    chan livePayload
	jobID int64
	// last remembers the snapshot fields this subscriber last had queued;
	// directional deltas use independent per-subscriber watermarks.
	last                     livePayload
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
	// lastLiveBytes remembers the highest live BytesDone seen per candidate
	// file, bridging the window where a finished file has left the live set
	// but the cached persisted map has not yet been refreshed. See
	// mergeLiveByteFloor.
	lastLiveBytes map[int64]map[string]int64

	mu     sync.Mutex
	subs   map[uint64]*streamSubscriber
	nextID uint64
	cancel context.CancelFunc
}

func newStreamHub(jobs JobsFunc, liveTransfers LiveTransfersFunc, throughput ThroughputFunc, transferBytes TransferBytesFunc, jobDetail JobDetailFunc, tickInterval, correlationInterval time.Duration) *streamHub {
	return &streamHub{
		jobs:                jobs,
		liveTransfers:       liveTransfers,
		throughput:          throughput,
		transferBytes:       transferBytes,
		jobDetail:           jobDetail,
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

// refreshCorrelation re-derives the job<->candidate correlation cache from
// h.jobs (deps.Jobs) — the only place GET /api/stream ever queries Postgres,
// and only on correlationInterval, never on the 1Hz tick. A nil func or an
// error leaves the existing cache in place rather than clearing it: a
// transient Postgres hiccup should degrade to slightly-stale correlation,
// not to no live data at all.
//
// It also refreshes h.bytesByCandidate (issue #161's per-file byte overlay,
// see jobBytesDone): this is the ONLY place GET /api/stream fetches
// per-candidate persisted bytes, again only on correlationInterval — the 1Hz
// tick reads the cached map (bytesSnapshot) and never queries. The set of
// candidate ids fetched is restricted to those with a live match right now
// (anyLiveMatch), the same bound /api/jobs applies, so this stays sized by
// concurrent downloads rather than the full job list. A brand new live match
// that appears between two correlation refreshes simply falls back to
// AlbumBytesDone (jobBytesDone's nil-map behavior) until the next refresh
// picks it up — bounded staleness, same as the correlation cache itself.
// scopedJobIDs returns the job ids currently being watched by a ?job=<id>
// subscriber. Read under h.mu and returned by value so the caller can query
// with it while holding no lock — refreshCorrelation must never hold h.mu and
// corrMu at once, since tick acquires them in the opposite order.
func (h *streamHub) scopedJobIDs(extra int64) []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[int64]struct{}, len(h.subs)+1)
	var out []int64
	add := func(id int64) {
		if id <= 0 {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	add(extra)
	for _, sub := range h.subs {
		add(sub.jobID)
	}
	return out
}

// fetchDetails loads the persisted job detail for each scoped id, best-effort:
// a nil func, an error or a missing job all just leave that id out of the
// cache, which omits Detail from its frames rather than failing them.
func (h *streamHub) fetchDetails(ctx context.Context, ids []int64) map[int64]core.JobDetail {
	if h.jobDetail == nil || len(ids) == 0 {
		return nil
	}
	out := make(map[int64]core.JobDetail, len(ids))
	for _, id := range ids {
		d, found, err := h.jobDetail(ctx, id)
		if err != nil || !found {
			continue
		}
		out[id] = d
	}
	return out
}

func (h *streamHub) refreshCorrelation(ctx context.Context, scoped []int64) {
	if h.jobs == nil {
		return
	}
	views, err := h.jobs(ctx)
	if err != nil {
		return
	}
	corr := projectJobCorrelation(views)
	details := h.fetchDetails(ctx, scoped)

	var bytesByCandidate map[int64]map[string]int64
	if h.transferBytes != nil {
		idx := newLiveTransferIndex(h.fetchLive(ctx))
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
		}
	}

	h.corrMu.Lock()
	h.correlation = corr
	h.bytesByCandidate = bytesByCandidate
	h.detailByJob = details
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

// mergeLiveByteFloor raises each persisted per-file figure to the highest
// live figure previously observed for that same file, and is what keeps the
// stream's album totals from regressing when a file finishes.
//
// The hub reads two sources that are fresh at different moments: live
// transfers every tick, but persisted bytes only every
// streamCorrelationInterval. jobBytesDone uses the live figure while a file
// is in ListDownloads and the persisted one once it leaves — and reconcile
// purges a file from the live backend immediately after persisting its final
// byte count (internal/pipeline/downloading.go), so the handover is exact
// against *fresh* persisted data. It is not exact against a cached snapshot:
// for up to one refresh interval after a file completes, the cached map still
// holds the pre-completion figure, and the album total drops by whatever the
// file gained in between. Observed in the lab as a ~1.2 MB backwards step two
// ticks after a file finished.
//
// The floor closes that window without another query: the last live figure
// seen for a file is precisely what reconcile persisted before purging it, so
// this restores the value the cache has not caught up to yet rather than
// guessing. Applied only to candidates the persisted map already covers —
// synthesising an entry would flip jobBytesDone out of its albumBytesDone
// fallback on partial data.
func mergeLiveByteFloor(persisted, floor map[int64]map[string]int64) map[int64]map[string]int64 {
	if len(persisted) == 0 || len(floor) == 0 {
		return persisted
	}
	out := make(map[int64]map[string]int64, len(persisted))
	for candidateID, byFilename := range persisted {
		seen := floor[candidateID]
		if len(seen) == 0 {
			out[candidateID] = byFilename
			continue
		}
		merged := make(map[string]int64, len(byFilename))
		for filename, done := range byFilename {
			merged[filename] = done
		}
		for filename, done := range seen {
			if done > merged[filename] {
				merged[filename] = done
			}
		}
		out[candidateID] = merged
	}
	return out
}

// observeLiveBytes records the highest live BytesDone seen per candidate file
// (see mergeLiveByteFloor) and returns the persisted map with that floor
// applied. Entries are dropped once the refreshed persisted map has caught up
// to them, and candidates absent from the current correlation are forgotten
// wholesale, so the memory tracks in-flight downloads rather than growing
// with every file the process has ever seen.
func (h *streamHub) observeLiveBytes(corr []jobCorrelation, idx liveTransferIndex, persisted map[int64]map[string]int64) map[int64]map[string]int64 {
	h.corrMu.Lock()
	defer h.corrMu.Unlock()

	next := make(map[int64]map[string]int64, len(corr))
	for _, c := range corr {
		seen := h.lastLiveBytes[c.candidateID]
		var kept map[string]int64
		for _, f := range c.files {
			done, ok := seen[f.Filename]
			if lt, live := idx.matchFile(c.username, f.Filename); live && lt.BytesDone > done {
				done, ok = lt.BytesDone, true
			}
			if !ok || done <= persisted[c.candidateID][f.Filename] {
				continue // nothing worth remembering, or the cache caught up
			}
			if kept == nil {
				kept = make(map[string]int64)
			}
			kept[f.Filename] = done
		}
		if kept != nil {
			next[c.candidateID] = kept
		}
	}
	h.lastLiveBytes = next
	return mergeLiveByteFloor(persisted, next)
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

// subscribe registers a new subscriber (jobID 0 for the unscoped stream)
// and starts the shared ticker if this is the first one. Returns the
// subscriber's id (for unsubscribe), its channel, and an immediate snapshot
// computed synchronously against ctx, so the connection's very first frame
// doesn't wait for the next tick — including a synchronous correlation
// refresh, so a subscriber connecting right after process start (or a long
// idle period with no subscribers, hence no correlation refresh — see run)
// doesn't have to wait up to correlationInterval for its first useful frame.
func (h *streamHub) subscribe(ctx context.Context, jobID int64) (id uint64, ch chan livePayload, initial livePayload) {
	// jobID is passed explicitly: this subscriber is not in h.subs yet, and
	// without it a freshly opened detail view would wait a whole
	// correlationInterval for its first Detail.
	h.refreshCorrelation(ctx, h.scopedJobIDs(jobID))
	live := h.fetchLive(ctx)
	throughput := h.fetchThroughput(ctx)

	corr := h.correlationSnapshot()
	persisted := h.observeLiveBytes(corr, newLiveTransferIndex(live), h.bytesSnapshot())
	detail, hasDetail := h.detailSnapshot(jobID)
	initial = buildLivePayload(corr, live, jobID, persisted, throughput, detail, hasDetail)
	initial.Throughput = toThroughputDTO(throughput.Download)
	initial.UploadThroughput = toThroughputDTO(throughput.Upload)

	sub := &streamSubscriber{
		ch:    make(chan livePayload, 1),
		jobID: jobID,
		last:  livePayload{Jobs: initial.Jobs, Detail: initial.Detail, Down: initial.Down, Up: initial.Up},
	}
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
			h.refreshCorrelation(ctx, h.scopedJobIDs(0))
		}
	}
}

// tick fetches the current live state exactly once (never a DB query — see
// livePayload's doc comment) and fans it out to every subscriber, each
// filtered to its own jobID and diffed against its own last-known state.
func (h *streamHub) tick(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, streamFetchTimeout)
	defer cancel()
	live := h.fetchLive(fetchCtx)
	throughput := h.fetchThroughput(fetchCtx)
	cachedJobs := h.correlationSnapshot()
	// Raises the cached per-file figures to the highest live figure seen, so
	// a file that finished since the last correlation refresh doesn't read
	// back at its pre-completion size. Takes corrMu, so it must stay outside
	// h.mu below.
	persisted := h.observeLiveBytes(cachedJobs, newLiveTransferIndex(live), h.bytesSnapshot())
	// Snapshotted before h.mu is taken: detailSnapshot needs corrMu, and tick
	// must never acquire corrMu while holding h.mu (see scopedJobIDs).
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
		payload := buildLivePayload(cachedJobs, live, sub.jobID, persisted, throughput, detail, hasDetail)
		freshDownload := newThroughputSince(throughput.Download, sub.lastDownloadThroughputAt)
		freshUpload := newThroughputSince(throughput.Upload, sub.lastUploadThroughputAt)
		next := livePayload{Jobs: payload.Jobs, Detail: payload.Detail, Down: payload.Down, Up: payload.Up}
		if !changedSinceLast(sub.last, next, len(freshDownload), len(freshUpload)) {
			continue
		}
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

// sendLatest delivers payload to ch without ever blocking. ch has capacity
// 1; when it already holds an undelivered payload, the fresh snapshot fields
// supersede the old ones. The two throughput fields are delta-encoded series,
// though, so each old delta is prepended to its matching fresh delta rather
// than discarded. The caller advances each watermark from what this function
// actually leaves queued, so a slow reader loses neither direction.
func sendLatest(ch chan livePayload, payload livePayload) livePayload {
	for {
		select {
		case ch <- payload:
			return payload
		default:
			select {
			case old := <-ch:
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
	hub := newStreamHub(deps.Jobs, deps.LiveTransfers, deps.Throughput, deps.TransferBytes, deps.JobDetail, tickInterval, correlationInterval)
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

		id, ch, initial := hub.subscribe(r.Context(), jobID)
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
