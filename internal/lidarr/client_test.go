package lidarr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWantedMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("missing api key")
		}
		w.Write([]byte(`{"records":[
		  {"id":11,"title":"Album A","artist":{"artistName":"Artist X"}}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	got, err := c.WantedMissing(context.Background())
	if err != nil {
		t.Fatalf("WantedMissing: %v", err)
	}
	if len(got) != 1 || got[0].ID != 11 || got[0].ArtistName != "Artist X" {
		t.Fatalf("unexpected: %+v", got)
	}
}
