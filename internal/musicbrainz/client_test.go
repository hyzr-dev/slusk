package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchArtists(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if got := r.Header.Get("User-Agent"); got == "" || got == "slskdarr/1.0 ()" {
			t.Errorf("User-Agent not identifying: %q", got)
		}
		w.Write([]byte(`{"count":1,"artists":[
		  {"id":"artist-1","name":"Metallica","type":"Group","country":"US","disambiguation":"","score":100}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.SearchArtists(context.Background(), "Metallica")
	if err != nil {
		t.Fatalf("SearchArtists: %v", err)
	}
	if len(got) != 1 || got[0].ID != "artist-1" || got[0].Name != "Metallica" || got[0].Score != 100 {
		t.Fatalf("unexpected: %+v", got)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// TestSearchArtistsSurfacesTotalPastCap covers issue #321's review finding:
// MusicBrainz's count can exceed the returned slice's length when the
// result was capped, and that truncation signal must survive rather than
// being discarded.
func TestSearchArtistsSurfacesTotalPastCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"count":250,"artists":[{"id":"artist-1","name":"Metallica","score":100}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.SearchArtists(context.Background(), "Metallica")
	if err != nil {
		t.Fatalf("SearchArtists: %v", err)
	}
	if total != 250 || total == len(got) {
		t.Fatalf("total = %d, want 250 (exceeding len(got)=%d)", total, len(got))
	}
}

func TestReleaseGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"release-group-count":1,"release-groups":[
		  {"id":"rg-1","title":"Ride the Lightning","first-release-date":"1984-07-27","primary-type":"Album","secondary-types":[]}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.ReleaseGroups(context.Background(), "artist-1")
	if err != nil {
		t.Fatalf("ReleaseGroups: %v", err)
	}
	if len(got) != 1 || got[0].ID != "rg-1" || got[0].Title != "Ride the Lightning" {
		t.Fatalf("unexpected: %+v", got)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// TestReleaseGroupsSurfacesTotalPastCap covers issue #321's review finding:
// MusicBrainz's release-group-count can exceed the returned slice's length
// when the result was capped, and that truncation signal must survive
// rather than being discarded.
func TestReleaseGroupsSurfacesTotalPastCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"release-group-count":150,"release-groups":[{"id":"rg-1","title":"Ride the Lightning"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.ReleaseGroups(context.Background(), "artist-1")
	if err != nil {
		t.Fatalf("ReleaseGroups: %v", err)
	}
	if total != 150 || total == len(got) {
		t.Fatalf("total = %d, want 150 (exceeding len(got)=%d)", total, len(got))
	}
}

// TestReleasesSumsMultiDiscMedia covers issue #321's worked example: a
// release's track count is the sum of every medium's track-count, since a
// multi-disc edition reports several media entries.
func TestReleasesSumsMultiDiscMedia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"release-count":1,"releases":[
		  {"id":"rel-1","title":"Deluxe Box","date":"2016","country":"US","status":"Official",
		   "media":[{"format":"CD","track-count":30},{"format":"CD","track-count":34}]}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.Releases(context.Background(), "rg-1")
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(got) != 1 || !got[0].TrackCountKnown || got[0].TrackCount != 64 {
		t.Fatalf("unexpected: %+v", got)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// TestReleasesSurfacesTotalPastCap covers issue #321's review finding:
// MusicBrainz's release-count can exceed the returned slice's length when
// the result was capped, and that truncation signal must survive rather
// than being discarded.
func TestReleasesSurfacesTotalPastCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"release-count":60,"releases":[{"id":"rel-1","title":"Ride the Lightning"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.Releases(context.Background(), "rg-1")
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if total != 60 || total == len(got) {
		t.Fatalf("total = %d, want 60 (exceeding len(got)=%d)", total, len(got))
	}
}

// TestReleasesNoMediaIsUnknownNotZero covers issue #321's explicit warning:
// an edition with no media data must map to an unknown track count, never a
// silent 0 that would be wrongly counted in a min/max band.
func TestReleasesNoMediaIsUnknownNotZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"release-count":1,"releases":[
		  {"id":"rel-1","title":"No Media Info","date":"1990","country":"XE","status":"Official"}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, _, err := c.Releases(context.Background(), "rg-1")
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(got) != 1 || got[0].TrackCountKnown || got[0].TrackCount != 0 {
		t.Fatalf("expected unknown track count, got: %+v", got)
	}
}

func TestGetNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	if _, _, err := c.SearchArtists(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
}

func TestNoContactRefusesToRequest(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, _, err := c.SearchArtists(context.Background(), "x"); !errors.Is(err, ErrNoContact) {
		t.Fatalf("SearchArtists with no contact = %v, want ErrNoContact", err)
	}
	if requests != 0 {
		t.Fatalf("expected no HTTP request without a contact, got %d", requests)
	}
}

// TestRateLimiterSerializesRequests covers the 1 req/s ceiling: two
// concurrent calls must not both hit the server within the same instant.
// The two calls use distinct queries so each is a genuine cache miss - two
// identical queries would race the cache (whichever request's response is
// cached first could make the second call a cache hit that never reaches
// the server), which would flake this test on something it does not claim
// to measure. The ~1s wall-clock assertion is inherent to testing a real
// rate limiter's timing and is accepted here rather than mocked away.
func TestRateLimiterSerializesRequests(t *testing.T) {
	hits := make(chan time.Time, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- time.Now()
		w.Write([]byte(`{"count":0,"artists":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	done := make(chan struct{}, 2)
	for _, query := range []string{"x", "y"} {
		go func(query string) {
			_, _, _ = c.SearchArtists(context.Background(), query)
			done <- struct{}{}
		}(query)
	}
	<-done
	<-done
	close(hits)
	var times []time.Time
	for ts := range hits {
		times = append(times, ts)
	}
	if len(times) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(times))
	}
	gap := times[1].Sub(times[0])
	if gap < 0 {
		gap = -gap
	}
	if gap < 900*time.Millisecond {
		t.Fatalf("expected the two requests to be spaced by ~1s, got %v apart", gap)
	}
}

func TestCacheHitSkipsSecondRequest(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write([]byte(`{"count":1,"artists":[{"id":"a","name":"A","score":100}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com", WithCacheTTL(time.Minute))
	if _, _, err := c.SearchArtists(context.Background(), "x"); err != nil {
		t.Fatalf("first SearchArtists: %v", err)
	}
	if _, _, err := c.SearchArtists(context.Background(), "x"); err != nil {
		t.Fatalf("second SearchArtists: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly 1 HTTP request across two identical calls, got %d", requests)
	}
}

func TestCacheRoundTripsDecodedValue(t *testing.T) {
	c := newTTLCache(time.Minute)
	body := []byte(`{"a":1}`)
	c.set("k", body)
	got, ok := c.get("k")
	if !ok {
		t.Fatal("expected a cache hit")
	}
	var decoded map[string]int
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal cached body: %v", err)
	}
	if decoded["a"] != 1 {
		t.Fatalf("unexpected decoded value: %+v", decoded)
	}
}

func TestCacheExpires(t *testing.T) {
	c := newTTLCache(time.Millisecond)
	c.set("k", []byte(`{}`))
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Fatal("expected the entry to have expired")
	}
}
