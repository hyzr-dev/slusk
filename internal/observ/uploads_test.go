package observ

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func newUploadsTestHandler(reg *prometheus.Registry, uploads UploadsFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.Uploads = uploads
	return NewServer(deps)
}

// TestUploadsEndpointServesReportShape asserts the GET /api/uploads DTO
// shape: an active entry (position:0) and a waiting entry with its 1-based
// position, both with their exact fields round-tripped.
func TestUploadsEndpointServesReportShape(t *testing.T) {
	reg := prometheus.NewRegistry()
	uploads := func() UploadReport {
		return UploadReport{
			Slots:     2,
			Active:    1,
			Queued:    1,
			Truncated: 0,
			Uploads: []UploadEntry{
				{Username: "alice", Filename: `Music\a.flac`, Active: true, Position: 0, Size: 1000, BytesWritten: 250},
				{Username: "bob", Filename: `Music\b.flac`, Active: false, Position: 1, Size: 2000, BytesWritten: 0},
			},
		}
	}
	h := newUploadsTestHandler(reg, uploads)

	req := httptest.NewRequest(http.MethodGet, "/api/uploads", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got uploadsDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want true")
	}
	if got.Slots != 2 || got.Active != 1 || got.Queued != 1 || got.Truncated != 0 {
		t.Errorf("counters = %+v, want slots=2 active=1 queued=1 truncated=0", got)
	}
	if len(got.Uploads) != 2 {
		t.Fatalf("uploads = %+v, want 2 entries", got.Uploads)
	}
	active := got.Uploads[0]
	if active.Username != "alice" || active.Filename != `Music\a.flac` || !active.Active || active.Position != 0 || active.Size != 1000 || active.BytesWritten != 250 {
		t.Errorf("active entry = %+v, want alice active at position 0", active)
	}
	waiting := got.Uploads[1]
	if waiting.Username != "bob" || waiting.Filename != `Music\b.flac` || waiting.Active || waiting.Position != 1 || waiting.Size != 2000 || waiting.BytesWritten != 0 {
		t.Errorf("waiting entry = %+v, want bob waiting at position 1", waiting)
	}
}

// TestUploadsEndpointEmptyUploadsEmitsEmptyArrayNotNull asserts uploads is
// always "[]" in JSON, never null, matching toSharesDTO's convention.
func TestUploadsEndpointEmptyUploadsEmitsEmptyArrayNotNull(t *testing.T) {
	reg := prometheus.NewRegistry()
	uploads := func() UploadReport { return UploadReport{} }
	h := newUploadsTestHandler(reg, uploads)

	req := httptest.NewRequest(http.MethodGet, "/api/uploads", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uploads":[]`) {
		t.Errorf(`expected "uploads":[] in body, got %s`, body)
	}
}

// TestUploadsEndpointNilUploadsReportsDisabled asserts GET /api/uploads
// answers 200 with enabled:false rather than a zero-value report that would
// look like an empty-but-configured upload manager when native Soulseek
// uploads are not enabled in the config.
func TestUploadsEndpointNilUploadsReportsDisabled(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newUploadsTestHandler(reg, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/uploads", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got uploadsDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false when Uploads dep is nil")
	}
	if got.Uploads == nil || len(got.Uploads) != 0 {
		t.Errorf("Uploads = %+v, want empty non-nil slice", got.Uploads)
	}
}

// TestUploadsEndpointRejectsNonGET asserts GET-only, matching
// registerShares's GET-branch method check.
func TestUploadsEndpointRejectsNonGET(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newUploadsTestHandler(reg, func() UploadReport { return UploadReport{} })

	req := httptest.NewRequest(http.MethodPost, "/api/uploads", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow header = %q, want %q", allow, "GET")
	}
}
