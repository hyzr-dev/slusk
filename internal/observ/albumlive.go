// Package observ: albumlive.go aggregates live (in-memory, non-persisted)
// transfer data from the peer backend's ListDownloads up to album level for
// the jobs list (GET /api/jobs, issue #157): per-file speed/queue-position is
// summed/mined across every live transfer belonging to a job's current
// candidate, so the list shows one album-level speed and ETA rather than
// requiring the frontend to open the per-job detail panel to see progress.
package observ

import (
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// maxETASeconds clamps etaSeconds so a momentary 1 B/s blip cannot render a
// multi-year ETA; 30 days is generous for a stalled-but-recovering transfer
// while still being an obviously-bogus number the frontend could choose to
// treat specially.
const maxETASeconds = 30 * 24 * 3600

// etaSeconds estimates remaining transfer time from remaining bytes and an
// average speed, or 0 (meaning: omit the field, see jobDTO.ETASeconds) when
// either input makes the estimate meaningless: avgSpeed <= 0 (no live
// throughput to divide by) or remaining <= 0 (nothing left, or unknown).
func etaSeconds(remaining, avgSpeed int64) int64 {
	if avgSpeed <= 0 || remaining <= 0 {
		return 0
	}
	eta := remaining / avgSpeed
	if eta > maxETASeconds {
		return maxETASeconds
	}
	return eta
}

// aggregateLiveAlbum sums the live transfers belonging to candidate's own
// files — matched exactly on (username, filename ∈ candidate.Files), via
// idx.matchFile (see liveTransferIndex in jobdetail.go), never on username
// alone, since a peer serving files for two different albums at once would
// otherwise have both counted on both album rows. Only transfers in a state
// that can still make progress (core.TransferQueued, core.TransferInProgress)
// are counted: a terminal-but-not-yet-reconciled transfer (errored,
// cancelled, completed) lingers in ListDownloads until the pipeline's next
// reconcile pass, and its speed would misrepresent current throughput.
// Returns the summed instantaneous speed and EWMA-smoothed average speed
// (for ETA), the minimum queue position among matched transfers that report
// one (the album's download effectively starts once its first file starts),
// and matched: whether at least one file matched a non-terminal live
// transfer at all, independent of whether that transfer happened to report
// zero speed. The SSE stream (buildStreamJobs, stream.go) uses matched to
// decide whether this job belongs in the live set at all; toJobDTO
// (observ.go) ignores it, since REST always reports every job regardless of
// live data. candidate == nil (a job with no candidate yet) yields all
// zeros / false.
//
// BytesDone is deliberately NOT computed here — see jobBytesDone's doc
// comment for why it needs a state-agnostic match (a just-completed transfer
// still carries the most accurate byte count available) instead of this
// non-terminal-only filter, and per-file persisted bytes this function has
// no access to. Remaining bytes are not computed here either: callers get
// them from core.JobView.AlbumBytesRemaining (store-computed — see
// internal/store/dashboard.go's jobViewSelect and the field comment on
// AlbumBytesRemaining).
func aggregateLiveAlbum(candidate *core.Candidate, idx liveTransferIndex) (speed, speedAvg int64, queuePosition uint32, hasQueuePosition bool, matched bool) {
	if candidate == nil {
		return 0, 0, 0, false, false
	}
	for _, f := range candidate.Files {
		lt, ok := idx.matchFile(candidate.Username, f.Filename)
		if !ok {
			continue
		}
		if lt.State != core.TransferQueued && lt.State != core.TransferInProgress {
			continue
		}
		matched = true
		speed += lt.Speed
		speedAvg += lt.SpeedAverage
		if lt.QueuePosition > 0 && (!hasQueuePosition || lt.QueuePosition < queuePosition) {
			queuePosition = lt.QueuePosition
			hasQueuePosition = true
		}
	}
	return speed, speedAvg, queuePosition, hasQueuePosition, matched
}

// anyLiveMatch reports whether at least one of files has a live counterpart
// at all, in ANY state — unlike aggregateLiveAlbum's matched this is NOT
// filtered to non-terminal transfers, since a lingering just-completed
// transfer (see jobBytesDone) is exactly the case that must not be missed.
// It is the trigger for fetching a candidate's persisted per-file bytes (see
// Store.TransferBytesByCandidate) instead of trusting the store's own
// album-wide AlbumBytesDone unmodified.
func anyLiveMatch(username string, files []core.CandidateFile, idx liveTransferIndex) bool {
	for _, f := range files {
		if _, ok := idx.matchFile(username, f.Filename); ok {
			return true
		}
	}
	return false
}

// sumFileBytesDone computes an album's true bytes-done as a direct per-file
// sum: a file with a live match (any state) contributes its live BytesDone;
// every other file contributes its persisted bytes from persistedByFilename
// (see Store.TransferBytesByCandidate). This replaces the old
// subtract-and-clamp overlay (issue #161 review), which computed
// persistedDone - persistedNonTerminalDone + liveDone and pinned the result
// to persistedDone via max() whenever the two terms' file sets didn't match
// exactly — which they routinely didn't: the moment a file completes, it
// drops out of the live non-terminal set before the next reconcile removes
// its contribution from persistedNonTerminalDone, collapsing the overlay term
// and freezing (or briefly regressing) the displayed total. See
// jobBytesDone's doc comment for why matching on ANY state, not just
// non-terminal, is what fixes that.
func sumFileBytesDone(username string, files []core.CandidateFile, idx liveTransferIndex, persistedByFilename map[string]int64) int64 {
	var done int64
	for _, f := range files {
		if lt, ok := idx.matchFile(username, f.Filename); ok {
			done += lt.BytesDone
			continue
		}
		done += persistedByFilename[f.Filename]
	}
	return done
}

// jobBytesDone computes one job's album-level BytesDone the same way for
// REST (toJobDTO, observ.go) and the SSE stream (buildStreamJobs, stream.go),
// so the two transports can never disagree about a given job's BytesDone
// (issue #161).
//
// The key property that makes matching on ANY live state (not just
// non-terminal) safe is continuity: internal/pipeline/downloading.go's
// reconcile only ever purges a transfer from the live backend (removeFromSlskd)
// in the same synchronous pass, immediately AFTER persisting its terminal
// state to Postgres (see reconcile's UpdateTransferProgress-then-removeFromSlskd
// sequence around the TransferCompleted/Cancelled/Errored branch, and Tick's
// reconcile-then-resolve-then-topUp ordering, which never interleaves a
// re-enqueue before that pair completes). So a terminal-but-unreconciled live
// entry always carries the most accurate byte count available — strictly
// more accurate than the not-yet-refreshed persisted row — and it is never
// possible to observe a live entry reporting FEWER bytes than what's already
// persisted for that same file, which is why no monotone/max() guard is
// needed here (contrast the old overlayBytesDone, which needed one because
// its non-terminal-only filter could make the live term collapse to less
// than what was already counted in persistedDone).
//
// candidateID/persisted are used to fetch this candidate's exact per-file
// persisted bytes (see Store.TransferBytesByCandidate) ONLY when at least one
// file has a live match (anyLiveMatch); a job with no live match at all, or a
// persisted result missing this candidate's id entirely (nil dep, failed
// query, or simply never fetched — e.g. the SSE hub's correlation refresh
// hasn't caught up with a brand new live match yet), falls back to
// albumBytesDone (the store's own AlbumBytesDone) unmodified — degrade, never
// guess.
func jobBytesDone(username string, files []core.CandidateFile, candidateID, albumBytesDone int64, idx liveTransferIndex, persisted map[int64]map[string]int64) int64 {
	if !anyLiveMatch(username, files, idx) {
		return albumBytesDone
	}
	byFilename, ok := persisted[candidateID]
	if !ok {
		return albumBytesDone
	}
	return sumFileBytesDone(username, files, idx, byFilename)
}

// clampBytesDone bounds a per-file bytes-done value to at most bytesTotal, so
// a peer momentarily reporting more bytes than the file's own known size
// can't push the per-file progress bar past 100% (transferDetailDTO.BytesDone,
// jobdetail.go). bytesTotal == 0 means "unknown", not "empty" (a candidate
// file's size can be legitimately unreported), so it disables the clamp
// rather than forcing done to 0.
func clampBytesDone(done, bytesTotal int64) int64 {
	if bytesTotal > 0 && done > bytesTotal {
		return bytesTotal
	}
	return done
}
