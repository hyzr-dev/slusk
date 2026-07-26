// Package observ: stream.go serves GET /api/stream (issue #161), a
// server-sent-events endpoint of live, in-memory-only dashboard data: bytes
// done/total, speed, queue position and ETA per job; per-file detail when
// ?job=<id> is set; recent throughput samples; and the aggregate current
// download speed. Everything Postgres-backed keeps being served by REST as
// today — see the design doc referenced from issue #161 for the full
// rationale of that split, and jobLiveIndex below for how per-job
// correlation is derived without the stream ever running its own DB query.
package observ

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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

// streamHeartbeatInterval is how often an otherwise-idle stream (nothing
// changed since the last tick) gets a bare SSE comment line, so a dead
// connection can't be mistaken for a quiet one and so intermediary proxies
// don't time out the idle connection.
const streamHeartbeatInterval = 15 * time.Second

// streamJobDTO is one job's live aggregate, served in livePayload.Jobs.
// Fields mirror jobDTO's own live fields (observ.go) exactly, computed via
// the same aggregateLiveAlbum/overlayBytesDone so the REST snapshot and the
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

// streamThroughputDTO is one throughput sample, served in
// livePayload.Throughput. Same shape as charts.go's throughputSampleDTO;
// kept as its own type rather than reused so the two packages' JSON shapes
// can evolve independently even though they start identical.
type streamThroughputDTO struct {
	At              string `json:"at"`
	BytesPerSecond  int64  `json:"bytesPerSecond"`
	ActiveTransfers int    `json:"activeTransfers"`
}

// livePayload is the JSON body of every `event: live` SSE frame. Every field
// here must be answerable from in-memory data only — deps.LiveTransfers and
// deps.Throughput, plus the job<->candidate correlation cached in
// jobLiveIndex — never a fresh Postgres query. There is deliberately no
// status/state/events/peers field at the job level: that is exactly the
// REST/stream split issue #161 draws (see this file's package comment).
type livePayload struct {
	Jobs       []streamJobDTO        `json:"jobs"`
	Files      []streamFileDTO       `json:"files,omitempty"`
	Throughput []streamThroughputDTO `json:"throughput,omitempty"`
	Down       int64                 `json:"down"`
}

// jobLiveIndex caches the job<->candidate correlation the SSE broadcaster
// needs to turn deps.LiveTransfers (peer + filename, keyed on nothing a
// stream client recognizes) into per-job/per-file numbers, without the
// broadcaster ever running a DB query of its own: GET /api/jobs already
// computes exactly this correlation on every poll (see NewServer's handler),
// and set is called there as a side effect. Between process start (or a
// server restart) and the first GET /api/jobs, the stream simply has an
// empty job list to report — not an error, and never observed in practice
// since the frontend always fetches the jobs list on mount before or
// alongside opening the stream.
type jobLiveIndex struct {
	mu   sync.RWMutex
	jobs []core.JobView
}

func (idx *jobLiveIndex) set(jobs []core.JobView) {
	idx.mu.Lock()
	idx.jobs = jobs
	idx.mu.Unlock()
}

func (idx *jobLiveIndex) snapshot() []core.JobView {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.jobs
}

func findJobView(jobs []core.JobView, id int64) (core.JobView, bool) {
	for _, v := range jobs {
		if v.Job.ID == id {
			return v, true
		}
	}
	return core.JobView{}, false
}

// buildStreamJobs computes one streamJobDTO per job that currently has a
// candidate — a job with none has nothing live to report, matching jobDTO's
// own omitempty rationale. Sorted by job ID so tick-to-tick comparison in
// changedSinceLast is independent of jobLiveIndex's own ordering (GET
// /api/jobs sorts by updated_at, which reorders on every write).
func buildStreamJobs(cachedJobs []core.JobView, idx liveTransferIndex) []streamJobDTO {
	out := make([]streamJobDTO, 0, len(cachedJobs))
	for _, v := range cachedJobs {
		if v.Attempt == nil {
			continue
		}
		speed, speedAvg, queuePosition, hasQueuePosition, liveBytesDone := aggregateLiveAlbum(v.Attempt, idx)
		dto := streamJobDTO{
			ID:         v.Job.ID,
			BytesDone:  overlayBytesDone(v.AlbumBytesDone, v.AlbumBytesDoneNonTerminal, liveBytesDone),
			BytesTotal: v.AlbumBytesTotal,
			Speed:      speed,
			ETASeconds: etaSeconds(v.AlbumBytesRemaining, speedAvg),
		}
		if hasQueuePosition {
			dto.QueuePosition = queuePosition
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildStreamFiles computes one streamFileDTO per file of job's current
// candidate that has a live match. A file not yet enqueued (no live entry at
// all) is simply absent — consistent with livePayload's "absence = removed"
// semantics, same as every other live-set field.
func buildStreamFiles(job core.JobView, idx liveTransferIndex) []streamFileDTO {
	if job.Attempt == nil {
		return nil
	}
	out := make([]streamFileDTO, 0, len(job.Attempt.Files))
	for _, f := range job.Attempt.Files {
		lt, ok := idx.byFallback[job.Attempt.Username+"\x00"+f.Filename]
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
// it doesn't depend on jobLiveIndex, so it is accurate even before the first
// GET /api/jobs poll has populated the cache. Non-terminal only, mirroring
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
// from already-fetched inputs: cachedJobs (from jobLiveIndex), live (this
// tick's deps.LiveTransfers), and jobID (0 for the unscoped dashboard
// stream, >0 for a ?job= scoped subscriber). Pure and I/O-free by
// construction, so it is table-testable without a server.
func buildLivePayload(cachedJobs []core.JobView, live []core.RemoteTransfer, jobID int64) livePayload {
	idx := newLiveTransferIndex(live)
	payload := livePayload{
		Jobs: buildStreamJobs(cachedJobs, idx),
		Down: sumDownSpeed(live),
	}
	if jobID > 0 {
		if job, ok := findJobView(cachedJobs, jobID); ok {
			payload.Files = buildStreamFiles(job, idx)
		}
	}
	return payload
}

// toStreamThroughputDTO formats throughput samples for the wire, same
// shape/format as charts.go's toThroughputDTO.
func toStreamThroughputDTO(samples []core.ThroughputSample) []streamThroughputDTO {
	out := make([]streamThroughputDTO, len(samples))
	for i, s := range samples {
		out[i] = streamThroughputDTO{At: s.At.Format(timeFormat), BytesPerSecond: s.BytesPerSecond, ActiveTransfers: s.ActiveTransfers}
	}
	return out
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
// either its per-job/per-file/down state differs from what it last
// received, or there is at least one new throughput sample (newThroughputCount
// > 0) — throughput can trigger a send even when nothing else changed, per
// newThroughputSince's doc comment. Pure — no I/O — so it is table-testable
// without a server or goroutines.
func changedSinceLast(prevJobs, nextJobs []streamJobDTO, prevFiles, nextFiles []streamFileDTO, prevDown, nextDown int64, newThroughputCount int) bool {
	if newThroughputCount > 0 {
		return true
	}
	if prevDown != nextDown {
		return true
	}
	if !reflect.DeepEqual(prevJobs, nextJobs) {
		return true
	}
	return !reflect.DeepEqual(prevFiles, nextFiles)
}

// streamSubscriber is one open GET /api/stream connection's mailbox. ch has
// capacity 1 and holds only the latest undelivered payload (see sendLatest),
// so a slow client can never block the shared broadcaster or build up a
// backlog of stale intermediate states — it just skips straight to whatever
// is current once it catches up.
type streamSubscriber struct {
	ch    chan livePayload
	jobID int64
	// last* remember what this subscriber last received, purely so the
	// broadcaster can decide (via changedSinceLast) whether the next tick has
	// anything new for it.
	lastJobs         []streamJobDTO
	lastFiles        []streamFileDTO
	lastDown         int64
	lastThroughputAt time.Time
}

// streamHub is the shared broadcaster behind GET /api/stream: one ticking
// goroutine feeds every open connection, started on the first subscriber and
// stopped on the last (see subscribe/unsubscribe) rather than running for
// the life of the process regardless of whether anyone is watching.
type streamHub struct {
	liveTransfers LiveTransfersFunc
	throughput    ThroughputFunc
	jobIndex      *jobLiveIndex
	tickInterval  time.Duration

	mu     sync.Mutex
	subs   map[uint64]*streamSubscriber
	nextID uint64
	cancel context.CancelFunc
}

func newStreamHub(liveTransfers LiveTransfersFunc, throughput ThroughputFunc, jobIndex *jobLiveIndex, tickInterval time.Duration) *streamHub {
	return &streamHub{
		liveTransfers: liveTransfers,
		throughput:    throughput,
		jobIndex:      jobIndex,
		tickInterval:  tickInterval,
		subs:          make(map[uint64]*streamSubscriber),
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

// subscribe registers a new subscriber (jobID 0 for the unscoped stream)
// and starts the shared ticker if this is the first one. Returns the
// subscriber's id (for unsubscribe), its channel, and an immediate snapshot
// computed synchronously against ctx, so the connection's very first frame
// doesn't wait for the next tick.
func (h *streamHub) subscribe(ctx context.Context, jobID int64) (id uint64, ch chan livePayload, initial livePayload) {
	live := h.fetchLive(ctx)
	throughputSamples := h.fetchThroughput(ctx)

	initial = buildLivePayload(h.jobIndex.snapshot(), live, jobID)
	initial.Throughput = toStreamThroughputDTO(throughputSamples)

	sub := &streamSubscriber{
		ch:        make(chan livePayload, 1),
		jobID:     jobID,
		lastJobs:  initial.Jobs,
		lastFiles: initial.Files,
		lastDown:  initial.Down,
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
// subscriber is registered (see subscribe/unsubscribe).
func (h *streamHub) run(ctx context.Context) {
	ticker := time.NewTicker(h.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.tick(ctx)
		}
	}
}

// tick fetches the current live state exactly once (never a DB query — see
// livePayload's doc comment) and fans it out to every subscriber, each
// filtered to its own jobID and diffed against its own last-known state.
func (h *streamHub) tick(ctx context.Context) {
	live := h.fetchLive(ctx)
	throughputSamples := h.fetchThroughput(ctx)
	cachedJobs := h.jobIndex.snapshot()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		payload := buildLivePayload(cachedJobs, live, sub.jobID)
		fresh := newThroughputSince(throughputSamples, sub.lastThroughputAt)
		if !changedSinceLast(sub.lastJobs, payload.Jobs, sub.lastFiles, payload.Files, sub.lastDown, payload.Down, len(fresh)) {
			continue
		}
		if len(fresh) > 0 {
			payload.Throughput = toStreamThroughputDTO(fresh)
			sub.lastThroughputAt = fresh[len(fresh)-1].At
		}
		sub.lastJobs, sub.lastFiles, sub.lastDown = payload.Jobs, payload.Files, payload.Down
		sendLatest(sub.ch, payload)
	}
}

// sendLatest delivers payload to ch without ever blocking: ch has capacity
// 1, and when it's already holding an undelivered payload, that stale
// payload is dropped in favor of the fresh one rather than queuing or
// blocking the shared broadcaster on one slow client (see streamSubscriber's
// doc comment).
func sendLatest(ch chan livePayload, payload livePayload) {
	for {
		select {
		case ch <- payload:
			return
		default:
			select {
			case <-ch:
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
// tickInterval/heartbeatInterval are parameters rather than reading the
// streamInterval/streamHeartbeatInterval constants directly so tests can use
// short durations instead of the real 1s/15s cadence; NewServer's call site
// passes the real constants.
func registerStream(mux *http.ServeMux, deps ServerDeps, jobIndex *jobLiveIndex, shutdown <-chan struct{}, tickInterval, heartbeatInterval time.Duration) {
	hub := newStreamHub(deps.LiveTransfers, deps.Throughput, jobIndex, tickInterval)
	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		var jobID int64
		if raw := r.URL.Query().Get("job"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				http.Error(w, "invalid job id", http.StatusBadRequest)
				return
			}
			jobID = id
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
		if _, err := fmt.Fprintf(w, "retry: %d\n\n", tickInterval.Milliseconds()); err != nil || rc.Flush() != nil {
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
			case <-shutdown:
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
