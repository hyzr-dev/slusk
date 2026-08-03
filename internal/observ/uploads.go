// Package observ: uploads.go serves the native Soulseek upload manager's
// live activity (issue #179): GET /api/uploads reports slot usage and the
// current active/queued upload entries, and GET /api/uploads/history pages the
// persisted record of finished uploads (issue #325). observ deliberately does
// not import internal/soulseek - UploadsFunc declares its own transport-level
// types, and cmd/slusk/main.go adapts between the two (see
// soulseek.Client.UploadReport).
package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/hyzr-dev/slusk/internal/core"
)

// UploadEntry is one upload in an UploadReport: either currently streaming
// (Active true, Position 0) or waiting in the queue (Position is its
// 1-based place). Filename is the normalized virtual share path
// (backslash-separated, rooted at the configured share name) and Username
// is a Soulseek nick - both are already visible to any peer that browses
// us, unlike registerShares's rescan-error branch (which refuses to echo
// err.Error() because those can carry local filesystem paths). There is
// nothing to redact here; do not "fix" this by adding redaction, and do not
// "fix" it the other way by echoing raw errors elsewhere either.
type UploadEntry struct {
	Username     string `json:"username"`
	Filename     string `json:"filename"`
	Active       bool   `json:"active"`
	Position     uint32 `json:"position"`
	Size         uint64 `json:"size"`
	BytesWritten uint64 `json:"bytesWritten"`
}

// UploadReport is the current native Soulseek upload manager's state,
// served at GET /api/uploads.
type UploadReport struct {
	Slots     int
	Active    int
	Queued    int
	Truncated int
	Uploads   []UploadEntry
}

// UploadsFunc reports the current upload activity. Nil when native
// Soulseek uploads are not enabled in this configuration - GET /api/uploads
// then answers "enabled: false" instead of a misleading empty report.
type UploadsFunc func() UploadReport

// uploadsDTO is the JSON shape served at GET /api/uploads.
type uploadsDTO struct {
	Enabled   bool          `json:"enabled"`
	Slots     int           `json:"slots"`
	Active    int           `json:"active"`
	Queued    int           `json:"queued"`
	Truncated int           `json:"truncated"`
	Uploads   []UploadEntry `json:"uploads"`
}

// disabledUploadsDTO returns the shape served at GET /api/uploads when
// native Soulseek uploads are not enabled (Uploads is nil).
func disabledUploadsDTO() uploadsDTO {
	return uploadsDTO{Uploads: make([]UploadEntry, 0)}
}

func toUploadsDTO(report UploadReport) uploadsDTO {
	uploads := report.Uploads
	if uploads == nil {
		uploads = make([]UploadEntry, 0)
	}
	return uploadsDTO{
		Enabled:   true,
		Slots:     report.Slots,
		Active:    report.Active,
		Queued:    report.Queued,
		Truncated: report.Truncated,
		Uploads:   uploads,
	}
}

// maxUploadHistoryLimit caps the page size GET /api/uploads/history will
// honor, regardless of the requested ?limit=.
const maxUploadHistoryLimit = 200

// defaultUploadHistoryLimit is the page size used when ?limit= is absent or
// invalid.
const defaultUploadHistoryLimit = 50

// UploadHistoryFunc pages finished uploads newest-first for GET
// /api/uploads/history. beforeID > 0 pages backwards from that row's id; limit
// is already clamped by the caller.
//
// It takes core.UploadHistoryEntry rather than a transport type of its own for
// the same reason ThreadFunc takes core.PrivateMessage: the type comes from the
// store, not from internal/soulseek, so there is nothing here to keep out of
// observ (see the messages.go package comment).
//
// Unlike UploadsFunc this carries no "enabled" notion and is wired
// unconditionally: the history is a fact already in the database, not a live
// capability, so rows written while the native backend was on stay readable
// after it is switched off.
type UploadHistoryFunc func(ctx context.Context, limit int, beforeID int64) ([]core.UploadHistoryEntry, error)

// UploadHistoryMarkFunc reports a cheap monotonic marker of the
// upload_history table — its highest row id, or 0 when the table is empty.
// GET /api/stream's hub folds it into the same fingerprint that drives
// `event: invalidate` (issue #366, see internal/observ/stream.go's
// invalidationFingerprint), so a finished upload reaches an open Shares view
// without the client polling for it.
//
// Deliberately a pull, not a push from the upload sink: observ receives
// every other input the same way (see stream.go's package comment), and the
// one function this serves does not justify making it the first component
// that can be told about a change rather than discovering one on its own
// schedule. max(id) on a BIGINT identity primary key is an index scan (see
// internal/store's UploadHistoryMaxID), so polling it on the existing
// correlation tick is effectively free.
//
// Nil is a no-op — the marker contributes nothing to the fingerprint and a
// finished upload simply never triggers an invalidation, which is the
// pre-#366 behaviour.
type UploadHistoryMarkFunc func(ctx context.Context) (int64, error)

// uploadHistoryDTO is the JSON shape of one row in GET /api/uploads/history.
//
// Filename is the virtual share path and Detail a short fixed reason string,
// never a raw error — see the UploadEntry comment above for why neither needs
// redacting and why that must not be taken as licence to echo errors here.
//
// Status is "completed", "aborted" or "rejected". On a rejected row nothing was
// ever streamed, so bytesSent and avgBytesPerSecond are 0 as a true measurement
// rather than a missing one; a UI must render that as "—" and not as "0 B/s".
type uploadHistoryDTO struct {
	ID                int64  `json:"id"`
	Username          string `json:"username"`
	Filename          string `json:"filename"`
	Size              uint64 `json:"size"`
	BytesSent         uint64 `json:"bytesSent"`
	AvgBytesPerSecond uint64 `json:"avgBytesPerSecond"`
	Status            string `json:"status"`
	Detail            string `json:"detail"`
	StartedAt         string `json:"startedAt"`
	FinishedAt        string `json:"finishedAt"`
}

func toUploadHistoryDTOs(entries []core.UploadHistoryEntry) []uploadHistoryDTO {
	dtos := make([]uploadHistoryDTO, len(entries))
	for i, e := range entries {
		dtos[i] = uploadHistoryDTO{
			ID:                e.ID,
			Username:          e.Username,
			Filename:          e.Filename,
			Size:              e.Size,
			BytesSent:         e.BytesSent,
			AvgBytesPerSecond: e.AvgBytesPerSecond,
			Status:            string(e.Status),
			Detail:            e.Detail,
			StartedAt:         e.StartedAt.Format(timeFormat),
			FinishedAt:        e.FinishedAt.Format(timeFormat),
		}
	}
	return dtos
}

// uploadHistoryResponse is the JSON body of GET /api/uploads/history.
type uploadHistoryResponse struct {
	Uploads []uploadHistoryDTO `json:"uploads"`
	HasMore bool               `json:"hasMore"`
}

// registerUploads wires GET /api/uploads and GET /api/uploads/history onto mux.
func registerUploads(mux *http.ServeMux, uploads UploadsFunc, history UploadHistoryFunc) {
	mux.HandleFunc("/api/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if uploads == nil {
			_ = json.NewEncoder(w).Encode(disabledUploadsDTO())
			return
		}
		_ = json.NewEncoder(w).Encode(toUploadsDTO(uploads()))
	})

	mux.HandleFunc("/api/uploads/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if history == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "upload history is not available in this configuration", nil)
			return
		}
		limit := parseUploadHistoryLimit(r.URL.Query().Get("limit"))
		var beforeID int64
		if raw := r.URL.Query().Get("before"); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				beforeID = parsed
			}
		}

		// Fetch one extra row to detect whether more history remains, then
		// trim it back off before serving.
		entries, err := history(r.Context(), limit+1, beforeID)
		if err != nil {
			writeConfigError(w, http.StatusInternalServerError, "failed to load upload history", nil)
			return
		}
		hasMore := len(entries) > limit
		if hasMore {
			entries = entries[:limit]
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(uploadHistoryResponse{Uploads: toUploadHistoryDTOs(entries), HasMore: hasMore})
	})
}

func parseUploadHistoryLimit(raw string) int {
	if raw == "" {
		return defaultUploadHistoryLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultUploadHistoryLimit
	}
	if limit > maxUploadHistoryLimit {
		return maxUploadHistoryLimit
	}
	return limit
}
