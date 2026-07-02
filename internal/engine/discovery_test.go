package engine

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// --- fakes ---

type fakeMusic struct {
	wanted         []lidarr.WantedAlbum
	candidates     []lidarr.ManualImportItem
	imported       [][]lidarr.ManualImportItem
	albumPresent   int
	albumTotal     int
	albumStatusErr error
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error) {
	return f.wanted, nil
}
func (f *fakeMusic) ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error) {
	return f.candidates, nil
}
func (f *fakeMusic) ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error {
	f.imported = append(f.imported, items)
	return nil
}
func (f *fakeMusic) AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error) {
	return f.albumPresent, f.albumTotal, f.albumStatusErr
}

type fakeSearcher struct {
	results    []slskd.Result
	enqueued   []string
	enqueueErr error
}

func (f *fakeSearcher) Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error) {
	return f.results, nil
}
func (f *fakeSearcher) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	if f.enqueueErr != nil {
		return "", f.enqueueErr
	}
	f.enqueued = append(f.enqueued, filename)
	return "slskd-" + filename, nil
}

// discoBackedStore is a real store-backed DiscoveryStore for these tests
// (simpler and more faithful than a hand-written fake, since the interface is wide).
type discoBackedStore struct {
	*store.Store
}

// newBackedStore opens a fresh store in a temp dir, closed automatically.
func newBackedStore(t *testing.T) *discoBackedStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &discoBackedStore{Store: st}
}

// seedVerifyingJob creates a DISCOVERED job for album 1, a candidate attempt, and
// a single COMPLETED transfer, then advances the job straight to VERIFYING - the
// state advanceImporting expects to find work in.
func (b *discoBackedStore) seedVerifyingJob(t *testing.T, now time.Time) {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	attemptID, err := b.CreateAttempt(ctx, job.ID, "bob", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	deadline := now.Add(30 * time.Minute)
	transferID, err := b.RecordEnqueueIntent(ctx, attemptID, "bob", `A\01.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := b.AttachTransferID(ctx, transferID, "slskd-1", now); err != nil {
		t.Fatalf("AttachTransferID: %v", err)
	}
	if err := b.UpdateTransferProgress(ctx, transferID, core.TransferCompleted, 100, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	if err := b.AdvanceJobState(ctx, job.ID, core.StateVerifying, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
}

// seedImportingJob creates a DISCOVERED job for album 1, a candidate attempt,
// and advances the job straight to IMPORTING at time at - the state
// confirmImports expects to find work in.
func (b *discoBackedStore) seedImportingJob(t *testing.T, at time.Time) {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, 1, at)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	attemptID, err := b.CreateAttempt(ctx, job.ID, "bob", 1.0, at)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	deadline := at.Add(30 * time.Minute)
	if _, err := b.RecordEnqueueIntent(ctx, attemptID, "bob", `A\01.flac`, deadline, at); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := b.AdvanceJobState(ctx, job.ID, core.StateImporting, at); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
}

// seedDownloadingJobWithFailedTransfer creates a DISCOVERED job for album 2, moves
// it to DOWNLOADING, creates a candidate attempt, and records a transfer that is
// then marked ERRORED - the state advanceDownloading's anyFailed branch expects.
func (b *discoBackedStore) seedDownloadingJobWithFailedTransfer(t *testing.T, now time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, 2, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if err := b.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	attemptID, err := b.CreateAttempt(ctx, job.ID, "bob", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	deadline := now.Add(30 * time.Minute)
	transferID, err := b.RecordEnqueueIntent(ctx, attemptID, "bob", `A\01.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := b.UpdateTransferProgress(ctx, transferID, core.TransferErrored, 0, 0, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	return job.ID
}

// seedFailedJob creates a DISCOVERED job for albumID and advances it straight to
// FAILED at time at, as if its candidate budget had already been exhausted.
func (b *discoBackedStore) seedFailedJob(t *testing.T, albumID int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, albumID, at)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if err := b.AdvanceJobState(ctx, job.ID, core.StateFailed, at); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
}

// defaultWeights returns representative matcher weights for tests.
func defaultWeights() config.Weights {
	return config.Weights{Format: 1, Bitrate: 1, Reliability: 1, FileCount: 1}
}

// testWriter adapts an io.Writer to *testing.T.Log, so slog output shows up
// inline with test failures instead of being lost.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// discoStore is a real store-backed DiscoveryStore for these tests (simpler than a fake).
func newDiscoParams(t *testing.T, music *fakeMusic, peers *fakeSearcher) (DiscovererParams, *discoBackedStore) {
	t.Helper()
	st := newBackedStore(t) // helper returning a *store.Store wrapper; see Step 3 note
	return DiscovererParams{
		Music: music, Peers: peers, Store: st, Ranker: matcher.NewWeighted(defaultWeights(), 192),
		CompleteDir: "/music/slskd-downloads", SearchTimeout: time.Second,
		TransferDeadline: 30 * time.Minute, CandidateBackoff: 10 * time.Minute,
		FailedCandidateBackoff: 30 * time.Second,
		FailedRetryAfter:       24 * time.Hour, ImportConfirmTimeout: 3 * time.Minute,
		MaxCandidates: 3, Batch: 10, MaxActive: 10,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}, st
}

func TestDiscoverStartsSearchAndEnqueues(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X", TrackCount: 2}}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\02.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.enqueued) != 2 {
		t.Fatalf("expected 2 files enqueued, got %d", len(peers.enqueued))
	}
	jobs, _ := st.JobsInState(context.Background(), core.StateDownloading, 10)
	if len(jobs) != 1 {
		t.Errorf("expected job in DOWNLOADING, got %d", len(jobs))
	}
}

func TestDiscoverImportsCleanCandidate(t *testing.T) {
	music := &fakeMusic{candidates: []lidarr.ManualImportItem{
		{ID: 1, Path: "/music/slskd-downloads/A/01.flac", Importable: true},
	}}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Seed a job in VERIFYING with a completed transfer.
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 1 {
		t.Fatalf("expected one ExecuteManualImport call, got %d", len(music.imported))
	}
	jobs, _ := st.JobsInState(ctx, core.StateCompleted, 10)
	if len(jobs) != 1 {
		t.Errorf("expected job COMPLETED after clean import, got %d", len(jobs))
	}
}

func TestDiscoverRejectedImportFailsCandidate(t *testing.T) {
	music := &fakeMusic{candidates: []lidarr.ManualImportItem{
		{ID: 1, Path: "/music/slskd-downloads/A/01.mp3", Rejections: []string{"Quality not in profile"}, Importable: false},
	}}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 0 {
		t.Errorf("must not import a rejected candidate")
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 {
		t.Errorf("rejected import should put the job in COOLDOWN, got %d", len(jobs))
	}
}

func TestDiscoverEmptyFolderCompletes(t *testing.T) {
	music := &fakeMusic{candidates: nil} // empty -> already imported
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 0 {
		t.Errorf("must not call ExecuteManualImport on an empty folder")
	}
	jobs, _ := st.JobsInState(ctx, core.StateCompleted, 10)
	if len(jobs) != 1 {
		t.Errorf("empty folder on a verifying job should complete it, got %d completed", len(jobs))
	}
}

func TestDiscoverIncompleteDownloadFailsCandidate(t *testing.T) {
	music := &fakeMusic{
		candidates: []lidarr.ManualImportItem{
			{ID: 1, Path: "/music/slskd-downloads/A/01.flac", Importable: true, TrackIDs: []int64{101}},
		},
		albumTotal: 12,
	}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 0 {
		t.Errorf("must not import a download that can't complete the release")
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 {
		t.Errorf("incomplete download should put the job in COOLDOWN, got %d", len(jobs))
	}
}

func TestDiscoverFullCoverageMovesToImportingNotYetConfirmed(t *testing.T) {
	music := &fakeMusic{
		candidates: []lidarr.ManualImportItem{
			{ID: 1, Path: "/music/slskd-downloads/A/01.flac", Importable: true, TrackIDs: []int64{101, 102}},
		},
		albumTotal:   2,
		albumPresent: 0, // Lidarr hasn't confirmed the async import yet
	}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 1 {
		t.Fatalf("expected one ExecuteManualImport call, got %d", len(music.imported))
	}
	jobs, _ := st.JobsInState(ctx, core.StateImporting, 10)
	if len(jobs) != 1 {
		t.Errorf("job with full coverage but unconfirmed import should be IMPORTING, got %d", len(jobs))
	}
	completed, _ := st.JobsInState(ctx, core.StateCompleted, 10)
	if len(completed) != 0 {
		t.Errorf("must not complete before Lidarr confirms the import, got %d completed", len(completed))
	}
}

func TestConfirmImportsCompletesWhenAlbumIsFull(t *testing.T) {
	music := &fakeMusic{albumPresent: 12, albumTotal: 12}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedImportingJob(t, now.Add(-time.Minute))
	d := NewDiscoverer(p)
	if err := d.confirmImports(ctx, now); err != nil {
		t.Fatalf("confirmImports: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCompleted, 10)
	if len(jobs) != 1 {
		t.Errorf("album with present>=total should complete the job, got %d completed", len(jobs))
	}
}

func TestConfirmImportsTimesOutWhenStillIncomplete(t *testing.T) {
	music := &fakeMusic{albumPresent: 8, albumTotal: 12}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedImportingJob(t, now.Add(-p.ImportConfirmTimeout-time.Second))
	d := NewDiscoverer(p)
	if err := d.confirmImports(ctx, now); err != nil {
		t.Fatalf("confirmImports: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 {
		t.Errorf("import stuck incomplete past the timeout should cool down, got %d", len(jobs))
	}
}

func TestConfirmImportsWaitsWithinTimeout(t *testing.T) {
	music := &fakeMusic{albumPresent: 8, albumTotal: 12}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedImportingJob(t, now.Add(-time.Second)) // well within ImportConfirmTimeout
	d := NewDiscoverer(p)
	if err := d.confirmImports(ctx, now); err != nil {
		t.Fatalf("confirmImports: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateImporting, 10)
	if len(jobs) != 1 {
		t.Errorf("import still within the timeout should stay IMPORTING, got %d", len(jobs))
	}
}

func TestDiscoverFailedTransferCooldowns(t *testing.T) {
	p, st := newDiscoParams(t, &fakeMusic{}, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID := st.seedDownloadingJobWithFailedTransfer(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	found := false
	for _, j := range jobs {
		if j.ID == jobID {
			found = true
			// A failed candidate must use the SHORT backoff: there are usually
			// more untried candidates to try immediately, so we should not wait
			// the long "nothing new to try" backoff before the next attempt.
			if j.NextAttemptAt == nil {
				t.Fatalf("cooldown job missing next_attempt_at")
			}
			if got := j.NextAttemptAt.Sub(now); got != p.FailedCandidateBackoff {
				t.Errorf("failed candidate should cool down for the short backoff %v, got %v", p.FailedCandidateBackoff, got)
			}
		}
	}
	if !found {
		t.Errorf("a job with a failed transfer should move to COOLDOWN")
	}
}

func TestDiscoverExhaustedCandidatesFails(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 55, Title: "A", ArtistName: "X"}}}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := st.UpsertDiscoveredJob(ctx, 55, now)
	for i := 0; i < p.MaxCandidates; i++ {
		_ = st.IncrementCandidatesTried(ctx, job.ID, now)
	}
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateFailed, 10)
	if len(jobs) != 1 {
		t.Errorf("job past the candidate budget should be FAILED, got %d", len(jobs))
	}
}

func TestDiscoverAlbumNoLongerWantedCancels(t *testing.T) {
	// The job exists but its album is absent from Lidarr's wanted list, so
	// albumFor cannot resolve it. The job must be cancelled, not retried forever.
	music := &fakeMusic{wanted: nil}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := st.UpsertDiscoveredJob(ctx, 5151, now); err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	cancelled, _ := st.JobsInState(ctx, core.StateCancelled, 10)
	if len(cancelled) != 1 {
		t.Fatalf("job for an unwanted album should be CANCELLED, got %d", len(cancelled))
	}
	// It must not linger in DISCOVERED where the next tick would retry it.
	discovered, _ := st.JobsInState(ctx, core.StateDiscovered, 10)
	if len(discovered) != 0 {
		t.Errorf("cancelled job should not remain DISCOVERED, got %d", len(discovered))
	}
}

func TestDiscoverEnqueueFailureMarksTransferErrored(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 77, Title: "A", ArtistName: "X", TrackCount: 1}}}
	peers := &fakeSearcher{
		results:    []slskd.Result{{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true}},
		enqueueErr: errors.New("peer offline"),
	}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	d := NewDiscoverer(p)
	// Tick 1: search + enqueue-fail -> job DOWNLOADING with its transfer marked ERRORED
	// (synchronously, not left to wait out the deadline).
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce 1: %v", err)
	}
	// Tick 2: advanceDownloading sees the errored transfer -> COOLDOWN.
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) == 0 {
		t.Errorf("enqueue failure should mark the transfer errored and lead to COOLDOWN, got no cooldown jobs")
	}
}

func TestDiscoverNoCandidateIncrementsBudget(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 91, Title: "A", ArtistName: "X"}}}
	peers := &fakeSearcher{} // no results -> no candidate
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := st.UpsertDiscoveredJob(ctx, 91, now); err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 || jobs[0].CandidatesTried != 1 {
		t.Fatalf("exhausted-candidate tick should increment candidates_tried to 1 and cooldown, got %+v", jobs)
	}
	// No untried candidate is available, so this uses the LONG backoff: there is
	// nothing new to try, so re-searching soon would just waste search budget.
	if jobs[0].NextAttemptAt == nil {
		t.Fatalf("cooldown job missing next_attempt_at")
	}
	if got := jobs[0].NextAttemptAt.Sub(now); got != p.CandidateBackoff {
		t.Errorf("no-candidate cooldown should use the long backoff %v, got %v", p.CandidateBackoff, got)
	}
}

func TestDiscoverRespectsMaxActiveCeiling(t *testing.T) {
	// Three DISCOVERED jobs, each searchable, but MaxActive only allows 2 total
	// active jobs at once: only 2 should be started this tick.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{
		{ID: 501, Title: "A", ArtistName: "X"},
		{ID: 502, Title: "B", ArtistName: "X"},
		{ID: 503, Title: "C", ArtistName: "X"},
	}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	p.MaxActive = 2
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 2 {
		t.Fatalf("expected only 2 jobs started under MaxActive=2, got %d", len(downloading))
	}
	discovered, _ := st.JobsInState(ctx, core.StateDiscovered, 10)
	if len(discovered) != 1 {
		t.Errorf("expected 1 job left DISCOVERED (ceiling reached), got %d", len(discovered))
	}
}

func TestDiscoverNoNewJobsWhenAtCeiling(t *testing.T) {
	// One job already DOWNLOADING (active) with MaxActive=1: a second DISCOVERED
	// job must not be started this tick.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{
		{ID: 601, Title: "A", ArtistName: "X"},
		{ID: 602, Title: "B", ArtistName: "X"},
	}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	p.MaxActive = 1
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, err := st.UpsertDiscoveredJob(ctx, 601, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	discovered, _ := st.JobsInState(ctx, core.StateDiscovered, 10)
	if len(discovered) != 1 {
		t.Errorf("expected album 602 to remain DISCOVERED (ceiling already reached), got %d", len(discovered))
	}
}

func TestSyncWantedCachesTitleAndArtist(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{
		{ID: 900, Title: "Dummy", ArtistName: "Portishead", TrackCount: 11},
	}}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	d := NewDiscoverer(p)

	if err := d.syncWanted(ctx, music.wanted, now); err != nil {
		t.Fatalf("syncWanted: %v", err)
	}

	jobs, err := st.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Title != "Dummy" || jobs[0].ArtistName != "Portishead" {
		t.Errorf("title/artist = %q / %q, want Dummy / Portishead", jobs[0].Title, jobs[0].ArtistName)
	}
}

func TestSyncWantedBackfillsMetadataForNonDiscoveredJobWithoutResettingUpdatedAt(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{
		{ID: 901, Title: "Legacy Album", ArtistName: "Legacy Artist", TrackCount: 8},
	}}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	failedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedFailedJob(t, 901, failedAt) // pre-existing job: title/artist left empty, as prod jobs from before metadata caching are

	d := NewDiscoverer(p)
	later := failedAt.Add(time.Hour)
	if err := d.syncWanted(ctx, music.wanted, later); err != nil {
		t.Fatalf("syncWanted: %v", err)
	}

	failed, err := st.JobsInState(ctx, core.StateFailed, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("expected 1 FAILED job, got %d", len(failed))
	}
	job := failed[0]
	if job.Title != "Legacy Album" || job.ArtistName != "Legacy Artist" {
		t.Errorf("title/artist = %q / %q, want backfilled Legacy Album / Legacy Artist", job.Title, job.ArtistName)
	}
	if !job.UpdatedAt.Equal(failedAt) {
		t.Errorf("updated_at = %v, want unchanged %v (must not reset the retry-cooldown clock)", job.UpdatedAt, failedAt)
	}
}

func TestDiscoverRetriesFailedAlbumAfterWindow(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 88, Title: "A", ArtistName: "X", TrackCount: 1}}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	failedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedFailedJob(t, 88, failedAt)
	now := failedAt.Add(p.FailedRetryAfter + time.Hour)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	failed, _ := st.JobsInState(ctx, core.StateFailed, 10)
	if len(failed) != 0 {
		t.Errorf("failed album past the retry window should have been retried, still FAILED")
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("retried album with a good candidate should reach DOWNLOADING, got %d", len(downloading))
	}
}
