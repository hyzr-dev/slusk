package slskd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnqueueReturnsID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			t.Errorf("missing api key header")
		}
		json.NewEncoder(w).Encode(map[string]string{"id": "guid-123"})
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	id, err := c.Enqueue(context.Background(), "bob", "album/01.flac")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id != "guid-123" {
		t.Errorf("id = %q, want guid-123", id)
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
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
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
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
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
