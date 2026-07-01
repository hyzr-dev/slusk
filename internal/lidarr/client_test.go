package lidarr

import (
	"context"
	"fmt"
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

func TestWantedMissingPaginates(t *testing.T) {
	const total = 5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "1", "":
			w.Write([]byte(fmt.Sprintf(`{"page":1,"pageSize":2,"totalRecords":%d,"records":[
			  {"id":1,"title":"A1","artist":{"artistName":"Artist"}},
			  {"id":2,"title":"A2","artist":{"artistName":"Artist"}}
			]}`, total)))
		case "2":
			w.Write([]byte(fmt.Sprintf(`{"page":2,"pageSize":2,"totalRecords":%d,"records":[
			  {"id":3,"title":"A3","artist":{"artistName":"Artist"}},
			  {"id":4,"title":"A4","artist":{"artistName":"Artist"}}
			]}`, total)))
		case "3":
			w.Write([]byte(fmt.Sprintf(`{"page":3,"pageSize":2,"totalRecords":%d,"records":[
			  {"id":5,"title":"A5","artist":{"artistName":"Artist"}}
			]}`, total)))
		default:
			w.Write([]byte(`{"page":4,"pageSize":2,"totalRecords":5,"records":[]}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	got, err := c.WantedMissing(context.Background())
	if err != nil {
		t.Fatalf("WantedMissing: %v", err)
	}
	if len(got) != total {
		t.Fatalf("expected %d records across pages, got %d: %+v", total, len(got), got)
	}
	for i, want := range []int64{1, 2, 3, 4, 5} {
		if got[i].ID != want {
			t.Errorf("record %d: ID = %d, want %d", i, got[i].ID, want)
		}
	}
}

func TestManualImportCandidatesParsesRejections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/manualimport" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`[
		  {"id":1,"path":"/music/slskd-downloads/A/01.flac","folderName":"A","artistId":5,"albumId":9,"rejections":[]},
		  {"id":2,"path":"/music/slskd-downloads/A/02.mp3","folderName":"A","artistId":5,"albumId":9,
		   "rejections":[{"reason":"Quality Unknown not in profile","type":"permanent"}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	items, err := c.ManualImportCandidates(context.Background(), "/music/slskd-downloads/A")
	if err != nil {
		t.Fatalf("ManualImportCandidates: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if !items[0].Importable {
		t.Errorf("item 0 has no rejections, should be importable")
	}
	if items[1].Importable {
		t.Errorf("item 1 has a rejection, should not be importable")
	}
	if items[1].Rejections[0] != "Quality Unknown not in profile" {
		t.Errorf("rejection reason not parsed: %v", items[1].Rejections)
	}
}
