// Package observ: uploads.go serves the native Soulseek upload manager's
// live activity (issue #179): GET /api/uploads reports slot usage and the
// current active/queued upload entries. observ deliberately does not import
// internal/soulseek - UploadsFunc declares its own transport-level types,
// and cmd/slskdarr/main.go adapts between the two (see
// soulseek.Client.UploadReport).
package observ

import (
	"encoding/json"
	"net/http"
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

// registerUploads wires GET /api/uploads onto mux.
func registerUploads(mux *http.ServeMux, uploads UploadsFunc) {
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
}
