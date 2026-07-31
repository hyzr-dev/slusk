package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestEscapeLuceneMetacharacters covers every metacharacter issue #321
// requires escaped: + - && || ! ( ) { } [ ] ^ " ~ * ? : \ /. Unescaped input
// is not a hard error against the live API - MusicBrainz's parser silently
// accepts it - which is exactly why this is tested directly against the
// escaper rather than only inferred from a client response.
func TestEscapeLuceneMetacharacters(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"a+b", `a\+b`},
		{"a-b", `a\-b`},
		{"a&&b", `a\&\&b`},
		{"a||b", `a\|\|b`},
		{"a!b", `a\!b`},
		{"a(b)", `a\(b\)`},
		{"a{b}", `a\{b\}`},
		{"a[b]", `a\[b\]`},
		{"a^b", `a\^b`},
		{`a"b`, `a\"b`},
		{"a~b", `a\~b`},
		{"a*b", `a\*b`},
		{"a?b", `a\?b`},
		{"a:b", `a\:b`},
		{`a\b`, `a\\b`},
		{"a/b", `a\/b`},
	}
	for _, c := range cases {
		if got := escapeLucene(c.in); got != c.want {
			t.Errorf("escapeLucene(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEscapeLuceneDirtyFolderName covers a realistic dirty folder name
// (issue #321's worked example): unescaped, its brackets and parentheses
// turned a 141-hit targeted search into 1,122,574 hits against the live API.
func TestEscapeLuceneDirtyFolderName(t *testing.T) {
	got := escapeLucene(`Ride The Lightning [1984] (FLAC)`)
	want := `Ride The Lightning \[1984\] \(FLAC\)`
	if got != want {
		t.Fatalf("escapeLucene(dirty folder name) = %q, want %q", got, want)
	}
}

// TestEscapeLuceneNeutralisesKeywords covers issue #321's review finding:
// AND/OR/NOT/TO are Lucene boolean/range operators when they stand alone as
// a whitespace-delimited token, so releasegroup:(NOT YOUR KIND OF PEOPLE)
// parsed "NOT" as negation and the correct release-group (Garbage's "Not
// Your Kind of People") was absent from the results entirely - measured
// against the live API. Lucene's keywords are case-sensitive, so a lowercase
// "not" is already a literal and must be left untouched, and a word that
// merely contains a keyword (NOTHING, ANDROMEDA, ORION) must not be mangled
// - that regression is the one this fix could easily introduce.
func TestEscapeLuceneNeutralisesKeywords(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"NOT Your Kind of People", `\NOT Your Kind of People`},
		{"Rock AND Roll", `Rock \AND Roll`},
		{"Dusk TO Dawn", `Dusk \TO Dawn`},
		{"Either OR", `Either \OR`},
		{"this is not a keyword", "this is not a keyword"},
		{"NOTHING ANDROMEDA ORION", "NOTHING ANDROMEDA ORION"},
	}
	for _, c := range cases {
		if got := escapeLucene(c.in); got != c.want {
			t.Errorf("escapeLucene(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReleaseGroupSearchQueryFuzzyWithMetacharacters covers the direct
// query-builder combination issue #321's review flagged as untested: fuzzy
// must append a bare "~" after the already-escaped album term, so "foo*"
// becomes "foo\*~", never "foo\*\~" (escaping the appended "~" itself) or
// "foo*~" (skipping the metacharacter escape).
func TestReleaseGroupSearchQueryFuzzyWithMetacharacters(t *testing.T) {
	got := releaseGroupSearchQuery("", "foo*", true)
	want := `releasegroup:(foo\*~)`
	if got != want {
		t.Fatalf("releaseGroupSearchQuery = %q, want %q", got, want)
	}
}

func TestSearchReleaseGroups(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got == "" || got == "slskdarr/1.0 ()" {
			t.Errorf("User-Agent not identifying: %q", got)
		}
		gotQuery = r.URL.Query().Get("query")
		w.Write([]byte(`{"count":2,"release-groups":[
		  {"id":"rg-1","title":"Ride the Lightning","score":100,"count":60,
		   "first-release-date":"1984-07-27","primary-type":"Album","secondary-types":[],
		   "artist-credit":[{"name":"Metallica","artist":{"id":"artist-1","name":"Metallica"}}]}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.SearchReleaseGroups(context.Background(), "Metallica", "Ride the Lightning")
	if err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if want := "artist:(Metallica) AND releasegroup:(Ride the Lightning)"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
	if len(got) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
	rg := got[0]
	if rg.ID != "rg-1" || rg.Title != "Ride the Lightning" || rg.Score != 100 || rg.EditionCount != 60 ||
		rg.ArtistName != "Metallica" || rg.ArtistID != "artist-1" {
		t.Fatalf("unexpected: %+v", rg)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
}

// TestSearchReleaseGroupsEmptyArtistStillQueries covers issue #321's
// explicit requirement: a blank artist is allowed and must still produce a
// query, just one that omits the artist:() clause.
func TestSearchReleaseGroupsEmptyArtistStillQueries(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Write([]byte(`{"count":1,"release-groups":[{"id":"rg-1","title":"Ride the Lightning"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	if _, _, err := c.SearchReleaseGroups(context.Background(), "", "Ride the Lightning"); err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if want := "releasegroup:(Ride the Lightning)"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
}

// TestSearchReleaseGroupsUsesStrictQueryFirst covers issue #321: the strict
// (non-fuzzy) query must be tried first, and a non-zero hit must not trigger
// the fuzzy retry - only ever a single request in that case.
func TestSearchReleaseGroupsUsesStrictQueryFirst(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if strings.Contains(r.URL.Query().Get("query"), "~") {
			t.Errorf("fuzzy query used on a query with hits: %s", r.URL.Query().Get("query"))
		}
		w.Write([]byte(`{"count":1,"release-groups":[{"id":"rg-1","title":"Ride the Lightning"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	if _, _, err := c.SearchReleaseGroups(context.Background(), "Metallica", "Ride the Lightning"); err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly 1 request, got %d", requests)
	}
}

// TestSearchReleaseGroupsRetriesFuzzyOnZeroHits covers issue #321's fuzzy
// retry: a strict query returning zero hits must be retried once with a
// fuzzy album term, and the retry's result (and total) is what's returned.
func TestSearchReleaseGroupsRetriesFuzzyOnZeroHits(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		if strings.Contains(q, "~") {
			w.Write([]byte(`{"count":1,"release-groups":[{"id":"rg-1","title":"Ride the Lightning","score":100}]}`))
			return
		}
		w.Write([]byte(`{"count":0,"release-groups":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.SearchReleaseGroups(context.Background(), "Metallica", "ride the lightening")
	if err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("expected 2 requests (strict then fuzzy), got %d: %v", len(queries), queries)
	}
	if want := "artist:(Metallica) AND releasegroup:(ride the lightening~)"; queries[1] != want {
		t.Errorf("fuzzy query = %q, want %q", queries[1], want)
	}
	if len(got) != 1 || got[0].ID != "rg-1" || total != 1 {
		t.Fatalf("unexpected: got=%+v total=%d", got, total)
	}
}

// TestSearchReleaseGroupsFuzzyRetryFailureKeepsEmptyResult covers issue
// #321's review finding: a strict query that legitimately returns zero hits
// (a normal "not found") must not become ErrIdentifyUnavailable just because
// the best-effort fuzzy retry for it errors. The strict leg already
// succeeded, so its empty result is the honest answer.
func TestSearchReleaseGroupsFuzzyRetryFailureKeepsEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("query"), "~") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"count":0,"release-groups":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.SearchReleaseGroups(context.Background(), "Metallica", "ride the lightening")
	if err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if len(got) != 0 || total != 0 {
		t.Fatalf("expected an empty result, got %+v total=%d", got, total)
	}
}

// TestSearchReleaseGroupsSurfacesTotalPastCap covers issue #321's review
// finding: MusicBrainz's count can exceed the returned slice's length when
// the result was capped, and that truncation signal must survive rather
// than being discarded.
func TestSearchReleaseGroupsSurfacesTotalPastCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"count":250,"release-groups":[{"id":"rg-1","title":"Ride the Lightning"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	got, total, err := c.SearchReleaseGroups(context.Background(), "Metallica", "Ride the Lightning")
	if err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if total != 250 || total == len(got) {
		t.Fatalf("total = %d, want 250 (exceeding len(got)=%d)", total, len(got))
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
	if _, _, err := c.SearchReleaseGroups(context.Background(), "", "x"); err == nil {
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
	if _, _, err := c.SearchReleaseGroups(context.Background(), "", "x"); !errors.Is(err, ErrNoContact) {
		t.Fatalf("SearchReleaseGroups with no contact = %v, want ErrNoContact", err)
	}
	if requests != 0 {
		t.Fatalf("expected no HTTP request without a contact, got %d", requests)
	}
}

// TestRateLimiterSerializesRequests covers the 1 req/s ceiling: two
// concurrent calls must not both hit the server within the same instant.
// The two calls use distinct album terms so each is a genuine cache miss -
// two identical queries would race the cache (whichever request's response
// is cached first could make the second call a cache hit that never reaches
// the server), which would flake this test on something it does not claim
// to measure. Each response reports a hit so the zero-hit fuzzy retry never
// fires and adds a third, unaccounted-for request. The ~1s wall-clock
// assertion is inherent to testing a real rate limiter's timing and is
// accepted here rather than mocked away.
func TestRateLimiterSerializesRequests(t *testing.T) {
	hits := make(chan time.Time, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- time.Now()
		w.Write([]byte(`{"count":1,"release-groups":[{"id":"rg-1","title":"x"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com")
	done := make(chan struct{}, 2)
	for _, album := range []string{"x", "y"} {
		go func(album string) {
			_, _, _ = c.SearchReleaseGroups(context.Background(), "", album)
			done <- struct{}{}
		}(album)
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
		w.Write([]byte(`{"count":1,"release-groups":[{"id":"rg-1","title":"A","score":100}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test@example.com", WithCacheTTL(time.Minute))
	if _, _, err := c.SearchReleaseGroups(context.Background(), "", "x"); err != nil {
		t.Fatalf("first SearchReleaseGroups: %v", err)
	}
	if _, _, err := c.SearchReleaseGroups(context.Background(), "", "x"); err != nil {
		t.Fatalf("second SearchReleaseGroups: %v", err)
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
