// Package observ: stream.go serves GET /api/stream (issue #161), a
// server-sent-events endpoint of live, in-memory-only dashboard data: bytes
// done/total, speed, queue position and ETA per job; per-file detail when
// ?job=<id> is set; recent throughput samples; and the aggregate current
// download speed. Everything Postgres-backed keeps being served by REST as
// today — see the design doc referenced from issue #161 for the full
// rationale of that split, and streamHub's correlation cache below for how
// per-job correlation is derived without every tick running its own DB
// query.
package observ

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// streamFileDTO is one file's live state, served in livePayload.Files only
// for a ?job=<id> scoped subscriber. Unlike jobs, files are not filtered to
// non-terminal states — State is exactly the field that tells the frontend a
// file just finished, so a completed/errored file's one lingering tick is
// meaningful, not noise to hide.
type streamFileDTO struct {
	Filename      string `json:"filename"`
	State         string `json:"state"`
	BytesDone     int64  `json:"bytesDone"`
	BytesTotal    int64  `json:"bytesTotal"`
	Speed         int64  `json:"speed,omitempty"`
	QueuePosition uint32 `json:"queuePosition,omitempty"`
}

// livePayload is the JSON body of every `event: live` SSE frame. Every field
// here is populated purely from in-memory data — deps.LiveTransfers and
// deps.Throughput, plus the job<->candidate correlation cached by streamHub
// (see jobCorrelation) — but that cache is itself refreshed from deps.Jobs
// (Postgres) on its own slow timer (streamCorrelationInterval): the
// broadcaster's per-tick loop never issues a query, though the correlation
// it reads was itself last derived from one. There is deliberately no
// status/state/events/peers field at the job level: that is exactly the
// REST/stream split issue #161 draws (see this file's package comment).
type livePayload struct {
	Jobs       []streamJobDTO        `json:"jobs"`
	Files      []streamFileDTO       `json:"files,omitempty"`
	Throughput []throughputSampleDTO `json:"throughput,omitempty"`
	Down       int64                 `json:"down"`
}

// jobCorrelation is the minimal per-job data the stream hub needs to derive
// live per-job/per-file numbers from deps.LiveTransfers: a projection of
// core.JobView down to just what buildStreamJobs/buildStreamFiles read, so
// the hub's correlation cache doesn't retain a job's full attempt history
// (title, artist, state, every Attempt.Files down to metadata this endpoint
// never serves) for the life of the process. Treat every field read-only —
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

func findJobCorrelation(jobs []jobCorrelation, id int64) (jobCorrelation, bool) {
	for _, v := range jobs {
		if v.id == id {
			return v, true
		}
	}
	return jobCorrelation{}, false
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

// buildStreamFiles computes one streamFileDTO per file of job's candidate
// that has a live match. A file not yet enqueued (no live entry at all) is
// simply absent — consistent with livePayload's "absence = removed"
// semantics, same as every other live-set field.
func buildStreamFiles(job jobCorrelation, idx liveTransferIndex) []streamFileDTO {
	out := make([]streamFileDTO, 0, len(job.files))
	for _, f := range job.files {
		lt, ok := idx.matchFile(job.username, f.Filename)
		if !ok {
			continue
		}
		dto := streamFileDTO{
			Filename:   f.Filename,
			State:      string(lt.State),
			BytesDone:  lt.BytesDone,
			BytesTotal: f.Size,
			Speed:      lt.Speed,
		}
		if lt.QueuePosition > 0 {
			dto.QueuePosition = lt.QueuePosition
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// sumDownSpeed is the stream's `down` field: total live download throughput
// summed across every non-terminal transfer, computed directly from
// deps.LiveTransfers with no job correlation needed — unlike buildStreamJobs
// it doesn't depend on the correlation cache, so it is accurate even before
// the first correlation refresh has run. Non-terminal only, mirroring
// aggregateLiveAlbum: a lingering terminal transfer's stale speed reading
// must not inflate the header's total.
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
// Throughput, which is merged in by the caller — see newThroughputSince)
// from already-fetched inputs: cachedJobs (the hub's correlation cache),
// live (this tick's deps.LiveTransfers), and jobID (0 for the unscoped
// dashboard stream, >0 for a ?job= scoped subscriber). persisted is threaded
// through to buildStreamJobs (issue #161). Pure and I/O-free by construction,
// so it is table-testable without a server.
func buildLivePayload(cachedJobs []jobCorrelation, live []core.RemoteTransfer, jobID int64, persisted map[int64]map[string]int64) livePayload {
	idx := newLiveTransferIndex(live)
	payload := livePayload{
		Jobs: buildStreamJobs(cachedJobs, idx, persisted),
		Down: sumDownSpeed(live),
	}
	if jobID > 0 {
		if job, ok := findJobCorrelation(cachedJobs, jobID); ok {
			payload.Files = buildStreamFiles(job, idx)
		}
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
// either its Jobs/Files/Down differ from what it last received (next vs
// prev — Throughput is not part of the comparison; the caller decides that
// via newThroughputCount), or there is at least one new throughput sample —
// throughput can trigger a send even when nothing else changed, per
// newThroughputSince's doc comment. Pure — no I/O — so it is table-testable
// without a server or goroutines.
func changedSinceLast(prev, next livePayload, newThroughputCount int) bool {
	if newThroughputCount > 0 {
		return true
	}
	if prev.Down != next.Down {
		return true
	}
	if !slices.Equal(prev.Jobs, next.Jobs) {
		return true
	}
	return !slices.Equal(prev.Files, next.Files)
}

// streamSubscriber is one open GET /api/stream connection's mailbox. ch has
// capacity 1 and holds only the latest undelivered payload (see sendLatest),
// so a slow client can never block the shared broadcaster or build up a
// backlog of stale intermediate states — it just skips straight to whatever
// is current once it catches up.
type streamSubscriber struct {
	ch    chan livePayload
	jobID int64
	// last remembers the Jobs/Files/Down this subscriber last had queued for
	// it (its own Throughput field is unused — see changedSinceLast), purely
	// so the broadcaster can decide whether the next tick has anything new.
	last             livePayload
	lastThroughputAt time.Time
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
	tickInterval        time.Duration
	correlationInterval time.Duration

	corrMu           sync.RWMutex
	correlation      []jobCorrelation
	bytesByCandidate map[int64]map[string]int64

	mu     sync.Mutex
	subs   map[uint64]*streamSubscriber
	nextID uint64
	cancel context.CancelFunc
}

func newStreamHub(jobs JobsFunc, liveTransfers LiveTransfersFunc, throughput ThroughputFunc, transferBytes TransferBytesFunc, tickInterval, correlationInterval time.Duration) *streamHub {
	return &streamHub{
		jobs:                jobs,
		liveTransfers:       liveTransfers,
		throughput:          throughput,
		transferBytes:       transferBytes,
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

func (h *streamHub) fetchThroughput(ctx context.Context) []core.ThroughputSample {
	if h.throughput == nil {
		return nil
	}
	samples, err := h.throughput(ctx)
	if err != nil {
		return nil
	}
	return samples
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
func (h *streamHub) refreshCorrelation(ctx context.Context) {
	if h.jobs == nil {
		return
	}
	views, err := h.jobs(ctx)
	if err != nil {
		return
	}
	corr := projectJobCorrelation(views)

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
	h.corrMu.Unlock()
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
	h.refreshCorrelation(ctx)
	live := h.fetchLive(ctx)
	throughputSamples := h.fetchThroughput(ctx)

	initial = buildLivePayload(h.correlationSnapshot(), live, jobID, h.bytesSnapshot())
	initial.Throughput = toThroughputDTO(throughputSamples)

	sub := &streamSubscriber{
		ch:    make(chan livePayload, 1),
		jobID: jobID,
		last:  livePayload{Jobs: initial.Jobs, Files: initial.Files, Down: initial.Down},
	}
	if n := len(throughputSamples); n > 0 {
		sub.lastThroughputAt = throughputSamples[n-1].At
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
			h.refreshCorrelation(ctx)
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
	throughputSamples := h.fetchThroughput(fetchCtx)
	cachedJobs := h.correlationSnapshot()
	persisted := h.bytesSnapshot()

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
		payload := buildLivePayload(cachedJobs, live, sub.jobID, persisted)
		fresh := newThroughputSince(throughputSamples, sub.lastThroughputAt)
		next := livePayload{Jobs: payload.Jobs, Files: payload.Files, Down: payload.Down}
		if !changedSinceLast(sub.last, next, len(fresh)) {
			continue
		}
		if len(fresh) > 0 {
			payload.Throughput = toThroughputDTO(fresh)
		}
		sub.last = next
		queued := sendLatest(sub.ch, payload)
		if n := len(queued.Throughput); n > 0 {
			if at, err := time.Parse(timeFormat, queued.Throughput[n-1].At); err == nil {
				sub.lastThroughputAt = at
			}
		}
	}
}

// sendLatest delivers payload to ch without ever blocking. ch has capacity
// 1; when it already holds an undelivered payload, that payload's
// Jobs/Files/Down are a full snapshot that the fresh payload's own
// Jobs/Files/Down simply supersede — but its Throughput is not a snapshot,
// it's a delta-encoded time series (see newThroughputSince), and a sample
// dropped here can never be resent. So the old payload's Throughput is
// prepended to payload's rather than discarded, and the caller (tick)
// advances its watermark from whatever this function actually leaves
// queued — not from what was merely computed this tick — so a slow reader
// accumulates un-acked samples across ticks instead of silently losing them.
func sendLatest(ch chan livePayload, payload livePayload) livePayload {
	for {
		select {
		case ch <- payload:
			return payload
		default:
			select {
			case old := <-ch:
				payload.Throughput = append(old.Throughput, payload.Throughput...)
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
	hub := newStreamHub(deps.Jobs, deps.LiveTransfers, deps.Throughput, deps.TransferBytes, tickInterval, correlationInterval)
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
