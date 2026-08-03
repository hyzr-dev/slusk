// Package observ: shares.go serves the native Soulseek share index (issue
// #160): GET /api/shares reports aggregate + per-folder statistics, POST
// /api/shares/rescan triggers a background re-index. observ deliberately
// does not import internal/soulseek - SharesFunc/RescanSharesFunc declare
// their own transport-level types, and cmd/slusk/main.go adapts between
// the two (see soulseek.ShareReport / soulseek.TriggerRescanShares).
package observ

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ShareFolderStats is one configured share's contribution to the published
// index, served as one entry of ShareStatsReport.Folders. Its json tags are
// the wire shape of one entry of sharesDTO.Folders - unlike sharesDTO itself,
// this needs no reformatting (no time.Time, no duration), so it is served
// directly rather than through an intermediate DTO.
type ShareFolderStats struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Directories int    `json:"directories"`
	Files       int    `json:"files"`
	TotalBytes  uint64 `json:"totalBytes"`
}

// ShareStatsReport is the current native Soulseek share index: aggregate
// stats, per-folder breakdown, and whether a scan is running right now.
type ShareStatsReport struct {
	Directories  int
	Files        int
	TotalBytes   uint64
	IndexedAt    time.Time
	ScanDuration time.Duration
	Scanning     bool
	Folders      []ShareFolderStats
}

// SharesFunc reports the current share index. Nil when native Soulseek
// sharing is not enabled in this configuration - GET /api/shares then answers
// "enabled: false" instead of a misleading zero report.
type SharesFunc func() ShareStatsReport

// RescanSharesFunc starts a background re-index, returning as soon as the
// scan is claimed rather than waiting for it to finish. Returns
// ErrShareScanInProgress when a scan is already running. Nil when native
// Soulseek sharing is not enabled.
type RescanSharesFunc func() error

// ErrShareScanInProgress is returned by a RescanSharesFunc when a share scan
// is already running; POST /api/shares/rescan maps this to 409.
var ErrShareScanInProgress = errors.New("share scan already in progress")

// sharesDTO is the JSON shape served at GET /api/shares.
type sharesDTO struct {
	Enabled        bool               `json:"enabled"`
	Scanning       bool               `json:"scanning"`
	IndexedAt      string             `json:"indexedAt"`
	ScanDurationMs int64              `json:"scanDurationMs"`
	Directories    int                `json:"directories"`
	Files          int                `json:"files"`
	TotalBytes     uint64             `json:"totalBytes"`
	Folders        []ShareFolderStats `json:"folders"`
}

// disabledSharesDTO returns the shape served at GET /api/shares when native
// Soulseek sharing is not enabled (Shares is nil).
func disabledSharesDTO() sharesDTO {
	return sharesDTO{Folders: make([]ShareFolderStats, 0)}
}

func toSharesDTO(report ShareStatsReport) sharesDTO {
	folders := report.Folders
	if folders == nil {
		folders = make([]ShareFolderStats, 0)
	}
	dto := sharesDTO{
		Enabled:        true,
		Scanning:       report.Scanning,
		ScanDurationMs: report.ScanDuration.Milliseconds(),
		Directories:    report.Directories,
		Files:          report.Files,
		TotalBytes:     report.TotalBytes,
		Folders:        folders,
	}
	if !report.IndexedAt.IsZero() {
		dto.IndexedAt = report.IndexedAt.Format(timeFormat)
	}
	return dto
}

// registerShares wires GET /api/shares and POST /api/shares/rescan onto mux.
func registerShares(mux *http.ServeMux, shares SharesFunc, rescan RescanSharesFunc) {
	mux.HandleFunc("/api/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if shares == nil {
			_ = json.NewEncoder(w).Encode(disabledSharesDTO())
			return
		}
		_ = json.NewEncoder(w).Encode(toSharesDTO(shares()))
	})
	mux.HandleFunc("/api/shares/rescan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if rescan == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "soulseek sharing is not enabled in the configuration", nil)
			return
		}
		err := rescan()
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(struct {
				OK       bool `json:"ok"`
				Scanning bool `json:"scanning"`
			}{OK: true, Scanning: true})
		case errors.Is(err, ErrShareScanInProgress):
			writeConfigError(w, http.StatusConflict, "a share scan is already in progress", nil)
		default:
			// Do not echo err.Error(): it may carry local filesystem paths
			// (see serveConfigPost's identical reasoning). Also do not guess at
			// a cause: observ deliberately does not import internal/soulseek, so
			// it cannot know which errors exist on the other side of that
			// boundary beyond the ErrShareScanInProgress sentinel above.
			writeConfigError(w, http.StatusServiceUnavailable, "failed to start share rescan", nil)
		}
	})
}
