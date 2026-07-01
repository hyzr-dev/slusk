package slskd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
