package slskd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnqueueSendsFilenameAndSize(t *testing.T) {
	var body []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			t.Errorf("missing api key header")
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]string{"id": "guid-123"})
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	id, err := c.Enqueue(context.Background(), "bob", "album/01.flac", 12345)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id != "guid-123" {
		t.Errorf("id = %q, want guid-123", id)
	}
	// slskd needs the size to request the file — a missing/zero size fails every
	// transfer. Assert both fields are sent.
	if len(body) != 1 || body[0]["filename"] != "album/01.flac" {
		t.Fatalf("filename not sent: %+v", body)
	}
	if body[0]["size"] != float64(12345) { // JSON numbers decode to float64
		t.Errorf("size not sent, got %v", body[0]["size"])
	}
}

func TestEnqueueRetriesOnServerError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Fail with a transient 503 the first two times, then succeed. slskd can
		// briefly reject/stall enqueues under load; a good candidate must not be
		// abandoned because of that.
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": "guid-ok"})
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.enqueueBackoff = time.Millisecond // keep the test fast
	id, err := c.Enqueue(context.Background(), "bob", "album/01.flac", 12345)
	if err != nil {
		t.Fatalf("Enqueue should retry through transient 503s: %v", err)
	}
	if id != "guid-ok" {
		t.Errorf("id = %q, want guid-ok", id)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts (2 failures + 1 success), got %d", calls)
	}
}

func TestEnqueueDoesNotRetryClientError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A 4xx is our fault (bad request) — retrying cannot help, so it must fail fast.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.enqueueBackoff = time.Millisecond
	_, err := c.Enqueue(context.Background(), "bob", "album/01.flac", 12345)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if calls != 1 {
		t.Errorf("4xx must not be retried; expected 1 attempt, got %d", calls)
	}
}

func TestDeleteDownloadFolderSendsBase64EncodedName(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if err := c.DeleteDownloadFolder(context.Background(), "1000 Forms of Fear (2014)"); err != nil {
		t.Fatalf("DeleteDownloadFolder: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	wantPath := "/api/v0/files/downloads/directories/" + base64.StdEncoding.EncodeToString([]byte("1000 Forms of Fear (2014)"))
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
}

func TestDeleteDownloadFolderReturnsErrorOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if err := c.DeleteDownloadFolder(context.Background(), "A"); err == nil {
		t.Fatal("expected error on non-2xx response")
	}
}

func TestIsNotFoundRecognizesA404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	err := c.DeleteDownloadFolder(context.Background(), "A")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
}

func TestIsNotFoundRejectsOtherStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	err := c.DeleteDownloadFolder(context.Background(), "A")
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = true, want false", err)
	}
}

func TestListDownloadsFlattens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"username":"bob","directories":[{"files":[
		    {"id":"g1","filename":"a.flac","state":"InProgress","size":100,"bytesTransferred":40}
		  ]}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	got, err := c.ListDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListDownloads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "g1" || got[0].Username != "bob" {
		t.Fatalf("unexpected transfers: %+v", got)
	}
	if got[0].BytesTransferred != 40 {
		t.Errorf("bytesTransferred = %d", got[0].BytesTransferred)
	}
}

func TestSearchPollsThenReturnsFlattenedResults(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			polls++
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[
			  {"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1000,
			   "files":[
			     {"filename":"@@x\\A\\01.flac","size":100,"bitRate":900,"isLocked":false},
			     {"filename":"@@x\\A\\locked.flac","size":100,"bitRate":900,"isLocked":true}
			   ]}
			]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	got, err := c.Search(context.Background(), "artist album", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if polls == 0 {
		t.Error("expected at least one poll of the search state")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result (locked file skipped), got %d", len(got))
	}
	if got[0].Username != "bob" || got[0].BitRate != 900 {
		t.Errorf("unexpected result: %+v", got[0])
	}
	if !got[0].HasFreeUploadSlot || got[0].UploadSpeed != 1000 {
		t.Errorf("per-user reliability fields not propagated: %+v", got[0])
	}
}

func TestSearchRetriesOnEmptyResults(t *testing.T) {
	var posts, respCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			posts++
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			respCalls++
			// slskd's search DB intermittently drops all responses: first search
			// comes back empty, a retry of the same query succeeds.
			if respCalls == 1 {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,
				  "files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
			}
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = time.Millisecond
	c.searchBackoff = time.Millisecond
	got, err := c.Search(context.Background(), "q", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected retry to recover results, got %d", len(got))
	}
	if posts < 2 {
		t.Errorf("expected a second search after an empty result, posts=%d", posts)
	}
}

func TestSearchGivesUpAfterRetriesWhenEmpty(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			posts++
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[]`)) // genuinely no matches: stays empty every attempt
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = time.Millisecond
	c.searchBackoff = time.Millisecond
	got, err := c.Search(context.Background(), "q", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d", len(got))
	}
	// Default is 2 retries → 3 total attempts, then give up (assume no matches).
	if posts != 3 {
		t.Errorf("expected 3 total search attempts, got %d", posts)
	}
}

func TestSearchReturnsPartialOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			// never completes -> forces the timeout path
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,"files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	got, err := c.Search(context.Background(), "q", 30*time.Millisecond)
	if err != nil {
		t.Fatalf("timeout should return partial results, not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected partial results on timeout, got %d", len(got))
	}
}
