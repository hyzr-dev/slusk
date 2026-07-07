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
// display-ready shape.
func toJobDetailDTO(d core.JobDetail) jobDetailDTO {
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
			a.Transfers[j] = t
		}
		out.Attempts[i] = a
	}
	return out
}

// JobDetailFunc produces a job's full detail view (typically backed by the
// store's JobDetail). found is false if no job has that id.
type JobDetailFunc func(ctx context.Context, jobID int64) (core.JobDetail, bool, error)
