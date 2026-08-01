package lidarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestWantedMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("missing api key")
		}
		w.Write([]byte(`{"records":[
		  {"id":11,"title":"Album A","releaseDate":"2024-03-15T00:00:00Z","artist":{"artistName":"Artist X"}}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	got, err := c.WantedMissing(context.Background())
	if err != nil {
		t.Fatalf("WantedMissing: %v", err)
	}
	if len(got) != 1 || got[0].ID != 11 || got[0].ArtistName != "Artist X" || got[0].ReleaseDate != "2024-03-15T00:00:00Z" {
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

func TestAlbumReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/album/42" {
			t.Errorf("path = %q, want /api/v1/album/42", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != "key" {
			t.Errorf("api key = %q, want key", got)
		}
		fmt.Fprint(w, `{"id":42,"title":"X","releases":[
			{"id":1,"albumId":42,"trackCount":12,"monitored":true},
			{"id":2,"albumId":42,"trackCount":10,"monitored":false}
		]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	rels, err := c.AlbumReleases(context.Background(), 42)
	if err != nil {
		t.Fatalf("AlbumReleases: %v", err)
	}
	want := []core.AlbumRelease{
		{ID: 1, TrackCount: 12, Monitored: true},
		{ID: 2, TrackCount: 10, Monitored: false},
	}
	if len(rels) != len(want) {
		t.Fatalf("got %d releases, want %d", len(rels), len(want))
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Errorf("release %d = %+v, want %+v", i, rels[i], want[i])
		}
	}
}

func TestAlbumTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/track" {
			t.Errorf("path = %q, want /api/v1/track", r.URL.Path)
		}
		if got := r.URL.Query().Get("albumId"); got != "42" {
			t.Errorf("albumId query param = %q, want 42", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "key" {
			t.Errorf("api key = %q, want key", got)
		}
		// trackNumber/mediumNumber are deliberately unlike the old (now-removed)
		// decode target - a number where "1" was a string, a string where an int
		// was - to pin that AlbumTracks no longer cares about their type at all
		// (see core.AlbumTrack's doc comment): only "title" is decoded, so a
		// type drift on fields nobody reads must never fail the whole call.
		fmt.Fprint(w, `[
			{"id":1,"albumId":42,"title":"Wartorn","trackNumber":1,"mediumNumber":"A"},
			{"id":2,"albumId":42,"title":"Riders","trackNumber":"A1","mediumNumber":1,"unknownField":{"nested":true}}
		]`)
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	tracks, err := c.AlbumTracks(context.Background(), 42)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	want := []core.AlbumTrack{
		{Title: "Wartorn"},
		{Title: "Riders"},
	}
	if len(tracks) != len(want) {
		t.Fatalf("got %d tracks, want %d", len(tracks), len(want))
	}
	for i := range want {
		if tracks[i] != want[i] {
			t.Errorf("track %d = %+v, want %+v", i, tracks[i], want[i])
		}
	}
}

func TestAlbumTracksErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	if _, err := c.AlbumTracks(context.Background(), 42); err == nil {
		t.Fatal("expected an error on a non-2xx response")
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
	items := []core.ImportItem{
		{
			ID: 1, Path: "/music/slskd-downloads/A/01.flac",
			ArtistID: 5, AlbumID: 9, AlbumReleaseID: 13,
			TrackIDs:     []int64{101, 102},
			Quality:      json.RawMessage(`{"quality":{"id":6,"name":"FLAC"}}`),
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

// TestManualImportCandidatesUsesScanTimeout proves the manualimport folder
// scan runs on its own, longer timeout rather than the shared client timeout:
// Lidarr parses audio tags per file during the scan, so large folders (box
// sets over NFS) legitimately exceed a normal API call's deadline.
func TestManualImportCandidatesUsesScanTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", WithManualImportTimeout(2*time.Second))
	// Shrink the general timeout below the server's delay: success proves the
	// scan is not bound by it.
	c.http.Timeout = 20 * time.Millisecond
	if _, err := c.ManualImportCandidates(context.Background(), "/f"); err != nil {
		t.Fatalf("scan should survive past the general client timeout: %v", err)
	}

	slow := New(srv.URL, "k", WithManualImportTimeout(20*time.Millisecond))
	if _, err := slow.ManualImportCandidates(context.Background(), "/f"); err == nil {
		t.Fatal("scan exceeding its own timeout should fail")
	}
}

func TestPing(t *testing.T) {
	t.Run("succeeds on 200 with the api key", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/system/status" {
				t.Errorf("path = %q, want /api/v1/system/status", r.URL.Path)
			}
			if r.Header.Get("X-Api-Key") != "k" {
				t.Errorf("missing api key")
			}
			w.Write([]byte(`{"version":"2.5.0"}`))
		}))
		defer srv.Close()
		if err := New(srv.URL, "k").Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})

	t.Run("reports a bad api key distinctly", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		err := New(srv.URL, "wrong").Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "API key") {
			t.Fatalf("want an API-key error, got %v", err)
		}
	})

	t.Run("surfaces other non-2xx statuses", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		err := New(srv.URL, "k").Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("want a status-500 error, got %v", err)
		}
	})

	t.Run("wraps a transport failure as unreachable", func(t *testing.T) {
		// Close the server immediately so the dial is refused.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()
		err := New(url, "k").Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "could not reach Lidarr") {
			t.Fatalf("want an unreachable error, got %v", err)
		}
	})
}

func TestAlbumByForeignID(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("foreignAlbumId"); got != "rg-1" {
				t.Errorf("foreignAlbumId = %q, want rg-1", got)
			}
			w.Write([]byte(`[{"id":42,"artistId":7,"monitored":true}]`))
		}))
		defer srv.Close()
		album, found, err := New(srv.URL, "k").AlbumByForeignID(context.Background(), "rg-1")
		if err != nil {
			t.Fatalf("AlbumByForeignID: %v", err)
		}
		if !found || album.ID != 42 || album.ArtistID != 7 || !album.Monitored {
			t.Fatalf("unexpected: found=%v album=%+v", found, album)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		_, found, err := New(srv.URL, "k").AlbumByForeignID(context.Background(), "rg-1")
		if err != nil {
			t.Fatalf("AlbumByForeignID: %v", err)
		}
		if found {
			t.Fatal("expected found = false for an empty array")
		}
	})

	t.Run("unreachable is an error, not an absence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		_, found, err := New(srv.URL, "k").AlbumByForeignID(context.Background(), "rg-1")
		if err == nil {
			t.Fatal("expected an error for a non-2xx response")
		}
		if found {
			t.Fatal("found must be false alongside an error")
		}
	})
}

func TestArtistByMBID(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("mbId"); got != "artist-1" {
				t.Errorf("mbId = %q, want artist-1", got)
			}
			w.Write([]byte(`[{"id":9,"foreignArtistId":"artist-1","artistName":"Aphex Twin","monitored":true}]`))
		}))
		defer srv.Close()
		artist, found, err := New(srv.URL, "k").ArtistByMBID(context.Background(), "artist-1")
		if err != nil {
			t.Fatalf("ArtistByMBID: %v", err)
		}
		if !found || artist.ID != 9 || artist.ForeignArtistID != "artist-1" || artist.Name != "Aphex Twin" || !artist.Monitored {
			t.Fatalf("unexpected: found=%v artist=%+v", found, artist)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		_, found, err := New(srv.URL, "k").ArtistByMBID(context.Background(), "artist-1")
		if err != nil {
			t.Fatalf("ArtistByMBID: %v", err)
		}
		if found {
			t.Fatal("expected found = false for an empty array")
		}
	})

	t.Run("unreachable is an error, not an absence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		_, found, err := New(srv.URL, "k").ArtistByMBID(context.Background(), "artist-1")
		if err == nil {
			t.Fatal("expected an error for a non-2xx response")
		}
		if found {
			t.Fatal("found must be false alongside an error")
		}
	})
}

func TestRootFolders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"path":"/music/library","accessible":true,"freeSpace":123,"defaultQualityProfileId":2,"defaultMetadataProfileId":1}]`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").RootFolders(context.Background())
	if err != nil {
		t.Fatalf("RootFolders: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].Path != "/music/library" || !got[0].Accessible ||
		got[0].FreeSpace != 123 || got[0].DefaultQualityProfileID != 2 || got[0].DefaultMetadataProfileID != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestQualityProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/qualityprofile" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`[{"id":1,"name":"Any"},{"id":2,"name":"Lossless"}]`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").QualityProfiles(context.Background())
	if err != nil {
		t.Fatalf("QualityProfiles: %v", err)
	}
	if len(got) != 2 || got[1].Name != "Lossless" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestMetadataProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metadataprofile" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`[{"id":1,"name":"Standard"}]`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").MetadataProfiles(context.Background())
	if err != nil {
		t.Fatalf("MetadataProfiles: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Standard" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestAddArtist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/artist" {
			t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["monitorNewItems"] != "none" {
			t.Errorf("monitorNewItems = %v, want none", body["monitorNewItems"])
		}
		addOptions, ok := body["addOptions"].(map[string]any)
		if !ok {
			t.Errorf("addOptions missing or wrong type: %v", body["addOptions"])
		}
		if addOptions["monitor"] != "none" {
			t.Errorf("addOptions.monitor = %v, want none", addOptions["monitor"])
		}
		if addOptions["searchForMissingAlbums"] != false {
			t.Errorf("addOptions.searchForMissingAlbums = %v, want false", addOptions["searchForMissingAlbums"])
		}
		if body["foreignArtistId"] != "artist-1" || body["artistName"] != "Aphex Twin" ||
			body["qualityProfileId"] != float64(2) || body["metadataProfileId"] != float64(1) ||
			body["rootFolderPath"] != "/music/library" {
			t.Errorf("unexpected request body: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":9,"foreignArtistId":"artist-1","artistName":"Aphex Twin","monitored":false}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").AddArtist(context.Background(), core.AddArtistRequest{
		ForeignArtistID: "artist-1", ArtistName: "Aphex Twin",
		QualityProfileID: 2, MetadataProfileID: 1, RootFolderPath: "/music/library",
	})
	if err != nil {
		t.Fatalf("AddArtist: %v", err)
	}
	if got.ID != 9 || got.ForeignArtistID != "artist-1" || got.Name != "Aphex Twin" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestAddArtistNon201IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k").AddArtist(context.Background(), core.AddArtistRequest{ForeignArtistID: "artist-1"})
	if err == nil {
		t.Fatal("expected an error for a non-201 response")
	}
}

// TestAddArtistTimeoutButArtistWasCreatedIsSuccess covers the live-probe
// finding in .lidarr-endpoints-verified.md: POST /artist can exceed the
// client's timeout while still creating the artist server-side. AddArtist
// must re-check ArtistByMBID and report success, not a transport error.
func TestAddArtistTimeoutButArtistWasCreatedIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artist":
			time.Sleep(150 * time.Millisecond) // outlast the client's short timeout below
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":9,"foreignArtistId":"artist-1","artistName":"Aphex Twin","monitored":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artist":
			w.Write([]byte(`[{"id":9,"foreignArtistId":"artist-1","artistName":"Aphex Twin","monitored":false}]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k", WithAddArtistTimeout(20*time.Millisecond))
	got, err := c.AddArtist(context.Background(), core.AddArtistRequest{ForeignArtistID: "artist-1", ArtistName: "Aphex Twin"})
	if err != nil {
		t.Fatalf("AddArtist should recover via the re-check, got error: %v", err)
	}
	if got.ID != 9 || got.ForeignArtistID != "artist-1" || got.Name != "Aphex Twin" {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestAddArtistTimeoutAndArtistGenuinelyAbsentIsError covers the other side
// of the same re-check: when the create really did fail, a clean re-check
// showing the artist absent must be reported as a definite error, not as
// ErrAddArtistUncertain.
func TestAddArtistTimeoutAndArtistGenuinelyAbsentIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artist":
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":9}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artist":
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k", WithAddArtistTimeout(20*time.Millisecond), WithAddArtistRecheck(2, time.Millisecond))
	_, err := c.AddArtist(context.Background(), core.AddArtistRequest{ForeignArtistID: "artist-1"})
	if err == nil {
		t.Fatal("expected an error when the create times out and the re-check finds no artist")
	}
	if errors.Is(err, ErrAddArtistUncertain) {
		t.Fatalf("a clean re-check showing absence must not be reported as uncertain, got %v", err)
	}
}

// TestAddArtistTimeoutAndRecheckFailsIsUncertain covers the third outcome:
// the create fails at the transport level AND the re-check itself fails, so
// the true state is unknown. The caller must be able to tell this apart from
// a definite failure via errors.Is(err, ErrAddArtistUncertain).
func TestAddArtistTimeoutAndRecheckFailsIsUncertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":9}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", WithAddArtistTimeout(20*time.Millisecond), WithAddArtistRecheck(2, time.Millisecond))
	_, err := c.AddArtist(context.Background(), core.AddArtistRequest{ForeignArtistID: "artist-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrAddArtistUncertain) {
		t.Fatalf("expected ErrAddArtistUncertain, got %v", err)
	}
}

// TestAddArtistRecheckSurvivesCancelledContext covers the second bug in the
// same recovery path: in production the context passed to AddArtist is the
// inbound request's, which is already cancelled by the time a transport
// error occurs if the browser gave up first - exactly when the re-check is
// needed most. The re-check must run against context.WithoutCancel, not ctx
// itself, or it is useless in the one case it exists for.
func TestAddArtistRecheckSurvivesCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/artist" {
			w.Write([]byte(`[{"id":9,"foreignArtistId":"artist-1","artistName":"Aphex Twin","monitored":false}]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before AddArtist is even called

	c := New(srv.URL, "k", WithAddArtistRecheck(2, time.Millisecond))
	got, err := c.AddArtist(ctx, core.AddArtistRequest{ForeignArtistID: "artist-1"})
	if err != nil {
		t.Fatalf("AddArtist should recover via the re-check even though ctx was already cancelled, got error: %v", err)
	}
	if got.ID != 9 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestSetArtistMonitored(t *testing.T) {
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/api/v1/artist/9" {
				t.Fatalf("GET path = %q", r.URL.Path)
			}
			w.Write([]byte(`{"id":9,"artistName":"Aphex Twin","monitored":false,"someOtherField":"keep-me"}`))
		case http.MethodPut:
			if r.URL.Path != "/api/v1/artist/9" {
				t.Fatalf("PUT path = %q", r.URL.Path)
			}
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()
	if err := New(srv.URL, "k").SetArtistMonitored(context.Background(), 9, true); err != nil {
		t.Fatalf("SetArtistMonitored: %v", err)
	}
	if putBody["monitored"] != true {
		t.Errorf("PUT monitored = %v, want true", putBody["monitored"])
	}
	if putBody["someOtherField"] != "keep-me" {
		t.Errorf("PUT body dropped an unrelated field: %+v", putBody)
	}
}

func TestMonitorAlbums(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/album/monitor" {
			t.Fatalf("method/path = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			AlbumIDs  []int64 `json:"albumIds"`
			Monitored bool    `json:"monitored"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(body.AlbumIDs) != 2 || body.AlbumIDs[0] != 1 || body.AlbumIDs[1] != 2 || !body.Monitored {
			t.Fatalf("unexpected body: %+v", body)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if err := New(srv.URL, "k").MonitorAlbums(context.Background(), []int64{1, 2}, true); err != nil {
		t.Fatalf("MonitorAlbums: %v", err)
	}
}

func TestRunningCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/command" {
			t.Errorf("path = %q, want /api/v1/command", r.URL.Path)
		}
		w.Write([]byte(`[{"name":"RefreshArtist","status":"started"},{"name":"RescanFolders","status":"queued"}]`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").RunningCommands(context.Background())
	if err != nil {
		t.Fatalf("RunningCommands: %v", err)
	}
	if len(got) != 2 || got[0].Name != "RefreshArtist" || got[0].Status != "started" ||
		got[1].Name != "RescanFolders" || got[1].Status != "queued" {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestRunningCommandsParsesArtistIDs covers the PR lab finding
// (.lidarr-endpoints-verified.md): a RefreshArtist triggered by our own add
// carries body.artistIds, so a caller can scope a wait to the artist actually
// being added rather than the whole instance's activity.
func TestRunningCommandsParsesArtistIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"name":"RefreshArtist","status":"started","body":{"artistIds":[28],"isNewArtist":true}},
			{"name":"RescanFolders","status":"queued","body":{}}
		]`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").RunningCommands(context.Background())
	if err != nil {
		t.Fatalf("RunningCommands: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected: %+v", got)
	}
	if len(got[0].ArtistIDs) != 1 || got[0].ArtistIDs[0] != 28 {
		t.Fatalf("got[0].ArtistIDs = %v, want [28]", got[0].ArtistIDs)
	}
	if len(got[1].ArtistIDs) != 0 {
		t.Fatalf("got[1].ArtistIDs = %v, want empty (no body.artistIds -> unscoped)", got[1].ArtistIDs)
	}
}

func TestAlbumsByArtist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("artistId"); got != "9" {
			t.Errorf("artistId = %q, want 9", got)
		}
		w.Write([]byte(`[{"id":1,"artistId":9,"monitored":false},{"id":2,"artistId":9,"monitored":false}]`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "k").AlbumsByArtist(context.Background(), 9)
	if err != nil {
		t.Fatalf("AlbumsByArtist: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("unexpected: %+v", got)
	}
}
