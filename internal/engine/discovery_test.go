package engine

import (
	"context"
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
	results  []slskd.Result
	enqueued []string
}

func (f *fakeSearcher) Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error) {
	return f.results, nil
}
func (f *fakeSearcher) Enqueue(ctx context.Context, username, filename string) (string, error) {
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
