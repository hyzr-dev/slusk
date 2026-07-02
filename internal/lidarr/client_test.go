package lidarr

import (
	"context"
	"encoding/json"
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
		  {"id":1,"path":"/music/slskd-downloads/A/01.flac","folderName":"A",
		   "artist":{"id":5},"album":{"id":9},"albumReleaseId":13,
		   "quality":{"quality":{"id":6,"name":"FLAC"}},"indexerFlags":0,
		   "disableReleaseSwitching":false,"rejections":[]},
		  {"id":2,"path":"/music/slskd-downloads/A/02.mp3","folderName":"A",
		   "artist":{"id":5},"album":{"id":9},"albumReleaseId":13,
		   "quality":{"quality":{"id":1,"name":"MP3-192"}},"indexerFlags":0,
		   "disableReleaseSwitching":false,
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
	if items[0].ArtistID != 5 || items[0].AlbumID != 9 || items[0].AlbumReleaseID != 13 {
		t.Errorf("item 0 ArtistID/AlbumID/AlbumReleaseID = %d/%d/%d, want 5/9/13",
			items[0].ArtistID, items[0].AlbumID, items[0].AlbumReleaseID)
	}
	if len(items[0].Quality) == 0 {
		t.Errorf("item 0 Quality not captured")
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

func TestManualImportCandidatesParsesTrackIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"id":1,"path":"/music/slskd-downloads/A/01.flac","folderName":"A",
		   "artist":{"id":5},"album":{"id":9},"albumReleaseId":13,
		   "tracks":[{"id":101}],"rejections":[]},
		  {"id":2,"path":"/music/slskd-downloads/A/02.flac","folderName":"A",
		   "artist":{"id":5},"album":{"id":9},"albumReleaseId":13,
		   "tracks":[],"rejections":[]}
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
	if len(items[0].TrackIDs) != 1 || items[0].TrackIDs[0] != 101 {
		t.Errorf("item 0 TrackIDs = %v, want [101]", items[0].TrackIDs)
	}
	if len(items[1].TrackIDs) != 0 {
		t.Errorf("item 1 TrackIDs = %v, want empty", items[1].TrackIDs)
	}
}

func TestAlbumStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/album/9" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"statistics":{"trackFileCount":8,"trackCount":12}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	present, total, err := c.AlbumStatus(context.Background(), 9)
	if err != nil {
		t.Fatalf("AlbumStatus: %v", err)
	}
	if present != 8 || total != 12 {
		t.Errorf("AlbumStatus = %d/%d, want 8/12", present, total)
	}
}

func TestExecuteManualImportBuildsCorrectPayload(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	items := []ManualImportItem{
		{
			ID: 1, Path: "/music/slskd-downloads/A/01.flac",
			ArtistID: 5, AlbumID: 9, AlbumReleaseID: 13,
			TrackIDs: []int64{101, 102},
			Quality:  json.RawMessage(`{"quality":{"id":6,"name":"FLAC"}}`),
			IndexerFlags: 0, DisableReleaseSwitching: false,
		},
	}
	if err := c.ExecuteManualImport(context.Background(), items); err != nil {
		t.Fatalf("ExecuteManualImport: %v", err)
	}

	files, ok := captured["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files = %v, want 1 entry", captured["files"])
	}
	f, ok := files[0].(map[string]any)
	if !ok {
		t.Fatalf("file entry not an object: %v", files[0])
	}

	want := map[string]any{
		"id":                      float64(1),
		"path":                    "/music/slskd-downloads/A/01.flac",
		"artistId":                float64(5),
		"albumId":                 float64(9),
		"albumReleaseId":          float64(13),
		"indexerFlags":            float64(0),
		"additionalFile":          false,
		"replaceExistingFiles":    true,
		"disableReleaseSwitching": false,
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("files[0][%q] = %v, want %v", k, f[k], v)
		}
	}
	trackIDs, ok := f["trackIds"].([]any)
	if !ok || len(trackIDs) != 2 || trackIDs[0] != float64(101) || trackIDs[1] != float64(102) {
		t.Errorf("files[0][trackIds] = %v, want [101 102]", f["trackIds"])
	}
	if _, ok := f["quality"]; !ok {
		t.Errorf("files[0][quality] missing")
	}
	if _, hasFolderName := f["folderName"]; hasFolderName {
		t.Errorf("files[0] should not include folderName, got %v", f["folderName"])
	}
}
