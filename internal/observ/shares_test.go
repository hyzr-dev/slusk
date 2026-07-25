package observ

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newSharesTestHandler(reg *prometheus.Registry, shares SharesFunc, rescan RescanSharesFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.Shares = shares
	deps.RescanShares = rescan
	return NewServer(deps)
}

// TestSharesEndpointServesReportShape asserts the GET /api/shares DTO shape,
// including indexedAt formatting, scanDurationMs as milliseconds (not a Go
// duration string), and a populated folders array.
func TestSharesEndpointServesReportShape(t *testing.T) {
	reg := prometheus.NewRegistry()
	indexedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	shares := func() ShareStatsReport {
		return ShareStatsReport{
			Directories:  412,
			Files:        8231,
			TotalBytes:   91234567890,
			IndexedAt:    indexedAt,
			ScanDuration: 1843 * time.Millisecond,
			Scanning:     false,
			Folders: []ShareFolderStats{
				{Name: "Music", Path: "/data/music", Directories: 400, Files: 8000, TotalBytes: 90000000000},
			},
		}
	}
	h := newSharesTestHandler(reg, shares, noopRescanShares)

	req := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got sharesDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want true")
	}
	if got.Scanning {
		t.Error("Scanning = true, want false")
	}
	if got.IndexedAt != indexedAt.Format(timeFormat) {
		t.Errorf("IndexedAt = %q, want %q", got.IndexedAt, indexedAt.Format(timeFormat))
	}
	if got.ScanDurationMs != 1843 {
		t.Errorf("ScanDurationMs = %d, want 1843", got.ScanDurationMs)
	}
	if got.Directories != 412 || got.Files != 8231 || got.TotalBytes != 91234567890 {
		t.Errorf("aggregate = %+v, want directories=412 files=8231 totalBytes=91234567890", got)
	}
	if len(got.Folders) != 1 {
		t.Fatalf("folders = %+v, want 1 entry", got.Folders)
	}
	folder := got.Folders[0]
	if folder.Name != "Music" || folder.Path != "/data/music" || folder.Directories != 400 || folder.Files != 8000 || folder.TotalBytes != 90000000000 {
		t.Errorf("folder = %+v, want the Music share's stats", folder)
	}
}

// TestSharesEndpointEmptyFoldersEmitsEmptyArrayNotNull asserts folders is
// always "[]" in JSON, never null, matching toChartsDTO's convention.
func TestSharesEndpointEmptyFoldersEmitsEmptyArrayNotNull(t *testing.T) {
	reg := prometheus.NewRegistry()
	shares := func() ShareStatsReport { return ShareStatsReport{} }
	h := newSharesTestHandler(reg, shares, noopRescanShares)

	req := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"folders":[]`) {
		t.Errorf(`expected "folders":[] in body, got %s`, body)
	}
	if !strings.Contains(body, `"indexedAt":""`) {
		t.Errorf(`expected zero-value indexedAt to serve "", got %s`, body)
	}
}

// TestSharesEndpointNilSharesReportsDisabled asserts GET /api/shares answers
// 200 with enabled:false rather than a zero-value report that would look
// like an empty-but-configured share index when native Soulseek sharing is
// not enabled in the config.
func TestSharesEndpointNilSharesReportsDisabled(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newSharesTestHandler(reg, nil, noopRescanShares)

	req := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got sharesDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false when Shares dep is nil")
	}
	if got.Scanning {
		t.Error("Scanning = true, want false when Shares dep is nil")
	}
	if got.Folders == nil || len(got.Folders) != 0 {
		t.Errorf("Folders = %+v, want empty non-nil slice", got.Folders)
	}
}

// TestSharesEndpointRejectsNonGET asserts GET-only, matching the GET-branch
// method check in newConfigHandler (config.go).
func TestSharesEndpointRejectsNonGET(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newSharesTestHandler(reg, func() ShareStatsReport { return ShareStatsReport{} }, noopRescanShares)

	req := httptest.NewRequest(http.MethodPost, "/api/shares", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow header = %q, want %q", allow, "GET")
	}
}

// TestSharesRescanEndpointAccepted asserts a successful trigger answers 202
// with {"ok":true,"scanning":true}.
func TestSharesRescanEndpointAccepted(t *testing.T) {
	reg := prometheus.NewRegistry()
	rescan := func() error { return nil }
	h := newSharesTestHandler(reg, func() ShareStatsReport { return ShareStatsReport{} }, rescan)

	req := httptest.NewRequest(http.MethodPost, "/api/shares/rescan", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK       bool `json:"ok"`
		Scanning bool `json:"scanning"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || !got.Scanning {
		t.Errorf("response = %+v, want ok=true scanning=true", got)
	}
}

// TestSharesRescanEndpointInProgressReturns409 asserts ErrShareScanInProgress
// maps to 409, matching the plan's concurrency contract.
func TestSharesRescanEndpointInProgressReturns409(t *testing.T) {
	reg := prometheus.NewRegistry()
	rescan := func() error { return ErrShareScanInProgress }
	h := newSharesTestHandler(reg, func() ShareStatsReport { return ShareStatsReport{} }, rescan)

	req := httptest.NewRequest(http.MethodPost, "/api/shares/rescan", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status code = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
}

// TestSharesRescanEndpointNilRescanReturns503 asserts POST
// /api/shares/rescan answers 503 (not enabled) when RescanShares is nil,
// mirroring GET's enabled:false rather than a misleading 500.
func TestSharesRescanEndpointNilRescanReturns503(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newSharesTestHandler(reg, func() ShareStatsReport { return ShareStatsReport{} }, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/shares/rescan", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
}

// TestSharesRescanEndpointArbitraryErrorReturns503WithoutRawMessage asserts
// an arbitrary rescan error maps to 503 and never echoes the raw error text
// (which could carry local filesystem paths), matching serveConfigPost's
// reasoning.
func TestSharesRescanEndpointArbitraryErrorReturns503WithoutRawMessage(t *testing.T) {
	reg := prometheus.NewRegistry()
	sensitive := "scan share \"Music\": stat /home/alice/private: permission denied"
	rescan := func() error { return errors.New(sensitive) }
	h := newSharesTestHandler(reg, func() ShareStatsReport { return ShareStatsReport{} }, rescan)

	req := httptest.NewRequest(http.MethodPost, "/api/shares/rescan", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), sensitive) {
		t.Errorf("response body echoed the raw error text: %s", rec.Body.String())
	}
}

// TestSharesRescanEndpointRejectsNonPOST asserts POST-only.
func TestSharesRescanEndpointRejectsNonPOST(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newSharesTestHandler(reg, func() ShareStatsReport { return ShareStatsReport{} }, noopRescanShares)

	req := httptest.NewRequest(http.MethodGet, "/api/shares/rescan", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Errorf("Allow header = %q, want %q", allow, "POST")
	}
}
