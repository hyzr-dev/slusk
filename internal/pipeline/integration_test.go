package pipeline

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// lifecycleModules bundles every pipeline module driven by these tests, all
// wired against one shared store-backed fixture and one set of fakes -
// exactly the shape a real deployment wires them in (minus the Runner, which
// these tests deliberately bypass in favor of hand-stepped Tick calls for
// determinism).
type lifecycleModules struct {
	st      *store.Store
	music   *fakeMusic
	search  *fakeSearcher
	network *fakeNetwork

	wanted   *WantedSync
	discover *Discovery
	selectS  *Selecting
	download *Downloading
	importM  *Importing
}

// newLifecycleModules constructs every module against a fresh store, sharing
// music/search/network fakes across all of them the way the real Runner
// would. maxRetries controls both Discovery's and Selecting's search-cycle
// retry budget (kept small in the exhaustion test to stay fast).
func newLifecycleModules(t *testing.T, music *fakeMusic, search *fakeSearcher, network *fakeNetwork, maxRetries int) *lifecycleModules {
	t.Helper()
	st := newBackedStore(t)
	logger := slog.New(slog.NewTextHandler(testDiscard{}, nil))

	wanted := NewWantedSync(WantedSyncParams{
		Music:             music,
		Store:             st,
		Interval:          15 * time.Minute,
		FailedReviveAfter: 30 * 24 * time.Hour,
		Logger:            logger,
	})

	discover := NewDiscovery(DiscoveryParams{
		Store:         st,
		Peers:         search,
		Music:         music,
		Ranker:        matcher.NewWeighted(matcher.Weights{Format: 1, Bitrate: 1, FileCount: 1}, 0),
		WantedSource:  wanted,
		SearchTimeout: 5 * time.Second,
		MaxCandidates: 5,
		MaxRetries:    maxRetries,
		BackoffBase:   15 * time.Minute,
		BackoffCap:    24 * time.Hour,
		Interval:      30 * time.Second,
		Logger:        logger,
	})

	selectS := NewSelecting(SelectingParams{
		Store:              st,
		Peers:              search,
		MaxActive:          5,
		MaxRetries:         maxRetries,
		BackoffBase:        15 * time.Minute,
		BackoffCap:         24 * time.Hour,
		CandidateTTL:       24 * time.Hour,
		MaxInflightPerPeer: 5,
		MaxTransferRetries: 3,
		TransferDeadline:   time.Hour,
		Interval:           30 * time.Second,
		Logger:             logger,
	})

	download := NewDownloading(DownloadingParams{
		Store:              st,
		Network:            network,
		Peers:              search,
		MaxActive:          5,
		MaxTransferRetries: 0, // any errored transfer terminates immediately, no retry churn
		StallTimeout:       time.Hour,
		MaxInflightPerPeer: 5,
		TransferDeadline:   time.Hour,
		Interval:           30 * time.Second,
		Logger:             logger,
	})

	importM := NewImporting(ImportingParams{
		Store:                st,
		Music:                music,
		Peers:                search,
		CompleteDir:          "/music/complete",
		MaxActive:            5,
		StuckAfter:           time.Hour,
		ImportConfirmTimeout: 3 * time.Minute,
		Interval:             30 * time.Second,
		Logger:               logger,
	})

	return &lifecycleModules{
		st: st, music: music, search: search, network: network,
		wanted: wanted, discover: discover, selectS: selectS, download: download, importM: importM,
	}
}

// completeTransfers marks every one of candID's queued transfers as
// "Completed, Succeeded" in the fake network (matched by the slskd id
// fakeSearcher.Enqueue assigned, "slskd-"+filename) and drives one
// Downloading.Tick, which reconciles them to COMPLETED and (once every
// transfer for the job's active candidate is done) resolves the job onward
// (IMPORTING on success).
func completeTransfers(t *testing.T, lm *lifecycleModules, candID int64, username string, files []core.CandidateFile, now time.Time) {
	t.Helper()
	ctx := context.Background()
	var downloads []slskd.Transfer
	for _, f := range files {
		downloads = append(downloads, slskd.Transfer{
			ID: "slskd-" + f.Filename, Username: username, Filename: f.Filename,
			State: "Completed, Succeeded", Size: f.Size, BytesTransferred: f.Size,
		})
	}
	lm.network.downloads = downloads
	if err := lm.download.Tick(ctx, now); err != nil {
		t.Fatalf("download.Tick (complete transfers): %v", err)
	}
	_ = candID
}

// failTransfersTerminally marks every one of candID's queued transfers as
// "Completed, Errored" in the fake network and drives one Downloading.Tick,
// which reconciles them to ERRORED (MaxTransferRetries=0 so this happens
// immediately, no retry) and (once every sibling is terminal) fails the
// candidate, returning the job to SELECTING.
func failTransfersTerminally(t *testing.T, lm *lifecycleModules, username string, files []core.CandidateFile, now time.Time) {
	t.Helper()
	ctx := context.Background()
	var downloads []slskd.Transfer
	for _, f := range files {
		downloads = append(downloads, slskd.Transfer{
			ID: "slskd-" + f.Filename, Username: username, Filename: f.Filename,
			State: "Completed, Errored", Size: f.Size,
		})
	}
	lm.network.downloads = downloads
	if err := lm.download.Tick(ctx, now); err != nil {
		t.Fatalf("download.Tick (fail transfers): %v", err)
	}
}

// TestFullLifecycleWantedToDone drives one album end to end through every
// pipeline module - WANTED -> SELECTING -> DOWNLOADING -> IMPORTING -> DONE -
// asserting the state at each transition, and that the terminal success
// landed in peer reliability history.
func TestFullLifecycleWantedToDone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	const artistID = int64(500)
	music := &fakeMusic{
		wanted: []core.WantedRelease{
			{ID: 1, Title: "Album", ArtistName: "Artist", ArtistID: artistID},
		},
		albumTotal: 1,
		manualImportItems: []core.ImportItem{
			{ID: 1, Path: "/music/complete/peer1/01.flac", Importable: true, TrackIDs: []int64{101}},
		},
	}
	search := &fakeSearcher{results: []core.SearchResult{
		{Username: "peer1", Filename: "peer1/01.flac", Size: 10, BitRate: 900},
	}}
	network := &fakeNetwork{}
	lm := newLifecycleModules(t, music, search, network, 3)

	// WANTED
	if err := lm.wanted.Tick(ctx, now); err != nil {
		t.Fatalf("wanted.Tick: %v", err)
	}
	wantedJobs, err := lm.st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState(WANTED): %v", err)
	}
	if len(wantedJobs) != 1 {
		t.Fatalf("expected 1 WANTED job, got %+v", wantedJobs)
	}
	jobID := wantedJobs[0].ID
	if wantedJobs[0].ArtistID != artistID {
		t.Fatalf("expected job's artist id cached as %d, got %d", artistID, wantedJobs[0].ArtistID)
	}

	// SELECTING (with candidates cached)
	if err := lm.discover.Tick(ctx, now); err != nil {
		t.Fatalf("discover.Tick: %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateSelecting {
		t.Fatalf("expected SELECTING after discovery, got %v", got)
	}

	// DOWNLOADING (candidate activated, transfers enqueued)
	if err := lm.selectS.Tick(ctx, now); err != nil {
		t.Fatalf("selectS.Tick: %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateDownloading {
		t.Fatalf("expected DOWNLOADING after selecting, got %v", got)
	}
	cand, found, err := lm.st.ActiveCandidate(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if cand.Username != "peer1" {
		t.Fatalf("expected peer1 activated, got %q", cand.Username)
	}
	files := []core.CandidateFile{{Filename: "peer1/01.flac", Size: 10}}

	// IMPORTING (transfers complete)
	completeTransfers(t, lm, cand.ID, "peer1", files, now)
	if got := jobStateFor(t, lm.st, jobID); got != core.StateImporting {
		t.Fatalf("expected IMPORTING after transfers complete, got %v", got)
	}

	// Importing tick 1: verify submits the import.
	if err := lm.importM.Tick(ctx, now); err != nil {
		t.Fatalf("importM.Tick (verify): %v", err)
	}
	if len(music.executedItems) != 1 {
		t.Fatalf("expected ExecuteManualImport called once, got %d", len(music.executedItems))
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateImporting {
		t.Fatalf("expected job still IMPORTING awaiting confirmation, got %v", got)
	}

	// Importing tick 2: confirm sees the album complete -> DONE.
	music.albumPresent = 1
	now2 := now.Add(time.Minute)
	if err := lm.importM.Tick(ctx, now2); err != nil {
		t.Fatalf("importM.Tick (confirm): %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateDone {
		t.Fatalf("expected DONE after import confirmed, got %v", got)
	}

	rel, err := lm.st.ReliabilityFor(ctx, artistID, []string{"peer1"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	if rel["peer1"].Artist.SuccessCount != 1 {
		t.Errorf("peer1's artist success count = %d, want 1", rel["peer1"].Artist.SuccessCount)
	}
	if rel["peer1"].Global.SuccessCount != 1 {
		t.Errorf("peer1's global success count = %d, want 1", rel["peer1"].Global.SuccessCount)
	}
}

// TestFullLifecycleFailedCandidateRotation seeds two candidates; the first
// (higher-scoring) one's transfers fail terminally, sending the job back to
// SELECTING, which activates the second candidate; that one succeeds through
// to DONE.
func TestFullLifecycleFailedCandidateRotation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{
		wanted:     []core.WantedRelease{{ID: 1, Title: "Album", ArtistName: "Artist"}},
		albumTotal: 1,
		manualImportItems: []core.ImportItem{
			{ID: 1, Path: "/music/complete/peer2/01.flac", Importable: true, TrackIDs: []int64{101}},
		},
	}
	search := &fakeSearcher{results: []core.SearchResult{
		// peer1 scores higher (better bitrate) so it is tried first.
		{Username: "peer1", Filename: "peer1/01.flac", Size: 10, BitRate: 900},
		{Username: "peer2", Filename: "peer2/01.flac", Size: 10, BitRate: 320},
	}}
	network := &fakeNetwork{}
	lm := newLifecycleModules(t, music, search, network, 3)

	if err := lm.wanted.Tick(ctx, now); err != nil {
		t.Fatalf("wanted.Tick: %v", err)
	}
	wantedJobs, err := lm.st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil || len(wantedJobs) != 1 {
		t.Fatalf("RunnableJobsInState(WANTED): %+v (%v)", wantedJobs, err)
	}
	jobID := wantedJobs[0].ID

	if err := lm.discover.Tick(ctx, now); err != nil {
		t.Fatalf("discover.Tick: %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateSelecting {
		t.Fatalf("expected SELECTING after discovery, got %v", got)
	}

	// Activate candidate 1 (peer1, higher score).
	if err := lm.selectS.Tick(ctx, now); err != nil {
		t.Fatalf("selectS.Tick 1: %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateDownloading {
		t.Fatalf("expected DOWNLOADING, got %v", got)
	}
	cand1, found, err := lm.st.ActiveCandidate(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if cand1.Username != "peer1" {
		t.Fatalf("expected peer1 (higher score) activated first, got %q", cand1.Username)
	}

	// Candidate 1's transfer errors terminally -> back to SELECTING, candidate 1 FAILED.
	failTransfersTerminally(t, lm, "peer1", []core.CandidateFile{{Filename: "peer1/01.flac", Size: 10}}, now)
	if got := jobStateFor(t, lm.st, jobID); got != core.StateSelecting {
		t.Fatalf("expected job back to SELECTING after candidate 1 failed, got %v", got)
	}
	if _, found, err := lm.st.ActiveCandidate(ctx, jobID); err != nil || found {
		t.Fatalf("expected no active candidate after failure, found=%v (%v)", found, err)
	}

	// Activate candidate 2 (peer2, the only one left).
	if err := lm.selectS.Tick(ctx, now); err != nil {
		t.Fatalf("selectS.Tick 2: %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateDownloading {
		t.Fatalf("expected DOWNLOADING again after activating candidate 2, got %v", got)
	}
	cand2, found, err := lm.st.ActiveCandidate(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if cand2.Username != "peer2" {
		t.Fatalf("expected peer2 (the remaining candidate) activated, got %q", cand2.Username)
	}
	if cand2.ID == cand1.ID {
		t.Fatalf("expected a different candidate activated on rotation")
	}

	// Candidate 2's transfer completes -> IMPORTING -> DONE.
	files2 := []core.CandidateFile{{Filename: "peer2/01.flac", Size: 10}}
	completeTransfers(t, lm, cand2.ID, "peer2", files2, now)
	if got := jobStateFor(t, lm.st, jobID); got != core.StateImporting {
		t.Fatalf("expected IMPORTING after candidate 2's transfers complete, got %v", got)
	}

	if err := lm.importM.Tick(ctx, now); err != nil {
		t.Fatalf("importM.Tick (verify): %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateImporting {
		t.Fatalf("expected job still IMPORTING awaiting confirmation, got %v", got)
	}

	music.albumPresent = 1
	now2 := now.Add(time.Minute)
	if err := lm.importM.Tick(ctx, now2); err != nil {
		t.Fatalf("importM.Tick (confirm): %v", err)
	}
	if got := jobStateFor(t, lm.st, jobID); got != core.StateDone {
		t.Fatalf("expected DONE after candidate 2's import confirmed, got %v", got)
	}
}

// TestFullLifecycleExhaustionToFailedAndRevival drives Discovery through
// repeated empty searches until the job's search-cycle retry budget is
// exhausted (FAILED, failed_at set), then advances the clock past
// FailedReviveAfter and confirms WantedSync revives the still-wanted album
// back to WANTED with retries reset.
func TestFullLifecycleExhaustionToFailedAndRevival(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	const maxRetries = 2 // small budget to keep the test fast

	music := &fakeMusic{wanted: []core.WantedRelease{{ID: 1, Title: "Album", ArtistName: "Artist"}}}
	search := &fakeSearcher{} // no results ever, primary or fallback
	network := &fakeNetwork{}
	lm := newLifecycleModules(t, music, search, network, maxRetries)

	if err := lm.wanted.Tick(ctx, now); err != nil {
		t.Fatalf("wanted.Tick: %v", err)
	}
	wantedJobs, err := lm.st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil || len(wantedJobs) != 1 {
		t.Fatalf("RunnableJobsInState(WANTED): %+v (%v)", wantedJobs, err)
	}
	jobID := wantedJobs[0].ID

	// Drive Discovery with empty searches until the job goes FAILED, stepping
	// the clock past each backoff's not_before in between.
	cur := now
	var failedAt *time.Time
	for i := 0; i < maxRetries+1; i++ {
		if err := lm.discover.Tick(ctx, cur); err != nil {
			t.Fatalf("discover.Tick (iteration %d): %v", i, err)
		}
		failedJobs, err := lm.st.RunnableJobsInState(ctx, core.StateFailed, cur, 10)
		if err != nil {
			t.Fatalf("RunnableJobsInState(FAILED): %v", err)
		}
		if len(failedJobs) == 1 && failedJobs[0].ID == jobID {
			failedAt = failedJobs[0].FailedAt
			break
		}
		wj, err := lm.st.RunnableJobsInState(ctx, core.StateWanted, cur.Add(48*time.Hour), 10)
		if err != nil || len(wj) != 1 {
			t.Fatalf("expected job still WANTED after iteration %d, got %+v (%v)", i, wj, err)
		}
		if wj[0].NotBefore == nil {
			t.Fatalf("expected not_before set after a backed-off search failure (iteration %d)", i)
		}
		cur = wj[0].NotBefore.Add(time.Second)
	}
	if failedAt == nil {
		t.Fatalf("expected job FAILED within %d discovery ticks, still not failed", maxRetries+1)
	}

	// Advance well past FailedReviveAfter (30 days) and re-sync: the album is
	// still wanted, so the job should be revived to WANTED with retries reset.
	reviveNow := failedAt.Add(31 * 24 * time.Hour)
	if err := lm.wanted.Tick(ctx, reviveNow); err != nil {
		t.Fatalf("wanted.Tick (revival): %v", err)
	}
	revived, err := lm.st.RunnableJobsInState(ctx, core.StateWanted, reviveNow, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState(WANTED, revival): %v", err)
	}
	var found bool
	for _, j := range revived {
		if j.ID == jobID {
			found = true
			if j.Retries != 0 {
				t.Errorf("expected revived job's retries reset to 0, got %d", j.Retries)
			}
		}
	}
	if !found {
		t.Fatalf("expected job %d revived to WANTED, got %+v", jobID, revived)
	}
}
