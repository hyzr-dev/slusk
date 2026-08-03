// Package observ: search.go serves manual Soulseek search (issue #58): POST
// /api/search starts a session, GET /api/search/{id} is its truth-source
// snapshot, DELETE /api/search/{id} cancels it. The incremental half lives in
// stream.go's `event: search` (?search=<id> scope), sharing this file's DTOs
// so the two transports can never disagree about a group's shape.
//
// observ deliberately does not import internal/store or internal/pipeline —
// only internal/app, for its sentinel errors, exactly like the rest of this
// package (see e.g. serveCreateJob).
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/samuelenocsson/slusk/internal/app"
	"github.com/samuelenocsson/slusk/internal/core"
)

// StartSearchFunc starts a new manual search session (typically backed by
// app.Searches.Start). Errors are mapped to a status code by POST
// /api/search: errors.Is(err, app.ErrSearchQueryInvalid) -> 422,
// errors.Is(err, app.ErrSearchBusy) -> 503 (with Retry-After: 30),
// errors.Is(err, app.ErrSearchUnavailable) -> 503, anything else -> 500.
type StartSearchFunc func(ctx context.Context, query string) (core.SearchSession, error)

// SearchSnapshotFunc looks up a session's whole current state (typically
// backed by app.Searches.Snapshot) for GET /api/search/{id} and the SSE
// stream's initial frame. false means the session does not exist — never
// existed, or has been evicted — mapped to 404 by GET /api/search/{id}.
type SearchSnapshotFunc func(id string) (core.SearchSession, bool)

// SearchDeltaFunc returns every group that changed since a cursor (typically
// backed by app.Searches.Delta). Used only by the SSE stream hub's ?search=
// scope (see stream.go) — registerSearch's own REST endpoints never call it.
// false means the session does not exist or has expired; the hub then sends
// one `expired: true` frame and falls silent.
type SearchDeltaFunc func(id string, since int) (core.SearchDelta, bool)

// StopSearchFunc cancels a session (typically backed by app.Searches.Stop)
// for DELETE /api/search/{id}. false means the session does not exist,
// mapped to 404.
type StopSearchFunc func(id string) bool

// searchFileDTO is the JSON shape of one core.SearchFile. Filename is the
// full peer-syntax path — exactly what POST /api/jobs requires to enqueue
// it — while Name is its display basename. Every optional attribute is
// omitempty: zero means the peer sent no such attribute (see
// core.SearchResult's doc comment), so the frontend's honest type is
// `number | undefined`, never a misleading zero.
type searchFileDTO struct {
	Filename        string `json:"filename"`
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	BitRate         int    `json:"bitrate,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	SampleRate      int    `json:"sampleRate,omitempty"`
	BitDepth        int    `json:"bitDepth,omitempty"`
	VariableBitRate bool   `json:"variableBitRate,omitempty"`
}

// searchGroupDTO is the JSON shape of one core.SearchGroup — one release
// offered by one peer. This shape is frozen as the contract the frontend
// (STAGE 2 of issue #58) is built against: both the 201 body of POST
// /api/search and the `event: search` SSE delta carry groups in exactly this
// shape, so the frontend needs exactly one normalizer for both transports.
type searchGroupDTO struct {
	ID              string          `json:"id"`
	Peer            string          `json:"peer"`
	Folder          string          `json:"folder"`
	Title           string          `json:"title"`
	Parent          string          `json:"parent"`
	TrackCount      int             `json:"trackCount"`
	SizeBytes       int64           `json:"sizeBytes"`
	DurationSeconds int             `json:"durationSeconds,omitempty"`
	Format          string          `json:"format,omitempty"`
	BitRate         int             `json:"bitrate,omitempty"`
	SampleRate      int             `json:"sampleRate,omitempty"`
	BitDepth        int             `json:"bitDepth,omitempty"`
	VariableBitRate bool            `json:"variableBitRate,omitempty"`
	FreeUploadSlot  bool            `json:"freeUploadSlot"`
	QueueLength     int             `json:"queueLength"`
	UploadSpeed     int             `json:"uploadSpeed"`
	Score           float64         `json:"score"`
	Files           []searchFileDTO `json:"files"`
}

// searchSessionDTO is the JSON shape served at POST /api/search (201) and
// GET /api/search/{id} (200) — byte-identical in shape between the two so
// the frontend has exactly one normalizer for both.
type searchSessionDTO struct {
	ID        string           `json:"id"`
	Query     string           `json:"query"`
	StartedAt string           `json:"startedAt"`
	Done      bool             `json:"done"`
	Streaming bool             `json:"streaming"`
	Truncated bool             `json:"truncated,omitempty"`
	Error     string           `json:"error,omitempty"`
	Total     int              `json:"total"`
	Groups    []searchGroupDTO `json:"groups"`
}

func toSearchFileDTO(f core.SearchFile) searchFileDTO {
	return searchFileDTO{
		Filename:        f.Filename,
		Name:            f.Name,
		Size:            f.Size,
		BitRate:         f.BitRate,
		DurationSeconds: f.Duration,
		SampleRate:      f.SampleRate,
		BitDepth:        f.BitDepth,
		VariableBitRate: f.VariableBitRate,
	}
}

func toSearchGroupDTO(g core.SearchGroup) searchGroupDTO {
	files := make([]searchFileDTO, len(g.Files))
	for i, f := range g.Files {
		files[i] = toSearchFileDTO(f)
	}
	return searchGroupDTO{
		ID:              g.ID,
		Peer:            g.Peer,
		Folder:          g.Folder,
		Title:           g.Title,
		Parent:          g.Parent,
		TrackCount:      g.TrackCount,
		SizeBytes:       g.SizeBytes,
		DurationSeconds: g.DurationSeconds,
		Format:          g.Format,
		BitRate:         g.BitRate,
		SampleRate:      g.SampleRate,
		BitDepth:        g.BitDepth,
		VariableBitRate: g.VariableBitRate,
		FreeUploadSlot:  g.FreeUploadSlot,
		QueueLength:     g.QueueLength,
		UploadSpeed:     g.UploadSpeed,
		Score:           g.Score,
		Files:           files,
	}
}

// toSearchGroupDTOs maps a slice of core.SearchGroup to their JSON shape,
// always returning a non-nil (possibly empty) slice so `groups` never
// serializes as JSON null.
func toSearchGroupDTOs(groups []core.SearchGroup) []searchGroupDTO {
	out := make([]searchGroupDTO, len(groups))
	for i, g := range groups {
		out[i] = toSearchGroupDTO(g)
	}
	return out
}

func toSearchSessionDTO(sess core.SearchSession) searchSessionDTO {
	return searchSessionDTO{
		ID:        sess.ID,
		Query:     sess.Query,
		StartedAt: sess.StartedAt.Format(timeFormat),
		Done:      sess.Done,
		Streaming: sess.Streaming,
		Truncated: sess.Truncated,
		Error:     sess.Err,
		Total:     sess.Total,
		Groups:    toSearchGroupDTOs(sess.Groups),
	}
}

// createSearchRequest is the POST /api/search request body.
type createSearchRequest struct {
	Query string `json:"query"`
}

// registerSearch wires POST /api/search, GET /api/search/{id}, and DELETE
// /api/search/{id} onto mux. Nil-safe by design, mirroring registerShares:
// when start is nil (no peer backend wired), every endpoint answers 503
// rather than panicking — search is then a capability absent from this
// build/config, not a transient failure.
func registerSearch(mux *http.ServeMux, start StartSearchFunc, snapshot SearchSnapshotFunc, stop StopSearchFunc) {
	mux.HandleFunc("POST /api/search", func(w http.ResponseWriter, r *http.Request) {
		if start == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "search is not enabled in this configuration", nil)
			return
		}
		var req createSearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeConfigError(w, http.StatusBadRequest, "invalid request body", nil)
			return
		}
		sess, err := start(r.Context(), req.Query)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(toSearchSessionDTO(sess))
		case errors.Is(err, app.ErrSearchQueryInvalid):
			writeConfigError(w, http.StatusUnprocessableEntity, "validation failed",
				map[string]string{"query": "is required and must be 256 characters or fewer"})
		case errors.Is(err, app.ErrSearchBusy):
			// Retry-After tells a well-behaved client to back off rather than
			// hammer an endpoint that broadcasts to the whole Soulseek network
			// on every accepted request.
			w.Header().Set("Retry-After", "30")
			writeConfigError(w, http.StatusServiceUnavailable, err.Error(), nil)
		case errors.Is(err, app.ErrSearchUnavailable):
			writeConfigError(w, http.StatusServiceUnavailable, err.Error(), nil)
		default:
			writeConfigError(w, http.StatusInternalServerError, "failed to start search", nil)
		}
	})
	mux.HandleFunc("GET /api/search/{id}", func(w http.ResponseWriter, r *http.Request) {
		if snapshot == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "search is not enabled in this configuration", nil)
			return
		}
		sess, ok := snapshot(r.PathValue("id"))
		if !ok {
			writeConfigError(w, http.StatusNotFound, "search session not found", nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toSearchSessionDTO(sess))
	})
	mux.HandleFunc("DELETE /api/search/{id}", func(w http.ResponseWriter, r *http.Request) {
		if stop == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "search is not enabled in this configuration", nil)
			return
		}
		if !stop(r.PathValue("id")) {
			writeConfigError(w, http.StatusNotFound, "search session not found", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
