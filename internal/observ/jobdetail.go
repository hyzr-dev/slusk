// Package observ: jobdetail.go serves the per-job detail panel: a job's full
// candidate attempt history plus each attempt's per-file transfers. Fetched
// lazily by the dashboard (GET /api/jobs/{id}/detail) so the main /api/jobs
// payload stays small.
package observ

import (
	"context"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// transferDetailDTO is one file transfer within an attempt, as shown in the
// job detail panel.
type transferDetailDTO struct {
	Filename string `json:"filename"`
	// State is chosen by toJobDetailDTO's monotone terminal rule whenever a
	// live match exists (issue #258): if EITHER side reports a terminal
	// state the transfer is terminal, preferring the persisted state when
	// both sides are terminal (reconcile is the authority on which terminal
	// outcome it was); otherwise (neither side terminal) live supplies the
	// state. See toJobDetailDTO for why neither side alone can be sole
	// authority.
	State string `json:"state"`
	// BytesDone is overlaid with the live in-memory value whenever a live
	// match exists, regardless of that match's state (issue #161) — mirroring
	// jobDTO.BytesDone's album-level per-file overlay in observ.go (see
	// jobBytesDone's doc comment for why terminal states must be included: a
	// just-completed transfer still carries the most accurate byte count
	// available, more accurate than the persisted row until the next
	// Downloading reconcile writes it). It is clamped to BytesTotal
	// (clampBytesDone) so a peer momentarily reporting more bytes than the
	// file's own known size can't push the file past 100%.
	// BytesTotal is never overlaid — see jobDTO.BytesTotal's comment; the same
	// rationale applies per-file.
	BytesDone      int64  `json:"bytesDone"`
	BytesTotal     int64  `json:"bytesTotal"`
	Retries        int    `json:"retries"`
	LastProgressAt string `json:"lastProgressAt"`
	// QueuePosition (place in the peer's upload queue) and Speed (bytes/sec) are
	// live, non-persisted values joined in from the peer backend's ListDownloads
	// at request time. omitempty is deliberate: a terminal transfer has no live
	// entry (both zero), a queued one reports a queue position but no speed, and
	// an actively-downloading one reports speed but queue position 0 — so each
	// field is simply absent rather than reported as a misleading zero, and the
	// frontend hides absent fields instead of showing "0 B/s".
	QueuePosition uint32 `json:"queuePosition,omitempty"`
	Speed         int64  `json:"speed,omitempty"`
}

// attemptDetailDTO is one candidate attempt with its per-file transfers.
type attemptDetailDTO struct {
	ID         int64               `json:"id"`
	Username   string              `json:"username"`
	FileCount  int                 `json:"fileCount"`
	State      string              `json:"state"`
	FailReason string              `json:"failReason"`
	CreatedAt  string              `json:"createdAt"`
	UpdatedAt  string              `json:"updatedAt"`
	Transfers  []transferDetailDTO `json:"transfers"`
}

// jobDetailDTO is the JSON shape served at /api/jobs/{id}/detail:
// { "job": { ...whole jobDTO... }, "attempts": [...] }. Job nests the whole
// jobDTO as a NAMED field (issue #268) — built by the same toJobDTO GET
// /api/jobs and the stream's job-list frames use — rather than a
// hand-picked subset of its fields. JobDetail.tsx used to read effectively
// all of jobDTO's fields (title, artist, status, state, queuePosition,
// source, peer, bytesDone, bytesTotal, notBefore, nextAttemptAt,
// maxCandidates, candidatesTried, retries, failReason) off a SEPARATE
// GET /api/jobs/all response, purely to find its one row; duplicating that
// subset here as named fields would only drift the moment either shape
// changed. Nesting (not embedding) is deliberate: an embedded jobDTO would
// marshal flattened, which would carry id/title/artist twice once the whole
// object is present — two copies of one field is exactly the defect class
// this whole line of work exists to remove, and the frontend's type
// (`{ job: Job; attempts: AttemptDetail[] }`) expects the nested shape.
// Nesting the SAME finished object still makes the two transports
// structurally unable to disagree, and it is what lets a ?job=<id> stream
// subscriber's header go live for free — the stream already sends this
// whole body per frame.
type jobDetailDTO struct {
	Job      jobDTO             `json:"job"`
	Attempts []attemptDetailDTO `json:"attempts"`
}

// terminalTransferState reports whether a persisted transfer state is one the
// pipeline treats as final — the same three that gate reconcile's purge of a
// transfer from the live backend (internal/pipeline/downloading.go). STALLED is
// deliberately excluded: it is a durable retry intent the next pass acts on,
// not an end state, and such a transfer is still genuinely in flight.
func terminalTransferState(s core.TransferState) bool {
	return s == core.TransferCompleted || s == core.TransferErrored || s == core.TransferCancelled
}

// toJobDetailDTO flattens a core.JobDetail into the detail panel's
// display-ready shape, enriching each transfer with live queue-position/speed
// from the peer backend where a match exists (see liveTransferIndex).
// Per-transfer State is chosen by a monotone rule when a live match exists —
// see the inline comment at the match site for why neither the persisted
// nor the live state can be sole authority.
//
// view is a separately-fetched core.JobView for the same job id (issue
// #268): d (core.JobDetail) carries the attempt/transfer history but not
// the job-level fields toJobDTO needs (status, retries, candidatesTried,
// bytesDone, etc. — see jobDetailDTO's doc comment), so the header is built
// from view via the exact same toJobDTO the job list uses. Callers are
// responsible for fetching view and d for the SAME job id; this function
// does not itself verify d.Job.ID == view.Job.ID.
func toJobDetailDTO(view core.JobView, d core.JobDetail, live liveTransferIndex, persisted map[int64]map[string]int64, failedRetryAfter time.Duration, maxCandidates int, now time.Time) jobDetailDTO {
	out := jobDetailDTO{
		Job:      toJobDTO(view, failedRetryAfter, maxCandidates, live, persisted, now),
		Attempts: make([]attemptDetailDTO, len(d.Attempts)),
	}
	for i, ad := range d.Attempts {
		a := attemptDetailDTO{
			ID:         ad.Attempt.ID,
			Username:   ad.Attempt.Username,
			FileCount:  len(ad.Transfers),
			State:      string(ad.Attempt.State),
			FailReason: ad.Attempt.FailReason,
			CreatedAt:  ad.Attempt.CreatedAt.Format(timeFormat),
			UpdatedAt:  ad.Attempt.UpdatedAt.Format(timeFormat),
			Transfers:  make([]transferDetailDTO, len(ad.Transfers)),
		}
		for j, tr := range ad.Transfers {
			t := transferDetailDTO{
				Filename:   tr.Filename,
				State:      string(tr.State),
				BytesDone:  tr.BytesDone,
				BytesTotal: tr.BytesTotal,
				Retries:    tr.Retries,
			}
			if tr.LastProgressAt != nil {
				t.LastProgressAt = tr.LastProgressAt.Format(timeFormat)
			}
			if lt, ok := live.match(tr); ok {
				// Neither side alone can be sole authority for State (issue
				// #258): reconcile is best-effort about purging a transfer
				// from the live backend once it commits the terminal
				// persisted state (removeFromSlskd,
				// internal/pipeline/downloading.go swallows non-404 errors),
				// so a live entry can outlive its persisted terminal row and
				// keep reporting IN_PROGRESS at a stale nonzero speed (#259's
				// regression, restored below as
				// TestToJobDetailDTOPersistedTerminalWinsOverLingeringLiveEntry).
				// But the native client also sets a transfer's terminal state
				// in memory the instant it finishes
				// (internal/soulseek/downloads.go), while Postgres only
				// catches up on the next Downloading reconcile (default
				// 15s) — so a lingering PERSISTED non-terminal row can
				// equally be the stale one
				// (TestToJobDetailDTOTerminalLiveMatchDropsSpeedAndQueue).
				//
				// The monotone rule that covers both directions: a transfer
				// cannot become unfinished, so if EITHER side reports a
				// terminal state the transfer is terminal, and
				// Speed/QueuePosition (which describe work still in flight)
				// are dropped either way. When both sides are terminal but
				// disagree on WHICH terminal state, prefer the persisted one
				// — reconcile is the authority on the actual outcome, not
				// ListDownloads' last snapshot before the entry was purged.
				// Only when NEITHER side is terminal does live supply state,
				// speed and queue position (TestToJobDetailDTOStalledKeepsLiveSpeedAndQueue).
				liveTerminal := terminalTransferState(lt.State)
				persistedTerminal := terminalTransferState(tr.State)
				// Kept as three separate cases, even though the first two
				// bodies are both a one-line assignment, because each maps
				// 1:1 onto one of the three cases the doc comment above
				// names and tests individually — collapsing persistedTerminal
				// and liveTerminal into one arm would obscure which rule a
				// future reader is looking at.
				switch {
				case persistedTerminal:
					t.State = string(tr.State)
				case liveTerminal:
					t.State = string(lt.State)
				default:
					t.State = string(lt.State)
					t.QueuePosition = lt.QueuePosition
					t.Speed = lt.Speed
				}
				// A matched live transfer supplies bytes regardless of its
				// state — see transferDetailDTO.BytesDone's comment.
				t.BytesDone = clampBytesDone(lt.BytesDone, tr.BytesTotal)
			}
			a.Transfers[j] = t
		}
		out.Attempts[i] = a
	}
	return out
}

// JobDetailFunc produces a job's full detail view (typically backed by the
// store's JobDetail). found is false if no job has that id.
type JobDetailFunc func(ctx context.Context, jobID int64) (core.JobDetail, bool, error)

// JobViewFunc looks up a single job's live-computed core.JobView by id
// (typically backed by the store's JobWithTransfer) — the same shape the
// paged job list and stream job-list frames use, fetched via toJobDTO. found
// is false if no job has that id. Backs /api/jobs/{id}/detail's embedded
// jobDTO header (issue #268, see jobDetailDTO and toJobDetailDTO).
type JobViewFunc func(ctx context.Context, jobID int64) (core.JobView, bool, error)

// LiveTransfersFunc returns the peer backend's current in-flight transfers
// (ListDownloads). The job detail endpoint calls it best-effort to enrich the
// persisted transfer rows with live queue position and speed; those two values
// live only in memory on the native backend and are never persisted. A nil
// func, or one wired to the slskd backend (which leaves the fields zero), just
// yields no enrichment.
type LiveTransfersFunc func(ctx context.Context) ([]core.RemoteTransfer, error)

// TransferBytesFunc returns per-candidate, per-filename persisted bytes-done
// for exactly the given candidate ids (typically backed by
// Store.TransferBytesByCandidate). See ServerDeps.TransferBytes and
// jobBytesDone (albumlive.go, issue #161).
type TransferBytesFunc func(ctx context.Context, candidateIDs []int64) (map[int64]map[string]int64, error)

// liveTransferIndex correlates persisted store transfers to their live
// ListDownloads counterpart the same way the reconcile loop does
// (internal/pipeline/downloading.go): by remote id first, then by
// username+filename. The zero value matches nothing, so callers with no live
// data can pass it directly. Also reused by aggregateLiveAlbum
// (albumlive.go, issue #157) for the same username+filename match at album
// level — candidate files have no SlskdID, so only byFallback is used there.
type liveTransferIndex struct {
	byID       map[string]core.RemoteTransfer
	byFallback map[string]core.RemoteTransfer
}

// newLiveTransferIndex builds the id and username+filename lookup tables from a
// ListDownloads snapshot.
func newLiveTransferIndex(live []core.RemoteTransfer) liveTransferIndex {
	idx := liveTransferIndex{
		byID:       make(map[string]core.RemoteTransfer, len(live)),
		byFallback: make(map[string]core.RemoteTransfer, len(live)),
	}
	for _, t := range live {
		idx.byID[t.ID] = t
		idx.byFallback[t.Username+"\x00"+t.Filename] = t
	}
	return idx
}

// match resolves a store transfer to its live counterpart, preferring the
// remote id and falling back to username+filename (matchFile).
func (idx liveTransferIndex) match(tr core.Transfer) (core.RemoteTransfer, bool) {
	if tr.SlskdID != "" {
		if lt, ok := idx.byID[tr.SlskdID]; ok {
			return lt, true
		}
	}
	return idx.matchFile(tr.Username, tr.Filename)
}

// matchFile resolves a live transfer by (username, filename) alone — the
// fallback half of match, factored out for callers that only ever have a
// candidate file to match on (no persisted core.Transfer, hence no SlskdID
// to try first): album-level aggregation (aggregateLiveAlbum, albumlive.go)
// and the SSE stream's per-file view (buildStreamFiles, stream.go).
func (idx liveTransferIndex) matchFile(username, filename string) (core.RemoteTransfer, bool) {
	lt, ok := idx.byFallback[username+"\x00"+filename]
	return lt, ok
}
