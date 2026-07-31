package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/store"
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
	wantRejectionDetail := "rejected 1 candidates: 1 above maximum track count, 0 below minimum track count, 0 not matching the requested album"
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
	wantDetail := "rejected 2000 candidates: 2000 above maximum track count, 0 below minimum track count, 0 not matching the requested album"
	if rejectionEvents[0].Detail != wantDetail {
		t.Errorf("rejection detail = %q, want %q", rejectionEvents[0].Detail, wantDetail)
	}
}

func TestDiscoveryEmptySearchBacksOffExponentially(t *testing.T) {
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
	if jobs[0].Retries != 1 {
		t.Errorf("expected retries=1, got %d", jobs[0].Retries)
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
	if jobs2[0].Retries != 2 {
		t.Errorf("expected retries=2, got %d", jobs2[0].Retries)
	}
	wantNotBefore2 := now2.Add(1 * time.Hour)
	if diff := jobs2[0].NotBefore.Sub(wantNotBefore2); diff < -time.Second || diff > time.Second {
		t.Errorf("expected not_before ~= %v, got %v", wantNotBefore2, *jobs2[0].NotBefore)
	}
}

func TestDiscoveryFailsJobAtMaxRetries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	wanted := map[int64]core.WantedRelease{1: {ID: 1, Title: "Album", ArtistName: "Artist"}}
	music := &fakeMusic{wanted: []core.WantedRelease{wanted[1]}}
	searcher := &fakeSearcher{}
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
