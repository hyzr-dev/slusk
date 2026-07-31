// Package app: search.go hosts the manual Soulseek search session registry
// (issue #58) — a transport-neutral service, exactly like Jobs, that sits
// between the peer backend and internal/observ's HTTP/SSE edge. Search
// sessions are entirely in-memory: nothing here is persisted, and nothing
// here touches internal/store.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
)

// ErrSearchBusy is returned by Start when searchMaxSessions live
// (not-yet-finished) sessions are already running process-wide. Each native
// search reserves a Soulseek token and broadcasts to the whole distributed
// network — a real resource this user-triggerable endpoint must bound (see
// issue #106).
var ErrSearchBusy = errors.New("too many concurrent searches")

// ErrSearchNotFound documents the "no such session" outcome the observ HTTP
// layer maps to 404. Snapshot/Delta/Stop themselves report this as a bool
// rather than this error (there is nothing to wrap), so this sentinel is
// declared for symmetry with the rest of the app package's error vocabulary
// and for any future caller that does want to return it as an error.
var ErrSearchNotFound = errors.New("search session not found")

// ErrSearchUnavailable is returned by Start when no peer backend is wired
// (Searches.Peers is nil) — mirrors registerShares' nil-safe convention:
// searching is a capability that can be entirely absent from a build/config,
// not just momentarily busy.
var ErrSearchUnavailable = errors.New("search is not available")

// ErrSearchQueryInvalid is returned by Start when the query is blank or
// exceeds the accepted length.
var ErrSearchQueryInvalid = errors.New("search query is required and must be 256 characters or fewer")

// searchQueryMaxLen bounds POST /api/search's query field.
const searchQueryMaxLen = 256

// searchMaxSessions caps concurrently LIVE (not-yet-finished) sessions
// process-wide. A finished session lingering for its display TTL does not
// count against this cap — see evictLocked/liveCountLocked.
const searchMaxSessions = 8

// searchMaxResults caps accepted core.SearchResult files per session,
// mirroring internal/soulseek's own maxSearchResults. Once reached, further
// results are dropped and the session's Truncated flag is set rather than
// silently under-reporting.
const searchMaxResults = 2000

// searchSessionTTL is how long a FINISHED session's snapshot/delta stays
// available after FinishedAt, for a client that reconnects or polls late.
// Eviction is lazy — swept at the top of Start and Snapshot, not by a
// janitor goroutine — so the bound on total memory is simply
// searchMaxSessions * searchMaxResults, a few MB, and a never-polled session
// costs nothing beyond that until the next Start/Snapshot call sweeps it.
const searchSessionTTL = 5 * time.Minute

// searchForceCancelGrace is added to the caller's requested timeout to bound
// how long a still-running session's goroutine is allowed to run before
// Searches force-cancels it — a safety net independent of whatever timeout
// behavior the peer backend itself honors.
const searchForceCancelGrace = 60 * time.Second

// PeerStreamSearcher is the slice of the peer backend a manual search needs.
// Both concrete backends (internal/soulseek.Client and internal/slskd.Client)
// satisfy this implicitly, same as pipeline.PeerSearcher.
//
// Contract:
//   - emit is called from the implementation's own goroutine, never
//     concurrently with itself, and never after SearchStream returns.
//   - emit receives a slice the callee no longer owns; the caller may retain it.
//   - SearchStream returns nil when the timeout elapsed normally (including
//     everything already emitted); it returns the failure (with everything
//     already emitted retained by the caller) on ctx cancellation or
//     connection loss.
//   - SearchStreaming reports whether results genuinely arrive incrementally
//     (native: true) or only as one batch at completion (slskd: false).
type PeerStreamSearcher interface {
	SearchStream(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error
	SearchStreaming() bool
}

// SearchesParams configures NewSearches.
type SearchesParams struct {
	// Peers is the backend a search runs against. nil is tolerated: Start
	// then always answers ErrSearchUnavailable, mirroring registerShares.
	Peers PeerStreamSearcher
	// Root is the process root context every session goroutine is derived
	// from — deliberately NOT a per-request context, which is cancelled the
	// instant the HTTP handler that created the session returns.
	Root context.Context
	// Timeout is how long one search runs before completing normally
	// (cfg.Pipeline.SearchTimeout — no new config key is introduced for this).
	Timeout time.Duration
	Logger  *slog.Logger
}

// Searches is the transport-neutral service backing manual Soulseek search
// (issue #58): a bounded, TTL-evicted, in-memory registry of search sessions.
type Searches struct {
	peers   PeerStreamSearcher
	root    context.Context
	timeout time.Duration
	logger  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*searchSession
}

// NewSearches constructs a Searches service.
func NewSearches(p SearchesParams) *Searches {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Searches{
		peers:    p.Peers,
		root:     p.Root,
		timeout:  p.Timeout,
		logger:   logger,
		sessions: make(map[string]*searchSession),
	}
}

// Start begins a new manual search: reserves a session slot, kicks off the
// backend search on a goroutine derived from Root (not ctx — see Root's doc
// comment), and returns the session's initial (empty) snapshot immediately,
// without waiting for any results.
func (s *Searches) Start(ctx context.Context, query string) (core.SearchSession, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > searchQueryMaxLen {
		return core.SearchSession{}, ErrSearchQueryInvalid
	}
	if s.peers == nil {
		return core.SearchSession{}, ErrSearchUnavailable
	}

	s.mu.Lock()
	now := time.Now()
	s.evictLocked(now)
	if s.liveCountLocked() >= searchMaxSessions {
		s.mu.Unlock()
		return core.SearchSession{}, ErrSearchBusy
	}
	id, err := newSearchSessionID()
	if err != nil {
		s.mu.Unlock()
		return core.SearchSession{}, err
	}
	sessCtx, cancel := context.WithCancel(s.root)
	sess := &searchSession{
		id:        id,
		query:     query,
		startedAt: now,
		streaming: s.peers.SearchStreaming(),
		cancel:    cancel,
		raw:       make(map[string]*searchGroupAccum),
		groups:    make(map[string]core.SearchGroup),
	}
	s.sessions[id] = sess
	s.mu.Unlock()

	go s.run(sessCtx, sess)

	return sess.snapshot(), nil
}

// run drives one session's backend search to completion on its own
// goroutine, feeding every emitted batch into the session and recording the
// terminal outcome. forceCtx bounds the whole run independently of whatever
// timeout behavior the backend itself honors (searchForceCancelGrace).
func (s *Searches) run(ctx context.Context, sess *searchSession) {
	forceCtx, forceCancel := context.WithTimeout(ctx, s.timeout+searchForceCancelGrace)
	defer forceCancel()

	err := s.peers.SearchStream(forceCtx, sess.query, s.timeout, sess.accept)
	sess.finish(err)
	if err != nil {
		s.logger.Debug("manual search ended with error", "id", sess.id, "query", sess.query, "err", err)
	}
}

// Snapshot returns the whole current state of a session — the truth source
// GET /api/search/{id} serves. false if the session doesn't exist or has
// been evicted.
func (s *Searches) Snapshot(id string) (core.SearchSession, bool) {
	s.mu.Lock()
	s.evictLocked(time.Now())
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return core.SearchSession{}, false
	}
	return sess.snapshot(), true
}

// Delta returns every group that changed since since (a Version cursor
// previously returned as Seq), for the SSE hub's per-subscriber poll. false
// if the session doesn't exist or has expired past searchSessionTTL — the
// hub then sends one `expired: true` frame and falls silent (see
// internal/observ/stream.go). Delta does not itself remove an expired
// session from the registry — see evictLocked — so this check is
// independent, cheap, and never mutates s.sessions.
func (s *Searches) Delta(id string, since int) (core.SearchDelta, bool) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return core.SearchDelta{}, false
	}
	if sess.expired(time.Now(), s.timeout) {
		return core.SearchDelta{}, false
	}
	return sess.delta(since), true
}

// Stop cancels a session's context (releasing a reserved Soulseek token and
// freeing a slot from the live-session cap immediately, rather than at
// timeout) and reports whether it existed.
func (s *Searches) Stop(id string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	sess.cancel()
	return true
}

// liveCountLocked counts sessions that have not yet finished. Must be called
// with s.mu held.
func (s *Searches) liveCountLocked() int {
	n := 0
	for _, sess := range s.sessions {
		if !sess.isDone() {
			n++
		}
	}
	return n
}

// evictLocked removes every FINISHED session whose FinishedAt is older than
// searchSessionTTL. A still-running session is never evicted here — it is
// bounded instead by run's forceCtx, which will finish it (with an error)
// on its own. Must be called with s.mu held.
func (s *Searches) evictLocked(now time.Time) {
	for id, sess := range s.sessions {
		if sess.expired(now, s.timeout) {
			delete(s.sessions, id)
		}
	}
}

// newSearchSessionID returns a 128-bit crypto/rand hex id — not a counter:
// the id travels in a URL query param, and a guessable id would let one
// browser tab read another's search results.
func newSearchSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate search session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// searchGroupAccum accumulates the raw core.SearchResult files offered for
// one (peer, release directory) group, so its display core.SearchGroup can
// be rebuilt fresh (buildSearchGroup) whenever a new file arrives.
type searchGroupAccum struct {
	peer  string
	dir   string
	files []core.SearchResult
}

// searchSession is one manual search's mutable state, guarded by its own
// mutex so the session goroutine (accept) and any number of concurrent
// readers (snapshot/delta, called from HTTP handlers and the SSE hub) never
// race.
type searchSession struct {
	mu         sync.Mutex
	id         string
	query      string
	startedAt  time.Time
	finishedAt time.Time
	done       bool
	streaming  bool
	truncated  bool
	err        string
	total      int
	seq        int
	raw        map[string]*searchGroupAccum
	groups     map[string]core.SearchGroup
	cancel     context.CancelFunc
}

// accept is the sink SearchStream/Search calls with every newly emitted
// batch of results. Results beyond searchMaxResults are dropped and set
// Truncated rather than growing the session unboundedly.
func (sess *searchSession) accept(results []core.SearchResult) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	changed := make(map[string]struct{})
	for _, r := range results {
		if sess.total >= searchMaxResults {
			sess.truncated = true
			continue
		}
		dir := matcher.ReleaseDir(r.Filename)
		id := searchGroupID(r.Username, dir)
		accum := sess.raw[id]
		if accum == nil {
			accum = &searchGroupAccum{peer: r.Username, dir: dir}
			sess.raw[id] = accum
		}
		accum.files = append(accum.files, r)
		sess.total++
		changed[id] = struct{}{}
	}
	if len(changed) == 0 {
		return
	}
	sess.seq++
	for id := range changed {
		sess.groups[id] = buildSearchGroup(id, sess.raw[id], sess.seq)
	}
}

// finish records the session's terminal outcome. Partial groups accumulated
// via accept are kept — a backend failure degrades the session to "done,
// with an error, showing whatever it found," never a blank slate.
func (sess *searchSession) finish(err error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.done = true
	sess.finishedAt = time.Now()
	if err != nil {
		sess.err = err.Error()
	}
}

func (sess *searchSession) isDone() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.done
}

// expired reports whether this session should no longer be served: a
// finished session past searchSessionTTL since FinishedAt, or a still-running
// session past its own force-cancel deadline (a defensive fallback — run's
// forceCtx should already have finished it by then, so this branch is not
// expected to fire in practice).
func (sess *searchSession) expired(now time.Time, timeout time.Duration) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.done {
		return now.Sub(sess.finishedAt) > searchSessionTTL
	}
	return now.Sub(sess.startedAt) > timeout+searchForceCancelGrace+searchSessionTTL
}

// snapshot returns the whole current session state, groups sorted by id for
// deterministic output.
func (sess *searchSession) snapshot() core.SearchSession {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	groups := make([]core.SearchGroup, 0, len(sess.groups))
	for _, g := range sess.groups {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return core.SearchSession{
		ID:         sess.id,
		Query:      sess.query,
		StartedAt:  sess.startedAt,
		FinishedAt: sess.finishedAt,
		Done:       sess.done,
		Streaming:  sess.streaming,
		Truncated:  sess.truncated,
		Err:        sess.err,
		Total:      sess.total,
		Groups:     groups,
	}
}

// delta returns every group with Version > since, groups sorted by id for
// deterministic output.
func (sess *searchSession) delta(since int) core.SearchDelta {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var changed []core.SearchGroup
	for _, g := range sess.groups {
		if g.Version > since {
			changed = append(changed, g)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].ID < changed[j].ID })
	return core.SearchDelta{
		ID:        sess.id,
		Seq:       sess.seq,
		Groups:    changed,
		Total:     sess.total,
		Done:      sess.done,
		Streaming: sess.streaming,
		Truncated: sess.truncated,
		Err:       sess.err,
	}
}

// searchGroupID returns the stable, opaque group identifier for (username,
// releaseDir) — safe as a JSON value and a React list key.
func searchGroupID(username, releaseDir string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + releaseDir))
	return hex.EncodeToString(sum[:])[:16]
}

// searchAudioExtensions bounds which files count toward a group's TrackCount
// and its modal Format — a release directory can legitimately contain
// non-audio files (covers, .m3u, .nfo) that must not be counted as tracks.
var searchAudioExtensions = map[string]struct{}{
	".flac": {}, ".mp3": {}, ".m4a": {}, ".aac": {}, ".ogg": {},
	".opus": {}, ".wav": {}, ".wma": {}, ".ape": {}, ".wv": {}, ".alac": {},
}

// buildSearchGroup rebuilds one group's display shape from its accumulated
// raw files (issue #58). Deliberately does NOT call matcher.Rank: Rank
// applies passesFloor (a bitrate floor) and dedupeTracks (collapses a release
// to a single format), which are automation policy that would hide results
// the user explicitly searched for — a manual search shows what the network
// offers, not what the matcher would have picked. Score reuses only Rank's
// underlying primitives (matcher.FormatScore, matcher.ReliabilityScore), with
// no reliability-history term, since a fresh manual search has no rel map to
// consult.
func buildSearchGroup(id string, accum *searchGroupAccum, version int) core.SearchGroup {
	g := core.SearchGroup{
		ID:      id,
		Peer:    accum.peer,
		Folder:  accum.dir,
		Title:   path.Base(accum.dir),
		Parent:  path.Base(path.Dir(accum.dir)),
		Version: version,
	}

	formatCounts := make(map[string]int)
	var bitrates, sampleRates, bitDepths []int
	var totalDuration int
	haveAllDurations := len(accum.files) > 0
	var formatScoreSum float64

	for _, f := range accum.files {
		ext := matcher.ExtOf(f.Filename)
		if _, audio := searchAudioExtensions[ext]; audio {
			g.TrackCount++
			formatCounts[ext]++
		}
		g.SizeBytes += f.Size
		if f.Duration > 0 {
			totalDuration += f.Duration
		} else {
			haveAllDurations = false
		}
		bitrates = append(bitrates, f.BitRate)
		sampleRates = append(sampleRates, f.SampleRate)
		bitDepths = append(bitDepths, f.BitDepth)
		if f.VariableBitRate {
			g.VariableBitRate = true
		}
		formatScoreSum += matcher.FormatScore(f.Filename)
		// The peer's upload-availability signals are per-peer, not per-file,
		// so every file in a group shares the same one; the last file's
		// values are as good as any.
		g.FreeUploadSlot = f.HasFreeUploadSlot
		g.QueueLength = f.QueueLength
		g.UploadSpeed = f.UploadSpeed

		g.Files = append(g.Files, core.SearchFile{
			Filename:        f.Filename,
			Name:            path.Base(strings.ReplaceAll(f.Filename, `\`, "/")),
			Size:            f.Size,
			BitRate:         f.BitRate,
			Duration:        f.Duration,
			SampleRate:      f.SampleRate,
			BitDepth:        f.BitDepth,
			VariableBitRate: f.VariableBitRate,
		})
	}
	sort.Slice(g.Files, func(i, j int) bool { return g.Files[i].Filename < g.Files[j].Filename })

	if haveAllDurations {
		g.DurationSeconds = totalDuration
	}
	g.Format = modalSearchString(formatCounts)
	g.BitRate = modalSearchInt(bitrates)
	g.SampleRate = modalSearchInt(sampleRates)
	g.BitDepth = modalSearchInt(bitDepths)

	var avgFormatScore float64
	if n := len(accum.files); n > 0 {
		avgFormatScore = formatScoreSum / float64(n)
	}
	g.Score = avgFormatScore +
		matcher.ReliabilityScore(core.SearchResult{HasFreeUploadSlot: g.FreeUploadSlot, QueueLength: g.QueueLength}) +
		float64(g.BitRate)/1000.0

	return g
}

// modalSearchString returns the extension with the highest count, ties
// broken lexicographically for determinism (map iteration order is not),
// with the leading "." stripped (buildSearchGroup's Format is bare, e.g.
// "flac").
func modalSearchString(counts map[string]int) string {
	var best string
	for ext, n := range counts {
		if best == "" || n > counts[best] || (n == counts[best] && ext < best) {
			best = ext
		}
	}
	return strings.TrimPrefix(best, ".")
}

// modalSearchInt returns the most common NONZERO value, ties broken by the
// smaller value for determinism. Zero values (meaning "the peer sent no such
// attribute" — see core.SearchResult's doc comment) are excluded so a group
// with some attributed and some unattributed files reports the attribute
// value that was actually seen, not falsely favors "unknown".
func modalSearchInt(values []int) int {
	counts := make(map[int]int)
	for _, v := range values {
		if v != 0 {
			counts[v]++
		}
	}
	var best, bestCount int
	first := true
	for v, n := range counts {
		if first || n > bestCount || (n == bestCount && v < best) {
			best, bestCount, first = v, n, false
		}
	}
	return best
}
