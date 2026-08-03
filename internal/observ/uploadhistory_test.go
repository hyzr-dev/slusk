package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyzr-dev/slusk/internal/core"
)

func newUploadHistoryTestHandler(reg *prometheus.Registry, history UploadHistoryFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.UploadHistory = history
	return NewServer(deps)
}

// uploadHistoryRows builds n synthetic entries with descending ids, the order
// the store returns them in.
func uploadHistoryRows(n int) []core.UploadHistoryEntry {
	started := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	rows := make([]core.UploadHistoryEntry, n)
	for i := range rows {
		rows[i] = core.UploadHistoryEntry{
			ID:                int64(n - i),
			Username:          "alice",
			Filename:          `Music\a.flac`,
			Size:              1000,
			BytesSent:         1000,
			AvgBytesPerSecond: 500,
			Status:            core.UploadCompleted,
			StartedAt:         started,
			FinishedAt:        started.Add(2 * time.Second),
		}
	}
	return rows
}

// TestUploadHistoryEndpointServesRowShape pins the DTO field-for-field,
// including that a rejected row keeps its honest zeroes rather than being
// dressed up into something a UI would render as a real rate.
func TestUploadHistoryEndpointServesRowShape(t *testing.T) {
	started := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	rows := []core.UploadHistoryEntry{
		{
			ID: 2, Username: "alice", Filename: `Music\a.flac`, Size: 1000, BytesSent: 1000,
			AvgBytesPerSecond: 500, Status: core.UploadCompleted,
			StartedAt: started, FinishedAt: started.Add(2 * time.Second),
		},
		{
			ID: 1, Username: "bob", Filename: `Music\b.flac`, Size: 2000, BytesSent: 0,
			AvgBytesPerSecond: 0, Status: core.UploadRejected, Detail: "peer declined",
			StartedAt: started, FinishedAt: started.Add(time.Second),
		},
	}
	h := newUploadHistoryTestHandler(prometheus.NewRegistry(), func(context.Context, int, int64) ([]core.UploadHistoryEntry, error) {
		return rows, nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got uploadHistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HasMore {
		t.Error("HasMore = true, want false for a short page")
	}
	if len(got.Uploads) != 2 {
		t.Fatalf("uploads = %+v, want 2 entries", got.Uploads)
	}
	completed := got.Uploads[0]
	if completed.ID != 2 || completed.Username != "alice" || completed.Filename != `Music\a.flac` ||
		completed.Size != 1000 || completed.BytesSent != 1000 || completed.AvgBytesPerSecond != 500 ||
		completed.Status != "completed" || completed.Detail != "" {
		t.Errorf("completed entry = %+v", completed)
	}
	if completed.StartedAt != started.Format(timeFormat) || completed.FinishedAt != started.Add(2*time.Second).Format(timeFormat) {
		t.Errorf("timestamps = %q/%q", completed.StartedAt, completed.FinishedAt)
	}
	rejected := got.Uploads[1]
	if rejected.Status != "rejected" || rejected.Detail != "peer declined" ||
		rejected.BytesSent != 0 || rejected.AvgBytesPerSecond != 0 {
		t.Errorf("rejected entry = %+v, want zero bytes/rate preserved", rejected)
	}
}

// TestUploadHistoryEndpointPagination asserts the limit+1 probe: the handler
// asks the store for one extra row, reports hasMore, and trims the extra row
// off before serving it.
func TestUploadHistoryEndpointPagination(t *testing.T) {
	var gotLimit int
	var gotBefore int64
	h := newUploadHistoryTestHandler(prometheus.NewRegistry(), func(_ context.Context, limit int, beforeID int64) ([]core.UploadHistoryEntry, error) {
		gotLimit, gotBefore = limit, beforeID
		return uploadHistoryRows(limit), nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/history?limit=3&before=42", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotLimit != 4 {
		t.Errorf("store limit = %d, want limit+1 = 4", gotLimit)
	}
	if gotBefore != 42 {
		t.Errorf("store beforeID = %d, want 42", gotBefore)
	}

	var got uploadHistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HasMore {
		t.Error("HasMore = false, want true when the store returned limit+1 rows")
	}
	if len(got.Uploads) != 3 {
		t.Errorf("served %d rows, want the requested 3 with the probe row trimmed", len(got.Uploads))
	}
}

// TestUploadHistoryLimitClamping pins the default and the ceiling, so a caller
// cannot turn a paginated endpoint back into an unbounded one.
func TestUploadHistoryLimitClamping(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", defaultUploadHistoryLimit},
		{"?limit=", defaultUploadHistoryLimit},
		{"?limit=0", defaultUploadHistoryLimit},
		{"?limit=-5", defaultUploadHistoryLimit},
		{"?limit=abc", defaultUploadHistoryLimit},
		{"?limit=10", 10},
		{"?limit=100000", maxUploadHistoryLimit},
	} {
		var gotLimit int
		h := newUploadHistoryTestHandler(prometheus.NewRegistry(), func(_ context.Context, limit int, _ int64) ([]core.UploadHistoryEntry, error) {
			gotLimit = limit
			return nil, nil
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/history"+tc.query, nil))
		if gotLimit != tc.want+1 {
			t.Errorf("%q: store limit = %d, want %d (clamped %d, plus the hasMore probe)", tc.query, gotLimit, tc.want+1, tc.want)
		}
	}
}

// TestUploadHistoryEndpointEmptyEmitsEmptyArrayNotNull keeps the JSON shape
// stable for a client that iterates the field unconditionally.
func TestUploadHistoryEndpointEmptyEmitsEmptyArrayNotNull(t *testing.T) {
	h := newUploadHistoryTestHandler(prometheus.NewRegistry(), func(context.Context, int, int64) ([]core.UploadHistoryEntry, error) {
		return nil, nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"uploads":[]`) {
		t.Errorf(`expected "uploads":[] in body, got %s`, body)
	}
}

// TestUploadHistoryEndpointStoreErrorIsOpaque asserts the store's error text
// never reaches the response: upload errors wrap local filesystem paths.
func TestUploadHistoryEndpointStoreErrorIsOpaque(t *testing.T) {
	h := newUploadHistoryTestHandler(prometheus.NewRegistry(), func(context.Context, int, int64) ([]core.UploadHistoryEntry, error) {
		return nil, errors.New("open /home/samuel/music/secret.flac: permission denied")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/history", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/home/samuel") {
		t.Errorf("response leaked the underlying error: %s", rec.Body.String())
	}
}

// TestUploadHistoryEndpointNilDepIs503 asserts a missing dependency answers
// 503 rather than an empty page that would read as "you have never uploaded
// anything".
func TestUploadHistoryEndpointNilDepIs503(t *testing.T) {
	h := newUploadHistoryTestHandler(prometheus.NewRegistry(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/history", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want 503", rec.Code)
	}
}

// TestUploadHistoryEndpointRejectsNonGET matches registerUploads' GET-only
// live endpoint.
func TestUploadHistoryEndpointRejectsNonGET(t *testing.T) {
	h := newUploadHistoryTestHandler(prometheus.NewRegistry(), func(context.Context, int, int64) ([]core.UploadHistoryEntry, error) {
		return nil, nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/uploads/history", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow header = %q, want %q", allow, "GET")
	}
}
