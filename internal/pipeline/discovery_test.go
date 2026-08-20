package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/matcher"
	"github.com/hyzr-dev/slusk/internal/store"
)

// fakeWantedSource is a WantedSource fake so tests can hand Discovery an
// album map without constructing a real WantedSync.
type fakeWantedSource struct {
	wanted map[int64]core.WantedRelease
}

func (f *fakeWantedSource) Wanted() map[int64]core.WantedRelease { return f.wanted }

// newDiscoveryParams builds DiscoveryParams over a fresh store-backed
// fixture. Ranker uses the real matcher.NewWeighted scorer rather than a
// fake: its ranking logic (grouping, floor, sort order) is exactly what the
// candidate-cap and ordering assertions below need to exercise, and
// constructing it is trivial, so a fake would only need to reimplement it.
func newDiscoveryParams(t *testing.T, music *fakeMusic, searcher *fakeSearcher, wanted map[int64]core.WantedRelease) (DiscoveryParams, *store.Store) {
	t.Helper()
	st := newBackedStore(t)
	return DiscoveryParams{
		Store:         st,
		Peers:         searcher,
		Music:         music,
		Ranker:        matcher.NewWeighted(matcher.Weights{Format: 1, Bitrate: 1, FileCount: 1}, 0),
		WantedSource:  &fakeWantedSource{wanted: wanted},
		SearchTimeout: 5 * time.Second,
		MaxCandidates: 2,
		MaxRetries:    3,
		BackoffBase:   15 * time.Minute,
		BackoffCap:    24 * time.Hour,
		Interval:      30 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	}, st
}

func TestDiscoveryCachesRankedCandidates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}}}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "good1", Filename: "good1/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "good1", Filename: "good1/Artist - Album/02.flac", Size: 10, BitRate: 900},
		{Username: "good2", Filename: "good2/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "good2", Filename: "good2/Artist - Album/02.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	// Add a third, oversized candidate directly to the fake results so the
	// track-band filter has something to reject: 5 files against a band of
	// [2, 2] (the album's one known release has 2 tracks).
	searcher.results = append(searcher.results, []core.SearchResult{
		{Username: "toobig", Filename: "toobig/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "toobig", Filename: "toobig/Artist - Album/02.flac", Size: 10, BitRate: 900},
		{Username: "toobig", Filename: "toobig/Artist - Album/03.flac", Size: 10, BitRate: 900},
		{Username: "toobig", Filename: "toobig/Artist - Album/04.flac", Size: 10, BitRate: 900},
		{Username: "toobig", Filename: "toobig/Artist - Album/05.flac", Size: 10, BitRate: 900},
	}...)

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(got) != 1 || got[0].ID != job.ID {
		t.Fatalf("expected job %d in SELECTING, got %+v", job.ID, got)
	}
	if got[0].Retries != 0 {
		t.Errorf("expected retries reset to 0, got %d", got[0].Retries)
	}

	c1, ok1, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil {
		t.Fatalf("NextNewCandidate: %v", err)
	}
	if !ok1 {
		t.Fatalf("expected at least one cached candidate")
	}
	if c1.Username == "toobig" {
		t.Errorf("oversized candidate should have been rejected, got it cached")
	}

	// Drain every NEW candidate deterministically (highest score first, per
	// NextNewCandidate's ordering) to assert exactly 2 rows were persisted:
	// 3 search results in, 1 ("toobig") excluded by the track-band filter.
	seen := map[string]bool{c1.Username: true}
	if err := st.FailCandidate(ctx, c1.ID, "drained for test assertion", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}
	count := 1
	for {
		c, ok, err := st.NextNewCandidate(ctx, job.ID)
		if err != nil {
			t.Fatalf("NextNewCandidate (drain): %v", err)
		}
		if !ok {
			break
		}
		if c.Username == "toobig" {
			t.Errorf("oversized candidate should have been rejected, got it cached")
		}
		seen[c.Username] = true
		count++
		if err := st.FailCandidate(ctx, c.ID, "drained for test assertion", now); err != nil {
			t.Fatalf("FailCandidate: %v", err)
		}
	}
	if count != 2 {
		t.Errorf("expected exactly 2 cached candidates, drained %d", count)
	}
	wantUsernames := map[string]bool{"good1": true, "good2": true}
	if len(seen) != len(wantUsernames) {
		t.Errorf("expected drained usernames %v, got %v", wantUsernames, seen)
	}
	for u := range wantUsernames {
		if !seen[u] {
			t.Errorf("expected %q among drained candidates, got %v", u, seen)
		}
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var rejectionDetail string
	for _, e := range events {
		if e.Event == core.EventCandidateRejected {
			rejectionDetail = e.Detail
			break
		}
	}
	wantRejectionDetail := "rejected 1 candidates: 1 above maximum track count, 0 below minimum track count, 0 not matching the requested album, 0 already failed for this job"
	if rejectionDetail != wantRejectionDetail {
		t.Errorf("candidate rejection detail = %q, want %q; events %+v", rejectionDetail, wantRejectionDetail, events)
	}
}

func TestDiscoverySummarizesThousandsOfRejectedCandidates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{
		wanted:        []core.WantedRelease{wanted[1]},
		albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}},
	}
	const rejectedCandidates = 2000
	results := make([]core.SearchResult, 0, rejectedCandidates*3)
	for i := 0; i < rejectedCandidates; i++ {
		username := fmt.Sprintf("oversized-%04d", i)
		for track := 1; track <= 3; track++ {
			results = append(results, core.SearchResult{
				Username: username,
				Filename: fmt.Sprintf("%s/%02d.flac", username, track),
				Size:     10,
				BitRate:  900,
			})
		}
	}
	searcher := &fakeSearcher{results: results}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := NewDiscovery(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var rejectionEvents []core.JobEvent
	for _, event := range events {
		if event.Event == core.EventCandidateRejected {
			rejectionEvents = append(rejectionEvents, event)
		}
	}
	if len(rejectionEvents) != 1 {
		t.Fatalf("candidate rejection events = %d, want 1; all events %+v", len(rejectionEvents), events)
	}
	wantDetail := "rejected 2000 candidates: 2000 above maximum track count, 0 below minimum track count, 0 not matching the requested album, 0 already failed for this job"
	if rejectionEvents[0].Detail != wantDetail {
		t.Errorf("rejection detail = %q, want %q", rejectionEvents[0].Detail, wantDetail)
	}
}

// TestDiscoveryEmptySearchBacksOffOnItsOwnCurveWithoutTouchingRetries
// (issue #334): a search cycle where the Soulseek network returns zero raw
// results (primary and fallback both empty) must back off on empty_searches
// alone - retries must stay at 0, since retries is reserved for "peers
// answered but every candidate was rejected by filtering".
func TestDiscoveryEmptySearchBacksOffOnItsOwnCurveWithoutTouchingRetries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{} // no results ever, primary or fallback
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now.Add(35*time.Minute), 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job still WANTED and runnable after 35m, got %+v", jobs)
	}
	if jobs[0].Retries != 0 {
		t.Errorf("expected retries=0 (empty search must not touch it), got %d", jobs[0].Retries)
	}
	if jobs[0].EmptySearches != 1 {
		t.Errorf("expected empty_searches=1, got %d", jobs[0].EmptySearches)
	}
	if jobs[0].NotBefore == nil {
		t.Fatalf("expected not_before to be set")
	}
	wantNotBefore := now.Add(30 * time.Minute)
	if diff := jobs[0].NotBefore.Sub(wantNotBefore); diff < -time.Second || diff > time.Second {
		t.Errorf("expected not_before ~= %v, got %v", wantNotBefore, *jobs[0].NotBefore)
	}

	now2 := now.Add(35 * time.Minute)
	if err := d.Tick(ctx, now2); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	jobs2, err := st.RunnableJobsInState(ctx, core.StateWanted, now2.Add(65*time.Minute), 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState 2: %v", err)
	}
	if len(jobs2) != 1 || jobs2[0].ID != job.ID {
		t.Fatalf("expected job still WANTED and runnable, got %+v", jobs2)
	}
	if jobs2[0].Retries != 0 {
		t.Errorf("expected retries=0, got %d", jobs2[0].Retries)
	}
	if jobs2[0].EmptySearches != 2 {
		t.Errorf("expected empty_searches=2, got %d", jobs2[0].EmptySearches)
	}
	wantNotBefore2 := now2.Add(1 * time.Hour)
	if diff := jobs2[0].NotBefore.Sub(wantNotBefore2); diff < -time.Second || diff > time.Second {
		t.Errorf("expected not_before ~= %v, got %v", wantNotBefore2, *jobs2[0].NotBefore)
	}
}

// TestDiscoveryEmptySearchNeverFailsJobEvenAtHighStreak (issue #334): unlike
// retries, empty_searches has no terminal branch - a job stuck at a high
// streak stays WANTED and keeps retrying at the backoff cap forever, rather
// than being marked FAILED and stranded for failed_revive_after.
func TestDiscoveryEmptySearchNeverFailsJobEvenAtHighStreak(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{} // no results ever, primary or fallback
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	p.MaxRetries = 3

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	// Pre-set an empty_searches count far beyond MaxRetries: if the empty
	// path ever consulted MaxRetries it would fail the job here. The
	// zero time.Time is fine as notBefore - only empty_searches is under
	// test here, and Tick below overwrites not_before with a real value.
	if err := st.SetJobEmptySearchBackoff(ctx, job.ID, 50, time.Time{}, now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now.Add(48*time.Hour), 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job still WANTED, never FAILED, got %+v", jobs)
	}
	if jobs[0].EmptySearches != 51 {
		t.Errorf("expected empty_searches=51, got %d", jobs[0].EmptySearches)
	}

	failed, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState FAILED: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("expected no FAILED jobs, got %+v", failed)
	}
}

// TestDiscoveryFailsJobAtMaxRetries (issue #334): the retry budget is only
// consumed when peers actually answer but every candidate is rejected by
// filtering - not when the network returns nothing at all. Results here are
// deliberately non-empty (one candidate, one file, below the album's 2-track
// band) so survivors ends up empty via filtering rather than via a raw-zero
// search.
func TestDiscoveryFailsJobAtMaxRetries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{
		wanted:        []core.WantedRelease{wanted[1]},
		albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}},
	}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "toofew", Filename: "toofew/Artist - Album/01.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	p.MaxRetries = 3

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	// Pre-set retries to maxRetries-1 so this tick's failure crosses the line.
	if err := st.SetJobBackoff(ctx, job.ID, p.MaxRetries-1, time.Time{}, now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED, got %+v", jobs)
	}
	if jobs[0].FailedAt == nil {
		t.Errorf("expected failed_at to be set")
	}
}

func TestDiscoveryFallbackQuery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album (Deluxe Edition)", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	primary := "Artist Album (Deluxe Edition)"
	fallback := matcher.NormalizeQuery(primary)
	searcher := &fakeSearcher{resultsForQuery: map[string][]core.SearchResult{
		fallback: {
			{Username: "peer", Filename: "peer/Artist - Album (Deluxe Edition)/01.flac", Size: 10, BitRate: 900},
		},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(searcher.queries) != 2 {
		t.Fatalf("expected 2 searches (primary + fallback), got %d: %v", len(searcher.queries), searcher.queries)
	}
	if searcher.queries[0] != primary {
		t.Errorf("expected first query %q, got %q", primary, searcher.queries[0])
	}
	if searcher.queries[1] != fallback {
		t.Errorf("expected fallback query %q, got %q", fallback, searcher.queries[1])
	}

	got, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(got) != 1 || got[0].ID != job.ID {
		t.Fatalf("expected job in SELECTING, got %+v", got)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var sawFallback bool
	for _, e := range events {
		if e.Event == core.EventSearchFallback {
			sawFallback = true
		}
	}
	if !sawFallback {
		t.Errorf("expected an EventSearchFallback event, got %+v", events)
	}
}

// TestDiscoveryTokenDropRewriteFiresAtThreshold (issue #334): once a job has
// accumulated emptySearchRewriteThreshold consecutive empty-search cycles,
// a search that is still empty after the normalized-query fallback gets one
// more attempt with a single artist token dropped (matcher.DropTokenQuery),
// exactly like "Bob Dylan Desire" (0 raw results) vs "Dylan Desire" (real
// hits) from the issue's own observation.
func TestDiscoveryTokenDropRewriteFiresAtThreshold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Desire", ArtistName: "Bob Dylan"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	primary := "Bob Dylan Desire"
	rewrite := "Dylan Desire" // matcher.DropTokenQuery("Bob Dylan", "Desire", 0)
	searcher := &fakeSearcher{resultsForQuery: map[string][]core.SearchResult{
		rewrite: {
			{Username: "peer", Filename: "peer/Bob Dylan - Desire/01.flac", Size: 10, BitRate: 900},
		},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.SetJobEmptySearchBackoff(ctx, job.ID, emptySearchRewriteThreshold, time.Time{}, now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// NormalizeQuery(primary) is a no-op here (no brackets/punctuation to
	// strip), so the existing normalized-query fallback is skipped and
	// exactly one extra search - the token-dropped rewrite - is issued.
	if len(searcher.queries) != 2 {
		t.Fatalf("expected 2 searches (primary + token-dropped rewrite), got %d: %v", len(searcher.queries), searcher.queries)
	}
	if searcher.queries[0] != primary {
		t.Errorf("expected first query %q, got %q", primary, searcher.queries[0])
	}
	if searcher.queries[1] != rewrite {
		t.Errorf("expected rewrite query %q, got %q", rewrite, searcher.queries[1])
	}

	got, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(got) != 1 || got[0].ID != job.ID {
		t.Fatalf("expected job in SELECTING (rewrite found a match), got %+v", got)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var sawFallback bool
	for _, e := range events {
		if e.Event == core.EventSearchFallback {
			sawFallback = true
		}
	}
	if !sawFallback {
		t.Errorf("expected an EventSearchFallback event for the rewrite, got %+v", events)
	}
}

// TestDiscoveryTokenDropRewriteDoesNotFireBelowThreshold (issue #334): below
// emptySearchRewriteThreshold, a run of empty searches is ordinary network
// noise, not evidence of a persistently blocked query - no rewrite search is
// issued, and empty_searches simply advances by one.
func TestDiscoveryTokenDropRewriteDoesNotFireBelowThreshold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Desire", ArtistName: "Bob Dylan"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{} // no results for any query
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.SetJobEmptySearchBackoff(ctx, job.ID, emptySearchRewriteThreshold-1, time.Time{}, now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// NormalizeQuery(primary) is a no-op, so below the threshold the primary
	// search is the only search issued.
	if len(searcher.queries) != 1 {
		t.Fatalf("expected exactly 1 search below the rewrite threshold, got %d: %v", len(searcher.queries), searcher.queries)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now.Add(48*time.Hour), 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job still WANTED, got %+v", jobs)
	}
	if jobs[0].EmptySearches != emptySearchRewriteThreshold {
		t.Errorf("expected empty_searches=%d, got %d", emptySearchRewriteThreshold, jobs[0].EmptySearches)
	}
}

func TestDiscoveryOrdersByReleaseDateDesc(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{
		1: {ID: 1, Title: "Old", ArtistName: "Artist", ReleaseDate: "2020-01-01"},
		2: {ID: 2, Title: "New", ArtistName: "Artist", ReleaseDate: "2025-01-01"},
	}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1], wanted[2]}}
	searcher := &fakeSearcher{} // empty results either way; we only care which query fires
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	oldJob, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob old: %v", err)
	}
	if err := st.UpdateJobMetadata(ctx, oldJob.ID, "Old", "Artist", "2020-01-01", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata old: %v", err)
	}
	newJob, err := st.UpsertWantedJob(ctx, 2, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob new: %v", err)
	}
	if err := st.UpdateJobMetadata(ctx, newJob.ID, "New", "Artist", "2025-01-01", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata new: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(searcher.queries) == 0 {
		t.Fatalf("expected at least one search")
	}
	if searcher.queries[0] != "Artist New" {
		t.Errorf("expected the newer release searched first, got query %q", searcher.queries[0])
	}
}

// TestDiscoveryTrackBandFilter: releases 10 and 12 tracks → band [10,12].
// A 9-file candidate (below min) and a 30-file candidate (above max) are
// rejected; an 11-file candidate survives and is cached.
func TestDiscoveryTrackBandFilter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{
		wanted: []core.WantedRelease{wanted[1]},
		albumReleases: []core.AlbumRelease{
			{ID: 1, TrackCount: 12, Monitored: true},
			{ID: 2, TrackCount: 10},
		},
	}
	var results []core.SearchResult
	for i := 1; i <= 9; i++ {
		results = append(results, core.SearchResult{Username: "toosmall", Filename: fmt.Sprintf("toosmall/Artist - Album/%02d.flac", i), Size: 10, BitRate: 900})
	}
	for i := 1; i <= 11; i++ {
		results = append(results, core.SearchResult{Username: "justright", Filename: fmt.Sprintf("justright/Artist - Album/%02d.flac", i), Size: 10, BitRate: 900})
	}
	for i := 1; i <= 30; i++ {
		results = append(results, core.SearchResult{Username: "toobig", Filename: fmt.Sprintf("toobig/Artist - Album/%02d.flac", i), Size: 10, BitRate: 900})
	}
	searcher := &fakeSearcher{results: results}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 1 || cands[0].Username != "justright" {
		t.Fatalf("expected exactly the 11-file candidate cached, got %+v", cands)
	}

	got, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(got) != 1 || got[0].ID != job.ID {
		t.Fatalf("expected job %d in SELECTING, got %+v", job.ID, got)
	}
	if got[0].MinTrackCount != 10 || got[0].MaxTrackCount != 12 {
		t.Errorf("expected persisted band (10,12), got (%d,%d)", got[0].MinTrackCount, got[0].MaxTrackCount)
	}
}

// TestDiscoveryTrackBandUnknownSkipsFilter: no releases with a positive
// track count → band (0,0) → no size filtering; all candidates cached.
func TestDiscoveryTrackBandUnknownSkipsFilter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: nil}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "peer", Filename: "peer/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "peer", Filename: "peer/Artist - Album/02.flac", Size: 10, BitRate: 900},
		{Username: "peer", Filename: "peer/Artist - Album/03.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 1 || cands[0].Username != "peer" {
		t.Fatalf("expected the 3-file candidate cached despite unknown band, got %+v", cands)
	}
}

// fakeDiscoveryMetrics is a local DiscoveryMetrics fake counting
// IncAlbumReleasesError calls, mirroring the fakeSink pattern used for
// Downloading's MetricsSink.
type fakeDiscoveryMetrics struct {
	albumReleasesErrors int
	albumTracksErrors   int
}

func (f *fakeDiscoveryMetrics) IncAlbumReleasesError() { f.albumReleasesErrors++ }
func (f *fakeDiscoveryMetrics) IncAlbumTracksError()   { f.albumTracksErrors++ }

// TestDiscoveryAlbumReleasesErrorLeavesJobUntouched: an AlbumReleases error
// aborts the pass without spending retry budget (same as the old AlbumStatus
// error handling), before any Soulseek search is issued (issue #92), and
// increments the AlbumReleasesErrors metric.
func TestDiscoveryAlbumReleasesErrorLeavesJobUntouched(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleasesErr: errors.New("boom")}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "peer", Filename: "peer/01.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	m := &fakeDiscoveryMetrics{}
	p.Metrics = m

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job still WANTED, got %+v", jobs)
	}
	if jobs[0].Retries != 0 {
		t.Errorf("expected retries untouched (0), got %d", jobs[0].Retries)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("expected no candidates cached, got %+v", cands)
	}
	if len(searcher.queries) != 0 {
		t.Errorf("expected no Soulseek search when AlbumReleases fails, got %v", searcher.queries)
	}
	if m.albumReleasesErrors != 1 {
		t.Errorf("expected AlbumReleasesErrors metric incremented once, got %d", m.albumReleasesErrors)
	}
}

// TestDiscoveryNoSearchPassOnAlbumReleasesError asserts a job whose
// AlbumReleases call fails (an abort path) records nothing (issue #88) and
// never reaches the Soulseek search (issue #92). No metrics sink is wired
// here to exercise the nil-guard.
func TestDiscoveryNoSearchPassOnAlbumReleasesError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleasesErr: errors.New("boom")}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "peer", Filename: "peer/01.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 0 {
		t.Errorf("expected no search pass when AlbumReleases fails, got %+v", passes)
	}
	if len(searcher.queries) != 0 {
		t.Errorf("expected no Soulseek search when AlbumReleases fails, got %v", searcher.queries)
	}
}

// failingSearchPassStore wraps a real store, promoting every DiscoveryStore
// method except RecordSearchPass (always fails), so tests can assert
// Discovery.Tick swallows a RecordSearchPass failure - same best-effort
// policy as recordEvent - rather than failing the tick.
type failingSearchPassStore struct {
	*store.Store
}

func (f *failingSearchPassStore) RecordSearchPass(ctx context.Context, p core.SearchPass) error {
	return errors.New("record search pass boom")
}

// TestDiscoveryRecordsSearchPassMatchedOnCandidatesPath asserts a completed
// search cycle that finds viable candidates records Matched=1 (issue #88).
func TestDiscoveryRecordsSearchPassMatchedOnCandidatesPath(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}}}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "good1", Filename: "good1/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "good1", Filename: "good1/Artist - Album/02.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	m := &fakeDiscoveryMetrics{}
	p.Metrics = m

	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 1 {
		t.Fatalf("expected 1 recorded pass, got %d: %+v", len(passes), passes)
	}
	if passes[0].Searched != 1 || passes[0].Matched != 1 {
		t.Errorf("pass = %+v, want Searched=1 Matched=1", passes[0])
	}
	if m.albumReleasesErrors != 0 {
		t.Errorf("expected AlbumReleasesErrors metric untouched on success path, got %d", m.albumReleasesErrors)
	}
}

// TestDiscoveryRecordsSearchPassUnmatchedOnBackoffPath asserts a completed
// search cycle with no viable candidates records Matched=0 (issue #88).
func TestDiscoveryRecordsSearchPassUnmatchedOnBackoffPath(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{} // no results ever, primary or fallback -> backs off
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 1 {
		t.Fatalf("expected 1 recorded pass, got %d: %+v", len(passes), passes)
	}
	if passes[0].Searched != 1 || passes[0].Matched != 0 {
		t.Errorf("pass = %+v, want Searched=1 Matched=0", passes[0])
	}
}

// TestDiscoveryNoSearchPassOnIdleTick asserts a tick with no runnable WANTED
// job records nothing (issue #88).
func TestDiscoveryNoSearchPassOnIdleTick(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newDiscoveryParams(t, &fakeMusic{}, &fakeSearcher{}, map[int64]core.WantedRelease{})

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 0 {
		t.Errorf("expected no search pass on an idle tick, got %+v", passes)
	}
}

// TestDiscoveryNoSearchPassOnSnapshotMiss asserts a job whose album is
// absent from the WantedSource snapshot (an abort path) records nothing
// (issue #88).
func TestDiscoveryNoSearchPassOnSnapshotMiss(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newDiscoveryParams(t, &fakeMusic{}, &fakeSearcher{}, map[int64]core.WantedRelease{})
	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 0 {
		t.Errorf("expected no search pass when the album is missing from the wanted snapshot, got %+v", passes)
	}
}

// TestDiscoveryNoSearchPassOnSearchError asserts a Search error (an abort
// path) propagates and records nothing (issue #88).
func TestDiscoveryNoSearchPassOnSearchError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{searchErrForQuery: map[string]error{"Artist Album": errors.New("search boom")}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err == nil {
		t.Fatal("expected Tick to propagate the search error")
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 0 {
		t.Errorf("expected no search pass recorded on a search error, got %+v", passes)
	}
}

// TestDiscoveryRecordSearchPassFailureDoesNotFailTick asserts a
// RecordSearchPass write failure is swallowed (best-effort, same policy as
// recordEvent) rather than failing the tick (issue #88).
func TestDiscoveryRecordSearchPassFailureDoesNotFailTick(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{} // empty results -> backoff path, still "completed"
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	p.Store = &failingSearchPassStore{Store: st}
	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick should swallow a RecordSearchPass failure, got: %v", err)
	}
}

// TestDiscoveryRejectsIrrelevantCandidate is the issue #316 regression case:
// a search for "The Absence"/"The Absence" network-matches a peer whose real
// share is an unrelated Kansas album (every query token appears somewhere in
// that path), and the relevance gate must reject it, leaving no survivors.
func TestDiscoveryRejectsIrrelevantCandidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "The Absence", ArtistName: "The Absence"}}
	music := &fakeMusic{
		wanted: []core.WantedRelease{wanted[1]},
		albumTracks: []core.AlbumTrack{
			{Title: "Wartorn"}, {Title: "Riders of the Plague"}, {Title: "Skin and Bones"},
		},
	}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "wrongpeer", Filename: `Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\01 - The Absence Of Presence.flac`, Size: 10, BitRate: 900},
		{Username: "wrongpeer", Filename: `Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\02 - Throwing Mountains.flac`, Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected the irrelevant candidate rejected, got %+v", cands)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var rejectionDetail string
	for _, e := range events {
		if e.Event == core.EventCandidateRejected {
			rejectionDetail = e.Detail
		}
	}
	if !strings.Contains(rejectionDetail, "not matching the requested album") {
		t.Errorf("expected a rejection event mentioning the album mismatch, got %q; events %+v", rejectionDetail, events)
	}
}

// TestDiscoverySearchExcludedFailsJobImmediately confirms a
// core.ErrSearchExcluded from the searcher (issue #319: the Soulseek server
// told every peer to ignore this exact query) fails the job on the very first
// hit rather than consuming the retry/backoff budget - retrying is pure waste
// since the excluded-phrase list is stable across retries - and that the
// error itself never escapes Tick (it is a routine, expected outcome, not a
// module failure).
func TestDiscoverySearchExcludedFailsJobImmediately(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	excludedErr := fmt.Errorf("%w: %q", core.ErrSearchExcluded, "artist album")
	searcher := &fakeSearcher{searchErrForQuery: map[string]error{"Artist Album": excludedErr}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	p.MaxRetries = 50 // high on purpose: an excluded search must not touch this budget

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick should swallow the excluded-search error, got: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED immediately, got %+v", jobs)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var sawExcluded bool
	for _, ev := range events {
		if ev.Event == core.EventSearchExcluded {
			sawExcluded = true
			if !strings.Contains(ev.Detail, "Artist Album") || !strings.Contains(ev.Detail, "artist album") {
				t.Errorf("search_excluded detail = %q, want it to name the query and the matched phrase", ev.Detail)
			}
		}
	}
	if !sawExcluded {
		t.Errorf("expected a search_excluded event, got %+v", events)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 1 || passes[0].Matched != 0 {
		t.Errorf("expected one unmatched search pass, got %+v", passes)
	}
}

// TestDiscoveryAlbumTracksErrorDegradesToDirectoryCheck asserts an
// AlbumTracks error DEGRADES the relevance gate to a directory-only check
// rather than aborting discovery (unlike AlbumReleases, which is load-bearing
// for the track-count band and does abort - see
// TestDiscoveryAlbumReleasesErrorLeavesJobUntouched, the opposite
// assertion): Peers.Search still runs, the AlbumTracksErrors metric is
// incremented, and a directory-relevant candidate still survives.
func TestDiscoveryAlbumTracksErrorDegradesToDirectoryCheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{
		wanted:         []core.WantedRelease{wanted[1]},
		albumTracksErr: errors.New("boom"),
	}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "peer", Filename: "peer/Artist - Album/01.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	m := &fakeDiscoveryMetrics{}
	p.Metrics = m

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(searcher.queries) == 0 {
		t.Fatalf("expected Peers.Search still called despite the AlbumTracks error")
	}
	if m.albumTracksErrors != 1 {
		t.Errorf("expected AlbumTracksErrors metric incremented once, got %d", m.albumTracksErrors)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 1 || cands[0].Username != "peer" {
		t.Fatalf("expected the directory-relevant candidate to still survive, got %+v", cands)
	}
}

// TestDiscoverySkipsAlbumTracksWhenSearchIsEmpty asserts AlbumTracks is not
// fetched at all when Peers.Search (both the primary and normalized-fallback
// query) returns no results - there is nothing for the relevance gate to
// check against, so the extra Lidarr call would be pure waste on every job
// whose search comes back empty.
func TestDiscoverySkipsAlbumTracksWhenSearchIsEmpty(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{} // no results ever, primary or fallback
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	if _, err := st.UpsertWantedJob(ctx, 1, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if music.albumTracksCalls != 0 {
		t.Errorf("expected AlbumTracks not called on an empty search, got %d calls", music.albumTracksCalls)
	}
}

// TestDiscoverySearchExcludedOnFallbackFailsJobImmediately confirms
// core.ErrSearchExcluded is handled the same way when it comes back from the
// *normalized fallback* search (issue #319) as when it comes back from the
// primary one: the primary query returns zero raw results (not an error),
// normalizeQuery strips its "(Deluxe Edition)" suffix into a different
// fallback query, and that fallback search is the one the server excludes.
// The job must still fail on the spot, without touching the retry budget,
// and a search_excluded event must still be recorded.
func TestDiscoverySearchExcludedOnFallbackFailsJobImmediately(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album (Deluxe Edition)", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	excludedErr := fmt.Errorf("%w: %q", core.ErrSearchExcluded, "artist album")
	searcher := &fakeSearcher{
		// Primary query "Artist Album (Deluxe Edition)" is absent from
		// resultsForQuery/searchErrForQuery, so it falls through to the
		// zero-value default `results` (nil) - a normal empty result, not an
		// error - which is what triggers the normalized fallback below.
		resultsForQuery:   map[string][]core.SearchResult{},
		searchErrForQuery: map[string]error{"Artist Album": excludedErr},
	}
	p, st := newDiscoveryParams(t, music, searcher, wanted)
	p.MaxRetries = 50 // high on purpose: an excluded search must not touch this budget

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick should swallow the excluded-search error, got: %v", err)
	}

	// Assert the fallback is what tripped the exclusion. The event detail cannot
	// carry that on its own: the fallback query is a substring of the primary
	// one, so a detail naming "Artist Album" would read the same either way.
	if len(searcher.queries) != 2 {
		t.Fatalf("expected the primary search then the normalized fallback, got %q", searcher.queries)
	}
	if searcher.queries[1] != "Artist Album" {
		t.Fatalf("second search = %q, want the normalized fallback query", searcher.queries[1])
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED immediately, got %+v", jobs)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var sawExcluded bool
	for _, ev := range events {
		if ev.Event == core.EventSearchExcluded {
			sawExcluded = true
			if !strings.Contains(ev.Detail, "Artist Album") || !strings.Contains(ev.Detail, "artist album") {
				t.Errorf("search_excluded detail = %q, want it to name the fallback query and the matched phrase", ev.Detail)
			}
		}
	}
	if !sawExcluded {
		t.Errorf("expected a search_excluded event, got %+v", events)
	}
}

// TestDiscoverySkipsAndFailsManualJob covers the Discovery source guard (issue
// #347): a manual
// job must never reach Peers.Search, whatever LidarrAlbumID it happens to
// carry. It reaches WANTED here the same way a production zombie would (the
// #58/#155 bug this self-heals): created straight into DOWNLOADING like any
// manual job, then found stuck in WANTED - a state a manual job should never
// be able to reach at all, which is exactly why Discovery must guard on
// Source rather than assume LidarrAlbumID means "safe to search".
func TestDiscoverySkipsAndFailsManualJob(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{}
	searcher := &fakeSearcher{}
	p, st := newDiscoveryParams(t, music, searcher, map[int64]core.WantedRelease{})

	job, err := st.CreateManualJob(ctx, "Album", "Artist", "alice", "",
		[]store.ManualJobFile{{Filename: "a.flac", Size: 1}}, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateWanted, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(searcher.queries) != 0 {
		t.Errorf("expected Peers.Search never called for a manual job, got queries %v", searcher.queries)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected manual job FAILED, got %+v (%v)", jobs, err)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Event == core.EventJobFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("expected EventJobFailed recorded, got %+v", events)
	}
}

// TestDiscoverySkipsCandidateThatAlreadyFailed is issue #317 end to end: a
// candidate that failed for this job must not be cached again on the next
// search cycle, even though the search is unchanged and the network answers
// with the same peers. Before the rejection history existed, the second Tick
// re-cached "bad" and the job downloaded the same broken files again, once per
// retry.
func TestDiscoverySkipsCandidateThatAlreadyFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}}}
	// "bad" outranks "good" on bitrate, so it is the candidate Selecting picks
	// first and therefore the one that fails - the ordering matters, otherwise
	// the assertion below could pass for the wrong reason.
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "bad", Filename: `bad\Artist - Album\01.flac`, Size: 10, BitRate: 1000},
		{Username: "bad", Filename: `bad\Artist - Album\02.flac`, Size: 10, BitRate: 1000},
		{Username: "good", Filename: `good\Artist - Album\01.flac`, Size: 10, BitRate: 900},
		{Username: "good", Filename: `good\Artist - Album\02.flac`, Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("first Tick: %v", err)
	}

	// Walk the top candidate through the failure the pipeline would: activated
	// by Selecting, then failed by Downloading or Importing.
	cand, ok, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !ok {
		t.Fatalf("NextNewCandidate: %v ok=%v", err, ok)
	}
	if cand.Username != "bad" {
		t.Fatalf("expected the highest-scoring candidate to be %q, got %q", "bad", cand.Username)
	}
	activated, _, err := st.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 100, now.Add(time.Hour), now)
	if err != nil || !activated {
		t.Fatalf("ActivateCandidateWithTransfers: %v activated=%v", err, activated)
	}
	if _, err := st.RejectCandidateAndAdvance(ctx, cand.ID, job.ID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}

	// Selecting's candidates-exhausted path: back to WANTED, candidate cache
	// gone, retries bumped.
	if err := st.ResetJobToWanted(ctx, job.ID, core.StateSelecting, 1, nil, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	later := now.Add(time.Hour)
	if err := d.Tick(ctx, later); err != nil {
		t.Fatalf("second Tick: %v", err)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected exactly 1 cached candidate on the second cycle, got %d (%+v)", len(cands), cands)
	}
	if cands[0].Username != "good" {
		t.Errorf("expected the failed peer to be filtered out, got %q re-cached", cands[0].Username)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	sawRejection := false
	for _, e := range events {
		if e.Event == core.EventCandidateRejected && strings.Contains(e.Detail, "1 already failed for this job") {
			sawRejection = true
		}
	}
	if !sawRejection {
		t.Errorf("expected a candidate_rejected event naming the already-failed candidate, got %+v", events)
	}
}

// TestDiscoveryStillCachesCandidateFromSamePeerInAnotherDirectory guards the
// granularity choice: a rejection is keyed on (username, release directory),
// not username alone. The same peer may well share the right album in a
// different folder, and blacklisting the whole peer would throw that away.
func TestDiscoveryStillCachesCandidateFromSamePeerInAnotherDirectory(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}}}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "alice", Filename: `alice\bad rip\Artist - Album\01.flac`, Size: 10, BitRate: 1000},
		{Username: "alice", Filename: `alice\bad rip\Artist - Album\02.flac`, Size: 10, BitRate: 1000},
		{Username: "alice", Filename: `alice\good rip\Artist - Album\01.flac`, Size: 10, BitRate: 900},
		{Username: "alice", Filename: `alice\good rip\Artist - Album\02.flac`, Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	d := NewDiscovery(p)
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	cand, ok, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !ok {
		t.Fatalf("NextNewCandidate: %v ok=%v", err, ok)
	}
	activated, _, err := st.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 100, now.Add(time.Hour), now)
	if err != nil || !activated {
		t.Fatalf("ActivateCandidateWithTransfers: %v activated=%v", err, activated)
	}
	if _, err := st.RejectCandidateAndAdvance(ctx, cand.ID, job.ID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := st.ResetJobToWanted(ctx, job.ID, core.StateSelecting, 1, nil, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	if err := d.Tick(ctx, now.Add(time.Hour)); err != nil {
		t.Fatalf("second Tick: %v", err)
	}

	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("CandidatesForJob: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected the peer's other directory to still be cached, got %d candidates (%+v)", len(cands), cands)
	}
	if cands[0].Username != "alice" {
		t.Fatalf("expected the surviving candidate to still be alice, got %q", cands[0].Username)
	}
	if got := cands[0].Files[0].Filename; !strings.Contains(got, "good rip") {
		t.Errorf("expected the non-rejected directory to survive, got %q", got)
	}
}

// ruinPeerHistory records enough fresh failures against a peer to put it below
// matcher.LastResortThreshold. matcher.ReliabilityCountCap bounds the decayed
// count, so recording more than the cap changes nothing - the loop runs to it
// exactly. artistID 0 keeps the history global, which is the shape of the
// peers this tier targets: they fail across many different albums.
func ruinPeerHistory(t *testing.T, st *store.Store, username string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	for range int(matcher.ReliabilityCountCap) {
		if err := st.RecordAttemptOutcome(ctx, 0, username, false, now.Add(-time.Hour)); err != nil {
			t.Fatalf("RecordAttemptOutcome: %v", err)
		}
	}
}

func TestDiscoveryEnqueuesASoleLastResortCandidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	// The tier is an ordering, not a filter, and this is the case that proves
	// it: an album whose only seeder is a ruined peer must still be fetchable.
	// Discovery truncates the ranked list at MaxCandidates, so a demotion that
	// pushed the candidate past the cut would silently make such an album
	// unobtainable - the exact failure #317 rejected the filter approach over.
	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}}}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "ruined", Filename: "ruined/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "ruined", Filename: "ruined/Artist - Album/02.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	ruinPeerHistory(t, st, "ruined", now)

	if err := NewDiscovery(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(got) != 1 || got[0].ID != job.ID {
		t.Fatalf("expected job %d to advance to SELECTING, got %+v", job.ID, got)
	}

	cand, ok, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil {
		t.Fatalf("NextNewCandidate: %v", err)
	}
	if !ok {
		t.Fatal("sole last-resort candidate was not cached; the tier is acting as a filter")
	}
	if cand.Username != "ruined" {
		t.Errorf("cached candidate = %q, want \"ruined\"", cand.Username)
	}
	if !cand.LastResort {
		t.Error("cached candidate LastResort = false, want true (the flag must persist to explain the choice)")
	}
}

func TestDiscoveryCachesLastResortFalseForAnUntriedPeer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	// The companion to the test above: without it, a flag that was hardcoded
	// true would pass every assertion made about it.
	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}, albumReleases: []core.AlbumRelease{{ID: 1, TrackCount: 2, Monitored: true}}}
	searcher := &fakeSearcher{results: []core.SearchResult{
		{Username: "untried", Filename: "untried/Artist - Album/01.flac", Size: 10, BitRate: 900},
		{Username: "untried", Filename: "untried/Artist - Album/02.flac", Size: 10, BitRate: 900},
	}}
	p, st := newDiscoveryParams(t, music, searcher, wanted)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	if err := NewDiscovery(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	cand, ok, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !ok {
		t.Fatalf("NextNewCandidate: found=%v (%v)", ok, err)
	}
	if cand.LastResort {
		t.Error("untried peer cached with LastResort = true, want false")
	}
}
