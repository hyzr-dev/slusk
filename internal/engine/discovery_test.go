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
	wanted     []lidarr.WantedAlbum
	candidates []lidarr.ManualImportItem
	imported   [][]lidarr.ManualImportItem
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

type fakeSearcher struct {
	results    []slskd.Result
	enqueued   []string
	enqueueErr error
}

func (f *fakeSearcher) Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error) {
	return f.results, nil
}
func (f *fakeSearcher) Enqueue(ctx context.Context, username, filename string) (string, error) {
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
		FailedRetryAfter: 24 * time.Hour, MaxCandidates: 3, Batch: 10,
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
