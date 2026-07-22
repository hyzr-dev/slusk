// Package observ: jobdetail.go serves the per-job detail panel: a job's full
// candidate attempt history plus each attempt's per-file transfers. Fetched
// lazily by the dashboard (GET /api/jobs/{id}/detail) so the main /api/jobs
// payload stays small.
package observ

import (
	"context"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// transferDetailDTO is one file transfer within an attempt, as shown in the
// job detail panel.
type transferDetailDTO struct {
	Filename       string `json:"filename"`
	State          string `json:"state"`
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

// jobDetailDTO is the JSON shape served at /api/jobs/{id}/detail.
type jobDetailDTO struct {
	ID       int64              `json:"id"`
	Title    string             `json:"title"`
	Artist   string             `json:"artist"`
	State    string             `json:"state"`
	Attempts []attemptDetailDTO `json:"attempts"`
}

// toJobDetailDTO flattens a core.JobDetail into the detail panel's
// display-ready shape, enriching each transfer with live queue-position/speed
// from the peer backend where a match exists (see liveTransferIndex).
func toJobDetailDTO(d core.JobDetail, live liveTransferIndex) jobDetailDTO {
	out := jobDetailDTO{
		ID:       d.Job.ID,
		Title:    d.Job.Title,
		Artist:   d.Job.ArtistName,
		State:    string(d.Job.State),
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
				t.QueuePosition = lt.QueuePosition
				t.Speed = lt.Speed
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

// LiveTransfersFunc returns the peer backend's current in-flight transfers
// (ListDownloads). The job detail endpoint calls it best-effort to enrich the
// persisted transfer rows with live queue position and speed; those two values
// live only in memory on the native backend and are never persisted. A nil
// func, or one wired to the slskd backend (which leaves the fields zero), just
// yields no enrichment.
type LiveTransfersFunc func(ctx context.Context) ([]core.RemoteTransfer, error)

// liveTransferIndex correlates persisted store transfers to their live
// ListDownloads counterpart the same way the reconcile loop does
// (internal/pipeline/downloading.go): by remote id first, then by
// username+filename. The zero value matches nothing, so callers with no live
// data can pass it directly.
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
// remote id and falling back to username+filename.
func (idx liveTransferIndex) match(tr core.Transfer) (core.RemoteTransfer, bool) {
	if tr.SlskdID != "" {
		if lt, ok := idx.byID[tr.SlskdID]; ok {
			return lt, true
		}
	}
	lt, ok := idx.byFallback[tr.Username+"\x00"+tr.Filename]
	return lt, ok
}
