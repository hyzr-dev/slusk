// Package observ: events.go serves the job audit trail: a job's own event
// history (GET /api/jobs/{id}/events) and the global event timeline (GET
// /api/events) backing the dashboard's Händelser tab.
package observ

import (
	"context"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// eventsLimitDefault and eventsLimitMax bound the "limit" query parameter on
// GET /api/events: unset defaults to eventsLimitDefault, and anything above
// eventsLimitMax is clamped down to it, so a careless client can't force an
// unbounded scan.
const (
	eventsLimitDefault = 100
	eventsLimitMax     = 500
)

// eventDTO is the JSON shape served in job/event lists.
type eventDTO struct {
	ID        int64  `json:"id"`
	JobID     int64  `json:"jobId"`
	Event     string `json:"event"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"createdAt"`
}

func toEventDTO(e core.JobEvent) eventDTO {
	return eventDTO{
		ID:        e.ID,
		JobID:     e.AlbumJobID,
		Event:     string(e.Event),
		Detail:    e.Detail,
		CreatedAt: e.CreatedAt.Format(timeFormat),
	}
}

func toEventDTOs(events []core.JobEvent) []eventDTO {
	out := make([]eventDTO, len(events))
	for i, e := range events {
		out[i] = toEventDTO(e)
	}
	return out
}

// JobEventsFunc produces one job's audit trail, newest first (typically
// backed by the store's JobEvents).
type JobEventsFunc func(ctx context.Context, jobID int64) ([]core.JobEvent, error)

// RecentEventsFunc produces the most recent events across every job, newest
// first, capped at limit (typically backed by the store's RecentEvents).
type RecentEventsFunc func(ctx context.Context, limit int) ([]core.JobEvent, error)
