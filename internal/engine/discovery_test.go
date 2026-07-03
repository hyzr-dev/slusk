package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	// candidatesErrForFolder and albumStatusErrForAlbum let tests inject a
	// per-job error (keyed by the folder ManualImportCandidates was asked
	// about, or the Lidarr album ID AlbumStatus was asked about) so a batch
	// with multiple jobs can have exactly one job fail while the rest succeed.
	candidatesErrForFolder map[string]error
	albumStatusErrForAlbum map[int64]error
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error) {
	return f.wanted, nil
}
func (f *fakeMusic) ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error) {
	if err, ok := f.candidatesErrForFolder[folder]; ok {
		return nil, err
	}
	return f.candidates, nil
}
func (f *fakeMusic) ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error {
	f.imported = append(f.imported, items)
	return nil
}
func (f *fakeMusic) AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error) {
	if err, ok := f.albumStatusErrForAlbum[albumID]; ok {
		return 0, 0, err
	}
	return f.albumPresent, f.albumTotal, f.albumStatusErr
}

type fakeSearcher struct {
	results        []slskd.Result
	enqueued       []string
	enqueuedSizes  map[string]int64
	enqueueErr     error
	deletedFolders []string
	deleteErr      error
	// enqueueErrForFile, if set, fails Enqueue only for the named file (once
	// per entry, decremented on each failing call) while other files succeed
	// normally. Lets tests reproduce a single transient enqueue failure amid
	// otherwise-successful calls.
	enqueueErrForFile map[string]int

	// queries records every query Search was called with, in order, so tests
	// can assert how many searches were issued and what they were.
	queries []string
	// resultsForQuery, when set, overrides results on a per-query basis
	// (falling back to results for queries not present in the map). Used to
	// give the primary and normalized fallback query different results.
	resultsForQuery map[string][]slskd.Result
	// searchErrForQuery injects a Search error for a specific query, so tests
	// can fail e.g. only the fallback search while the primary succeeds.
	searchErrForQuery map[string]error

	cancelled []string // slskd ids passed to Cancel
	cancelErr error
}

func (f *fakeSearcher) Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error) {
	f.queries = append(f.queries, query)
	if err, ok := f.searchErrForQuery[query]; ok {
		return nil, err
	}
	if r, ok := f.resultsForQuery[query]; ok {
		return r, nil
	}
	return f.results, nil
}
func (f *fakeSearcher) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	if f.enqueueErr != nil {
		return "", f.enqueueErr
	}
	if f.enqueueErrForFile[filename] > 0 {
		f.enqueueErrForFile[filename]--
		return "", errors.New("transient enqueue failure")
	}
	f.enqueued = append(f.enqueued, filename)
	if f.enqueuedSizes == nil {
		f.enqueuedSizes = map[string]int64{}
	}
	f.enqueuedSizes[filename] = size
	return "slskd-" + filename, nil
}
func (f *fakeSearcher) Cancel(ctx context.Context, username, id string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}
func (f *fakeSearcher) DeleteDownloadFolder(ctx context.Context, name string) error {
	f.deletedFolders = append(f.deletedFolders, name)
	return f.deleteErr
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

// seedVerifyingJobForAlbum is seedVerifyingJob generalized to an arbitrary
// Lidarr album ID and updated_at, with a folder leaf unique to the album
// (`Alb<albumID>`), so tests can seed multiple VERIFYING jobs in one batch
// with distinct ManualImportCandidates folders. It returns the job ID.
func (b *discoBackedStore) seedVerifyingJobForAlbum(t *testing.T, albumID int64, updatedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, albumID, updatedAt)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	attemptID, err := b.CreateAttempt(ctx, job.ID, "bob", 1.0, updatedAt)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	deadline := updatedAt.Add(30 * time.Minute)
	filename := fmt.Sprintf(`Alb%d\01.flac`, albumID)
	transferID, err := b.RecordEnqueueIntent(ctx, attemptID, "bob", filename, deadline, updatedAt)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := b.AttachTransferID(ctx, transferID, fmt.Sprintf("slskd-%d", albumID), updatedAt); err != nil {
		t.Fatalf("AttachTransferID: %v", err)
	}
	if err := b.UpdateTransferProgress(ctx, transferID, core.TransferCompleted, 100, 100, updatedAt); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	if err := b.AdvanceJobState(ctx, job.ID, core.StateVerifying, updatedAt); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	return job.ID
}

// seedImportingJob creates a DISCOVERED job for album 1, a candidate attempt,
// and advances the job straight to IMPORTING at time at - the state
// confirmImports expects to find work in.
func (b *discoBackedStore) seedImportingJob(t *testing.T, at time.Time) {
	t.Helper()
	b.seedImportingJobForAlbum(t, 1, at)
}

// seedImportingJobForAlbum is seedImportingJob generalized to an arbitrary
// Lidarr album ID, so tests can seed multiple IMPORTING jobs in one batch.
func (b *discoBackedStore) seedImportingJobForAlbum(t *testing.T, albumID int64, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, albumID, at)
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
	return job.ID
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

// seedDownloadingJobWithFailedAndPendingSibling creates a DISCOVERED job for
// album 3, moves it to DOWNLOADING, and gives its one attempt two transfers in
// the same remote directory ("A"): one already ERRORED, and one still PENDING
// (never sent to slskd) - the state topUpAttempt would otherwise release.
func (b *discoBackedStore) seedDownloadingJobWithFailedAndPendingSibling(t *testing.T, now time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, 3, now)
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
	erroredID, err := b.RecordEnqueueIntent(ctx, attemptID, "bob", `A\01.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := b.UpdateTransferProgress(ctx, erroredID, core.TransferErrored, 0, 0, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	if err := b.RecordPendingTransfer(ctx, attemptID, "bob", `A\02.flac`, 10, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}
	return job.ID
}

// seedDownloadingJobWithFailedActiveAndPendingSiblings creates a DISCOVERED job
// for album 4, moves it to DOWNLOADING, and gives its one attempt three transfers
// in the same remote directory ("A"): one already ERRORED, one still IN_PROGRESS
// in slskd (with a slskd id, so it must be cancelled there), and one PENDING
// (never sent). This is the mixed state advanceDownloading's two-phase fail path
// must handle: cancel the active sibling, cancel the pending sibling in the DB,
// and defer cleanup. It returns the job ID and the IN_PROGRESS transfer's DB id
// (so a test can later drive it terminal).
func (b *discoBackedStore) seedDownloadingJobWithFailedActiveAndPendingSiblings(t *testing.T, now time.Time) (jobID, inProgressID int64) {
	t.Helper()
	ctx := context.Background()
	job, err := b.UpsertDiscoveredJob(ctx, 4, now)
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
	erroredID, err := b.RecordEnqueueIntent(ctx, attemptID, "bob", `A\01.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent errored: %v", err)
	}
	if err := b.UpdateTransferProgress(ctx, erroredID, core.TransferErrored, 0, 0, now); err != nil {
		t.Fatalf("UpdateTransferProgress errored: %v", err)
	}
	inProgressID, err = b.RecordEnqueueIntent(ctx, attemptID, "bob", `A\02.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent in-progress: %v", err)
	}
	if err := b.AttachTransferID(ctx, inProgressID, "slskd-inprog", now); err != nil {
		t.Fatalf("AttachTransferID: %v", err)
	}
	if err := b.UpdateTransferProgress(ctx, inProgressID, core.TransferInProgress, 50, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress in-progress: %v", err)
	}
	if err := b.RecordPendingTransfer(ctx, attemptID, "bob", `A\03.flac`, 10, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}
	return job.ID, inProgressID
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

// jobState scans every pipeline state to find jobID's current state, since
// DiscoveryStore has no direct get-by-ID lookup.
func jobState(t *testing.T, st *discoBackedStore, jobID int64) core.AlbumJobState {
	t.Helper()
	ctx := context.Background()
	all := []core.AlbumJobState{
		core.StateDiscovered, core.StateSearching, core.StateSelecting,
		core.StateDownloading, core.StateVerifying, core.StateImporting,
		core.StateCompleted, core.StateCooldown, core.StateFailed, core.StateCancelled,
	}
	for _, state := range all {
		jobs, err := st.JobsInState(ctx, state, 100)
		if err != nil {
			t.Fatalf("JobsInState(%v): %v", state, err)
		}
		for _, j := range jobs {
			if j.ID == jobID {
				return state
			}
		}
	}
	t.Fatalf("job %d not found in any known state", jobID)
	return ""
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
		MaxCandidates: 3, Batch: 10, MaxActive: 10, MaxInflightPerPeer: 3,
		MaxCandidateFileRatio: 2,
		Logger:                slog.New(slog.NewTextHandler(testWriter{t}, nil)),
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

func TestStartJobRejectsGrosslyOversizedCandidate(t *testing.T) {
	// Reproduces a live incident: a Soulseek user shared a flat, non-album
	// folder containing an entire artist's discography (146 files: singles,
	// remixes, and collaborations with other artists) for an album Lidarr
	// expects to have far fewer tracks. Grouped by (username, dir), matcher.Rank
	// treats this as one candidate; startJob must reject it as implausible
	// rather than downloading the whole 146-file dump.
	files := make([]slskd.Result, 0, 146)
	for i := 0; i < 146; i++ {
		files = append(files, slskd.Result{
			Username: "mayamaya", Filename: fmt.Sprintf(`mayamaya\ILLENIUM\%03d track.flac`, i),
			BitRate: 900, HasFreeUploadSlot: true,
		})
	}
	music := &fakeMusic{
		wanted:     []lidarr.WantedAlbum{{ID: 1, Title: "ILLENIUM", ArtistName: "ILLENIUM"}},
		albumTotal: 12, // Lidarr's known track count for the actual album
	}
	peers := &fakeSearcher{results: files}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.enqueued) != 0 {
		t.Fatalf("oversized candidate must not be enqueued, got %d files enqueued", len(peers.enqueued))
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 0 {
		t.Errorf("job must not reach DOWNLOADING off an oversized candidate, got %d", len(downloading))
	}
	cooldown, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(cooldown) != 1 {
		t.Errorf("job should cool down when the only candidate is grossly oversized, got %d", len(cooldown))
	}
}

func TestStartJobAcceptsNormalSizedCandidateWithAlbumTotalKnown(t *testing.T) {
	// A candidate whose file count is plausible for the album must still
	// proceed normally, even once Lidarr's expected track count is known and
	// checked.
	music := &fakeMusic{
		wanted:     []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X"}},
		albumTotal: 2,
	}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\02.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.enqueued) != 2 {
		t.Fatalf("expected 2 files enqueued, got %d", len(peers.enqueued))
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("normal-sized candidate should reach DOWNLOADING, got %d", len(downloading))
	}
}

func TestStartJobDoesNotRejectOversizedWhenAlbumTotalUnknown(t *testing.T) {
	// When Lidarr's track count for the album is unknown (AlbumStatus returns
	// total == 0, e.g. transient Lidarr data gap), the size sanity check must
	// not reject candidates outright -- Lidarr is the final arbiter of import
	// correctness downstream, so an unreliable total must not block otherwise
	// good candidates.
	files := make([]slskd.Result, 0, 50)
	for i := 0; i < 50; i++ {
		files = append(files, slskd.Result{
			Username: "bob", Filename: fmt.Sprintf(`bob\A\%03d.flac`, i), BitRate: 900, HasFreeUploadSlot: true,
		})
	}
	music := &fakeMusic{
		wanted:     []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X"}},
		albumTotal: 0,
	}
	peers := &fakeSearcher{results: files}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("unknown album total should not block a candidate, got %d DOWNLOADING", len(downloading))
	}
}

func TestStartJobSkipsUndercompleteCandidateAndPicksNextOne(t *testing.T) {
	// A candidate with fewer files than the album's expected track count is
	// guaranteed to be rejected by the VERIFYING completeness gate later, so
	// startJob must skip it outright and fall through to the next candidate
	// (from a different user) that can actually cover the album.
	music := &fakeMusic{
		wanted:     []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X"}},
		albumTotal: 3,
	}
	peers := &fakeSearcher{results: []slskd.Result{
		// alice only shares 1 of the 3 expected tracks - undercomplete.
		{Username: "alice", Filename: `alice\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
		// bob shares all 3 expected tracks - a viable candidate.
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\02.flac`, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\03.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, f := range peers.enqueued {
		if strings.HasPrefix(f, `alice\`) {
			t.Errorf("undercomplete candidate's file %q must not be enqueued", f)
		}
	}
	if len(peers.enqueued) != 3 {
		t.Fatalf("expected the 3-file bob candidate to be enqueued, got %d files enqueued: %v", len(peers.enqueued), peers.enqueued)
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("job should reach DOWNLOADING off the complete candidate, got %d", len(downloading))
	}
}

func TestStartJobAcceptsUndercompleteCandidateWhenAlbumTotalUnknown(t *testing.T) {
	// When Lidarr's track count for the album is unknown (total == 0), the
	// under-complete check must be skipped entirely, same as the ratio check -
	// an unreliable total must never block an otherwise viable candidate.
	music := &fakeMusic{
		wanted:     []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X"}},
		albumTotal: 0,
	}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.enqueued) != 1 {
		t.Fatalf("expected 1 file enqueued, got %d", len(peers.enqueued))
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("unknown album total should not block an undercomplete candidate, got %d DOWNLOADING", len(downloading))
	}
}

func TestStartJobAllUndercompleteCandidatesCoolsDown(t *testing.T) {
	// If every candidate is undercomplete for the album, none may be enqueued;
	// the job must fall through to the existing "no untried candidate
	// available" path, cooling down with candidates_tried incremented by
	// exactly 1 (not once per skipped candidate).
	music := &fakeMusic{
		wanted:     []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X"}},
		albumTotal: 3,
	}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "alice", Filename: `alice\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.enqueued) != 0 {
		t.Fatalf("no candidate should be enqueued when all are undercomplete, got %d", len(peers.enqueued))
	}
	cooldown, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(cooldown) != 1 || cooldown[0].CandidatesTried != 1 {
		t.Fatalf("job should cool down with candidates_tried == 1, got %+v", cooldown)
	}
}

// transferStateCounts sums the transfer states across every attempt of the
// DOWNLOADING job for lidarrAlbumID, so tests can assert how many files were
// sent to slskd versus deferred as PENDING.
func transferStateCounts(t *testing.T, st *discoBackedStore, lidarrAlbumID int64) map[core.TransferState]int {
	t.Helper()
	ctx := context.Background()
	counts := map[core.TransferState]int{}
	for _, state := range []core.AlbumJobState{core.StateDownloading, core.StateVerifying, core.StateImporting, core.StateCompleted} {
		jobs, _ := st.JobsInState(ctx, state, 100)
		for _, job := range jobs {
			if job.LidarrAlbumID != lidarrAlbumID {
				continue
			}
			attempts, _ := st.AttemptsForJob(ctx, job.ID)
			for _, a := range attempts {
				transfers, _ := st.TransfersForAttempt(ctx, a.ID)
				for _, tr := range transfers {
					counts[tr.State]++
				}
			}
		}
	}
	return counts
}

func TestStartJobThrottlesEnqueuePerPeer(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X", TrackCount: 4}}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\02.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\03.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\04.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	p.MaxInflightPerPeer = 2
	d := NewDiscoverer(p)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Only the cap is sent to slskd immediately; the rest wait as PENDING so the
	// peer's per-user queued-megabyte limit is not tripped by a burst.
	if len(peers.enqueued) != 2 {
		t.Fatalf("expected 2 files enqueued (cap), got %d", len(peers.enqueued))
	}
	counts := transferStateCounts(t, st, 1)
	if counts[core.TransferPending] != 2 {
		t.Errorf("expected 2 PENDING transfers, got %d", counts[core.TransferPending])
	}
	if counts[core.TransferQueued] != 2 {
		t.Errorf("expected 2 sent (QUEUED) transfers, got %d", counts[core.TransferQueued])
	}
}

// completeInflightTransfers marks every sent-but-unfinished transfer of the
// DOWNLOADING job for lidarrAlbumID as COMPLETED, simulating slskd finishing the
// current batch so the next tick can release more PENDING files.
func completeInflightTransfers(t *testing.T, st *discoBackedStore, lidarrAlbumID int64, now time.Time) {
	t.Helper()
	ctx := context.Background()
	jobs, _ := st.JobsInState(ctx, core.StateDownloading, 100)
	for _, job := range jobs {
		if job.LidarrAlbumID != lidarrAlbumID {
			continue
		}
		attempts, _ := st.AttemptsForJob(ctx, job.ID)
		for _, a := range attempts {
			transfers, _ := st.TransfersForAttempt(ctx, a.ID)
			for _, tr := range transfers {
				if tr.State == core.TransferQueued || tr.State == core.TransferInProgress {
					_ = st.UpdateTransferProgress(ctx, tr.ID, core.TransferCompleted, tr.BytesTotal, tr.BytesTotal, now)
				}
			}
		}
	}
}

func TestTopUpDownloadsReleasesPendingAsInflightCompletes(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X", TrackCount: 4}}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\02.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\03.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\04.flac`, Size: 10, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	p.MaxInflightPerPeer = 2
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if len(peers.enqueued) != 2 {
		t.Fatalf("first tick should send only the cap, got %d", len(peers.enqueued))
	}
	// The two in-flight downloads finish; the next tick must release the rest.
	completeInflightTransfers(t, st, 1, now)
	if err := d.RunOnce(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(peers.enqueued) != 4 {
		t.Fatalf("second tick should release the remaining files, got %d enqueued", len(peers.enqueued))
	}
	if counts := transferStateCounts(t, st, 1); counts[core.TransferPending] != 0 {
		t.Errorf("no PENDING transfers should remain, got %d", counts[core.TransferPending])
	}
	// A deferred file must be enqueued with the size persisted at PENDING time,
	// not 0 - slskd rejects size-0 downloads (they time out immediately).
	if got := peers.enqueuedSizes[`bob\A\04.flac`]; got != 10 {
		t.Errorf("deferred file should be enqueued with its persisted size 10, got %d", got)
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

func TestDiscoverFailedTransferDeletesDownloadedFolder(t *testing.T) {
	// The failed transfer's filename `A\01.flac` shares one common remote
	// directory ("A"), so the leaf is unambiguous: the leftover files that
	// attempt already deposited in slskd's completeDir must be purged before
	// the next candidate is tried, or they'd get mixed into a same-named
	// folder from a different peer.
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedDownloadingJobWithFailedTransfer(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected DeleteDownloadFolder(\"A\"), got %+v", peers.deletedFolders)
	}
}

func TestCleanupLogsNotFoundQuietly(t *testing.T) {
	// A 404 from DeleteDownloadFolder means the attempt never wrote any bytes
	// to disk (e.g. it failed before any transfer started) - a routine outcome
	// for a best-effort cleanup, not a real failure worth an ERROR log line.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	p, st := newDiscoParams(t, &fakeMusic{}, nil)
	p.Peers = slskd.New(srv.URL, "k")
	p.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedDownloadingJobWithFailedTransfer(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if strings.Contains(logBuf.String(), "level=ERROR") {
		t.Errorf("a 404 (nothing to clean up) must not be logged as ERROR, got:\n%s", logBuf.String())
	}
}

func TestCleanupLogsOtherFailuresAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	p, st := newDiscoParams(t, &fakeMusic{}, nil)
	p.Peers = slskd.New(srv.URL, "k")
	p.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedDownloadingJobWithFailedTransfer(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") {
		t.Errorf("a real cleanup failure (5xx) must still be logged as ERROR, got:\n%s", logBuf.String())
	}
}

func TestAdvanceDoesNotTopUpAttemptAlreadyFailed(t *testing.T) {
	// Reproduces a live race: topUpDownloads used to run before
	// advanceDownloading in the same tick, so an attempt with one already-
	// ERRORED transfer (recorded by an earlier reconcile pass) got its
	// remaining PENDING sibling released to slskd in this tick, moments
	// before advanceDownloading's anyFailed branch cleaned up and deleted
	// the very folder that sibling was actively downloading into.
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedDownloadingJobWithFailedAndPendingSibling(t, now)
	d := NewDiscoverer(p)
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(peers.enqueued) != 0 {
		t.Errorf("an already-failed attempt's pending sibling must not be topped up, got enqueued %+v", peers.enqueued)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected the failed attempt's folder to be cleaned up, got %+v", peers.deletedFolders)
	}
}

// transferStates returns the states of a job's most recent attempt's transfers,
// keyed by filename, so tests can assert per-transfer outcomes.
func transferStates(t *testing.T, st *discoBackedStore, jobID int64) map[string]core.TransferState {
	t.Helper()
	ctx := context.Background()
	attempts, err := st.AttemptsForJob(ctx, jobID)
	if err != nil || len(attempts) == 0 {
		t.Fatalf("AttemptsForJob(%d): %v (n=%d)", jobID, err, len(attempts))
	}
	transfers, err := st.TransfersForAttempt(ctx, attempts[len(attempts)-1].ID)
	if err != nil {
		t.Fatalf("TransfersForAttempt: %v", err)
	}
	out := make(map[string]core.TransferState, len(transfers))
	for _, tr := range transfers {
		out[tr.Filename] = tr.State
	}
	return out
}

func TestAdvanceDownloadingCancelsActiveSiblingsBeforeCleanup(t *testing.T) {
	// A failed attempt whose siblings are still IN_PROGRESS/PENDING must NOT be
	// cleaned up yet: the live download would keep writing into the folder we
	// just deleted. Tick 1 cancels the active sibling in slskd and marks the
	// never-sent PENDING sibling CANCELLED, but defers cleanup/FailAttempt; the
	// job stays DOWNLOADING. Once the cancelled sibling goes terminal, tick 2
	// runs the real cleanup + FailAttempt + cooldown.
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, inProgressID := st.seedDownloadingJobWithFailedActiveAndPendingSiblings(t, now)
	d := NewDiscoverer(p)

	// Tick 1: cancel active + pending, no cleanup, job stays DOWNLOADING.
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance tick 1: %v", err)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "slskd-inprog" {
		t.Errorf("expected the IN_PROGRESS sibling cancelled in slskd, got %+v", peers.cancelled)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("cleanup must not run while a sibling is still active, got %+v", peers.deletedFolders)
	}
	if got := jobState(t, st, jobID); got != core.StateDownloading {
		t.Errorf("job should stay DOWNLOADING during cancellation, got %v", got)
	}
	states := transferStates(t, st, jobID)
	if states[`A\03.flac`] != core.TransferCancelled {
		t.Errorf("never-sent PENDING sibling should be CANCELLED, got %v", states[`A\03.flac`])
	}
	if states[`A\02.flac`] != core.TransferInProgress {
		t.Errorf("IN_PROGRESS sibling stays non-terminal until slskd/reconciler confirms, got %v", states[`A\02.flac`])
	}

	// The reconciler picks up slskd's cancellation and marks the sibling terminal.
	if err := st.UpdateTransferProgress(ctx, inProgressID, core.TransferCancelled, 50, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	// Tick 2: everything terminal -> cleanup + FailAttempt + COOLDOWN.
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance tick 2: %v", err)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected the failed attempt's folder cleaned up on tick 2, got %+v", peers.deletedFolders)
	}
	if got := jobState(t, st, jobID); got != core.StateCooldown {
		t.Errorf("job should be COOLDOWN after all siblings terminal, got %v", got)
	}
	cool, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	for _, j := range cool {
		if j.ID == jobID {
			if j.NextAttemptAt == nil || j.NextAttemptAt.Sub(now) != p.FailedCandidateBackoff {
				t.Errorf("failed candidate should use the short backoff %v, got %+v", p.FailedCandidateBackoff, j.NextAttemptAt)
			}
		}
	}
}

func TestAdvanceDownloadingAllTerminalFailsFirstTick(t *testing.T) {
	// An attempt whose transfers are all terminal (today's common case) has
	// nothing to cancel, so it fails on the first tick exactly as before.
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID := st.seedDownloadingJobWithFailedTransfer(t, now)
	d := NewDiscoverer(p)
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(peers.cancelled) != 0 {
		t.Errorf("an all-terminal attempt has nothing to cancel, got %+v", peers.cancelled)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("an all-terminal failed attempt should be cleaned up on the first tick, got %+v", peers.deletedFolders)
	}
	if got := jobState(t, st, jobID); got != core.StateCooldown {
		t.Errorf("an all-terminal failed attempt should COOLDOWN on the first tick, got %v", got)
	}
}

func TestAdvanceDownloadingCancelErrorRetriesNextTick(t *testing.T) {
	// If cancelling the active sibling in slskd fails, the transfer is left
	// active (not marked terminal) and cleanup does not run; a later tick retries
	// the cancel.
	peers := &fakeSearcher{cancelErr: errors.New("slskd down")}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, _ := st.seedDownloadingJobWithFailedActiveAndPendingSiblings(t, now)
	d := NewDiscoverer(p)

	// Tick 1: cancel fails -> nothing recorded cancelled, no cleanup, still DOWNLOADING.
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance tick 1: %v", err)
	}
	if len(peers.cancelled) != 0 {
		t.Errorf("a failed Cancel must not be recorded as cancelled, got %+v", peers.cancelled)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("cleanup must not run while the active sibling is uncancelled, got %+v", peers.deletedFolders)
	}
	if got := jobState(t, st, jobID); got != core.StateDownloading {
		t.Errorf("job should stay DOWNLOADING when the cancel failed, got %v", got)
	}
	if states := transferStates(t, st, jobID); states[`A\02.flac`] != core.TransferInProgress {
		t.Errorf("the active sibling must stay non-terminal after a failed cancel, got %v", states[`A\02.flac`])
	}

	// Tick 2: slskd recovers, the cancel now succeeds.
	peers.cancelErr = nil
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance tick 2: %v", err)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "slskd-inprog" {
		t.Errorf("the retried cancel should reach slskd, got %+v", peers.cancelled)
	}
}

func TestDiscoverRejectedImportDeletesDownloadedFolder(t *testing.T) {
	music := &fakeMusic{candidates: []lidarr.ManualImportItem{
		{ID: 1, Path: "/music/slskd-downloads/A/01.mp3", Rejections: []string{"Quality not in profile"}, Importable: false},
	}}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// seedVerifyingJob's transfer filename is `A\01.flac` -> leaf "A".
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected DeleteDownloadFolder(\"A\") on rejected import, got %+v", peers.deletedFolders)
	}
}

func TestDiscoverAmbiguousFolderSkipsDelete(t *testing.T) {
	// A failed attempt whose transfers don't share a single common directory is
	// ambiguous: deleting could be interpreted as the whole downloads root, so
	// no delete call must be made at all.
	music := &fakeMusic{candidates: []lidarr.ManualImportItem{
		{ID: 1, Path: "/music/slskd-downloads/a/1.mp3", Rejections: []string{"bad"}, Importable: false},
	}}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := st.UpsertDiscoveredJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	attemptID, err := st.CreateAttempt(ctx, job.ID, "bob", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	deadline := now.Add(30 * time.Minute)
	// Two transfers in different remote directories -> commonLeaf == "".
	t1, err := st.RecordEnqueueIntent(ctx, attemptID, "bob", `a\1.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := st.AttachTransferID(ctx, t1, "slskd-1", now); err != nil {
		t.Fatalf("AttachTransferID: %v", err)
	}
	if err := st.UpdateTransferProgress(ctx, t1, core.TransferCompleted, 100, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	t2, err := st.RecordEnqueueIntent(ctx, attemptID, "bob", `b\2.flac`, deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := st.AttachTransferID(ctx, t2, "slskd-2", now); err != nil {
		t.Fatalf("AttachTransferID: %v", err)
	}
	if err := st.UpdateTransferProgress(ctx, t2, core.TransferCompleted, 100, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateVerifying, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("ambiguous folder must not trigger DeleteDownloadFolder, got %+v", peers.deletedFolders)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 {
		t.Errorf("rejected import should still put the job in COOLDOWN, got %d", len(jobs))
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

// seedDownloadingAttempt creates a DOWNLOADING job with one CandidateAttempt
// and returns the attempt ID, so tests can drive topUpAttempt directly with
// full control over which files are PENDING and their retry counts.
func seedDownloadingAttempt(t *testing.T, st *discoBackedStore, lidarrAlbumID int64, now time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := st.UpsertDiscoveredJob(ctx, lidarrAlbumID, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	attemptID, err := st.CreateAttempt(ctx, job.ID, "bob", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	return attemptID
}

func TestTopUpAttemptEnqueueFailureWithRetriesLeftReturnsToPendingAndResends(t *testing.T) {
	// With retries remaining, a failed enqueue must return the transfer to
	// PENDING (not ERRORED) with its retry count bumped, and the next tick
	// (another topUpAttempt call) must attempt to resend it.
	filename := `bob\A\01.flac`
	peers := &fakeSearcher{enqueueErrForFile: map[string]int{filename: 1}} // fails once, then succeeds
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	p.MaxTransferRetries = 1
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	attemptID := seedDownloadingAttempt(t, st, 77, now)
	if err := st.RecordPendingTransfer(ctx, attemptID, "bob", filename, 100, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}
	d := NewDiscoverer(p)

	// Tick 1: enqueue fails -> transfer returned to PENDING with retries bumped.
	sent, err := d.topUpAttempt(ctx, attemptID, now)
	if err != nil {
		t.Fatalf("topUpAttempt 1: %v", err)
	}
	if sent != 0 {
		t.Fatalf("failed enqueue must not count as sent, got %d", sent)
	}
	transfers, err := st.TransfersForAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("TransfersForAttempt: %v", err)
	}
	if len(transfers) != 1 || transfers[0].State != core.TransferPending || transfers[0].Retries != 1 {
		t.Fatalf("expected 1 PENDING transfer with retries == 1, got %+v", transfers)
	}

	// Tick 2: the retried transfer is resent and now succeeds.
	sent, err = d.topUpAttempt(ctx, attemptID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("topUpAttempt 2: %v", err)
	}
	if sent != 1 {
		t.Fatalf("expected the retried transfer to be resent, got sent=%d", sent)
	}
	if len(peers.enqueued) != 1 || peers.enqueued[0] != filename {
		t.Fatalf("expected the retried transfer to be resent, got %+v", peers.enqueued)
	}
	transfers, err = st.TransfersForAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("TransfersForAttempt: %v", err)
	}
	if len(transfers) != 1 || transfers[0].State != core.TransferQueued {
		t.Fatalf("expected the resent transfer to be QUEUED, got %+v", transfers)
	}
}

func TestTopUpAttemptEnqueueFailureWithRetriesExhaustedMarksErrored(t *testing.T) {
	// Once the retry budget is exhausted, a failed enqueue must fall back to
	// today's behavior: mark the transfer terminally ERRORED.
	filename := `bob\A\01.flac`
	peers := &fakeSearcher{enqueueErr: errors.New("peer offline")}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	p.MaxTransferRetries = 0
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	attemptID := seedDownloadingAttempt(t, st, 77, now)
	if err := st.RecordPendingTransfer(ctx, attemptID, "bob", filename, 100, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}
	d := NewDiscoverer(p)
	sent, err := d.topUpAttempt(ctx, attemptID, now)
	if err != nil {
		t.Fatalf("topUpAttempt: %v", err)
	}
	if sent != 0 {
		t.Fatalf("expected 0 sent for an exhausted-retry enqueue failure, got %d", sent)
	}
	transfers, err := st.TransfersForAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("TransfersForAttempt: %v", err)
	}
	if len(transfers) != 1 || transfers[0].State != core.TransferErrored {
		t.Fatalf("expected the transfer to be ERRORED once retries are exhausted, got %+v", transfers)
	}
}

func TestTopUpAttemptEnqueueFailureForOneFileDoesNotBlockOthers(t *testing.T) {
	// Within a single topUpAttempt loop, a failed enqueue for one file must not
	// prevent other files from being sent.
	failing := `bob\A\01.flac`
	ok := `bob\A\02.flac`
	peers := &fakeSearcher{enqueueErrForFile: map[string]int{failing: 1}}
	p, st := newDiscoParams(t, &fakeMusic{}, peers)
	p.MaxTransferRetries = 1
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	attemptID := seedDownloadingAttempt(t, st, 77, now)
	if err := st.RecordPendingTransfer(ctx, attemptID, "bob", failing, 100, now); err != nil {
		t.Fatalf("RecordPendingTransfer (failing): %v", err)
	}
	if err := st.RecordPendingTransfer(ctx, attemptID, "bob", ok, 100, now); err != nil {
		t.Fatalf("RecordPendingTransfer (ok): %v", err)
	}
	d := NewDiscoverer(p)
	sent, err := d.topUpAttempt(ctx, attemptID, now)
	if err != nil {
		t.Fatalf("topUpAttempt: %v", err)
	}
	if sent != 1 {
		t.Fatalf("expected the succeeding file to still be sent despite the other's enqueue failure, got sent=%d", sent)
	}
	if len(peers.enqueued) != 1 || peers.enqueued[0] != ok {
		t.Fatalf("expected only the succeeding file to be enqueued, got %+v", peers.enqueued)
	}
	transfers, err := st.TransfersForAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("TransfersForAttempt: %v", err)
	}
	states := map[string]core.TransferState{}
	for _, tr := range transfers {
		states[tr.Filename] = tr.State
	}
	if states[ok] != core.TransferQueued {
		t.Errorf("expected %q to be QUEUED, got %+v", ok, states)
	}
	if states[failing] != core.TransferPending {
		t.Errorf("expected %q to be returned to PENDING for retry, got %+v", failing, states)
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

func TestStartJobFallsBackToNormalizedQueryWhenPrimaryEmpty(t *testing.T) {
	// "Album (Deluxe Edition)" mirrors a real Lidarr title suffix that peers
	// rarely share verbatim: the primary query gets zero raw results, so the
	// normalized fallback ("X Album") should be tried and its results used.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 1, Title: "Album (Deluxe Edition)", ArtistName: "X"}}}
	primaryQuery := "X Album (Deluxe Edition)"
	fallbackQuery := normalizeQuery(primaryQuery)
	peers := &fakeSearcher{
		resultsForQuery: map[string][]slskd.Result{
			fallbackQuery: {
				{Username: "bob", Filename: `bob\Album\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
			},
		},
	}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.queries) != 2 {
		t.Fatalf("expected primary + one fallback search, got queries=%v", peers.queries)
	}
	if peers.queries[0] != primaryQuery {
		t.Errorf("expected primary query %q first, got %q", primaryQuery, peers.queries[0])
	}
	if peers.queries[1] != fallbackQuery {
		t.Errorf("expected fallback query %q second, got %q", fallbackQuery, peers.queries[1])
	}
	if len(peers.enqueued) != 1 {
		t.Fatalf("expected fallback result to be enqueued, got %d files", len(peers.enqueued))
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("job should reach DOWNLOADING via the fallback query's candidate, got %d", len(downloading))
	}
}

func TestStartJobSearchesOnceWhenPrimaryHasResults(t *testing.T) {
	// Even though the title would normalize to a different string, the
	// primary query already returned results, so no fallback search should
	// be issued.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 1, Title: "Album (Deluxe Edition)", ArtistName: "X"}}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\Album\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.queries) != 1 {
		t.Fatalf("expected exactly one search when the primary query already has results, got queries=%v", peers.queries)
	}
	downloading, _ := st.JobsInState(ctx, core.StateDownloading, 10)
	if len(downloading) != 1 {
		t.Errorf("expected job in DOWNLOADING, got %d", len(downloading))
	}
}

func TestStartJobCooldownWhenPrimaryAndFallbackBothEmpty(t *testing.T) {
	// Both the primary and the normalized fallback query return zero results:
	// budget/cooldown semantics must be unchanged from today (a single
	// candidate-budget increment, long backoff), not doubled by the extra search.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 91, Title: "Album (Deluxe Edition)", ArtistName: "X"}}}
	peers := &fakeSearcher{} // both primary and fallback queries get zero results
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
	if len(peers.queries) != 2 {
		t.Fatalf("expected primary + one fallback search, got queries=%v", peers.queries)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 || jobs[0].CandidatesTried != 1 {
		t.Fatalf("exhausted tick should increment candidates_tried to 1 exactly once and cooldown, got %+v", jobs)
	}
	if jobs[0].NextAttemptAt == nil {
		t.Fatalf("cooldown job missing next_attempt_at")
	}
	if got := jobs[0].NextAttemptAt.Sub(now); got != p.CandidateBackoff {
		t.Errorf("no-candidate cooldown should use the long backoff %v, got %v", p.CandidateBackoff, got)
	}
}

func TestStartJobSkipsFallbackWhenNormalizedQueryIsEmpty(t *testing.T) {
	// A query made entirely of bracketed groups normalizes to the empty
	// string; searching Soulseek for "" is meaningless, so no fallback search
	// must be issued and the job takes the ordinary cooldown path.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 91, Title: "[Untitled]", ArtistName: "(!!!)"}}}
	peers := &fakeSearcher{} // primary query gets zero results
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
	if len(peers.queries) != 1 {
		t.Fatalf("expected no fallback search when the normalized query is empty, got queries=%q", peers.queries)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 || jobs[0].CandidatesTried != 1 {
		t.Fatalf("job should cool down with candidates_tried == 1, got %+v", jobs)
	}
}

func TestStartJobFallbackSearchErrorPropagates(t *testing.T) {
	// The fallback Search's error must propagate out of startJob like the
	// primary's: startNewJobs logs and isolates it, so the job stays where it
	// was with no candidate budget spent, and is retried on a later tick.
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 91, Title: "Album (Deluxe Edition)", ArtistName: "X"}}}
	fallbackQuery := normalizeQuery("X Album (Deluxe Edition)")
	peers := &fakeSearcher{
		searchErrForQuery: map[string]error{fallbackQuery: errors.New("slskd down")},
	}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := st.UpsertDiscoveredJob(ctx, 91, now); err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce should isolate the per-job error: %v", err)
	}
	if len(peers.queries) != 2 {
		t.Fatalf("expected primary + fallback search, got queries=%q", peers.queries)
	}
	cooldown, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(cooldown) != 0 {
		t.Errorf("errored fallback search must not spend budget or cool down, got %+v", cooldown)
	}
	discovered, _ := st.JobsInState(ctx, core.StateDiscovered, 10)
	if len(discovered) != 1 || discovered[0].CandidatesTried != 0 {
		t.Fatalf("job should remain DISCOVERED with candidates_tried == 0, got %+v", discovered)
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

func TestAdvanceImportingSkipsFailingJobButProcessesOthers(t *testing.T) {
	// Reproduces the production incident: one VERIFYING job's
	// ManualImportCandidates call errors (e.g. Lidarr times out on a broken
	// folder). That must not prevent a second, healthy VERIFYING job in the
	// same batch from being processed this tick.
	music := &fakeMusic{
		candidates: []lidarr.ManualImportItem{
			{ID: 1, Path: "/music/slskd-downloads/Alb20/01.flac", Importable: true},
		},
		candidatesErrForFolder: map[string]error{
			"/music/slskd-downloads/Alb10": errors.New("context deadline exceeded"),
		},
	}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJobForAlbum(t, 10, now)          // will error
	job20 := st.seedVerifyingJobForAlbum(t, 20, now) // must still be processed
	d := NewDiscoverer(p)
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, now); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(music.imported) != 1 {
		t.Fatalf("expected the healthy job's candidate to still be imported, got %d imports", len(music.imported))
	}
	if state := jobState(t, st, job20); state != core.StateCompleted && state != core.StateImporting {
		t.Errorf("healthy job should have progressed past VERIFYING, got state %v", state)
	}
}

func TestAdvanceImportingEscalatesStuckManualImportCandidatesError(t *testing.T) {
	// A job whose ManualImportCandidates call keeps failing forever (e.g. a
	// permanently broken folder) must eventually be failed and cooled down
	// rather than sitting in VERIFYING forever, once it has been stuck longer
	// than ImportConfirmTimeout.
	music := &fakeMusic{
		candidatesErrForFolder: map[string]error{
			"/music/slskd-downloads/Alb10": errors.New("context deadline exceeded"),
		},
	}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	stuckSince := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJobForAlbum(t, 10, stuckSince)
	d := NewDiscoverer(p)

	// Still within the timeout: job must stay VERIFYING, not be abandoned.
	withinTimeout := stuckSince.Add(time.Second)
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, withinTimeout); err != nil {
		t.Fatalf("Advance (within timeout): %v", err)
	}
	verifying, _ := st.JobsInState(ctx, core.StateVerifying, 10)
	if len(verifying) != 1 {
		t.Fatalf("expected job to remain VERIFYING within the timeout, got %d", len(verifying))
	}

	// Past the timeout: job must be failed and cooled down, not stuck forever.
	pastTimeout := stuckSince.Add(p.ImportConfirmTimeout + time.Second)
	if err := d.Advance(ctx, map[int64]lidarr.WantedAlbum{}, pastTimeout); err != nil {
		t.Fatalf("Advance (past timeout): %v", err)
	}
	cooldown, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(cooldown) != 1 {
		t.Fatalf("expected job stuck past ImportConfirmTimeout to cool down, got %d COOLDOWN jobs", len(cooldown))
	}
}

func TestConfirmImportsSkipsFailingJobButProcessesOthers(t *testing.T) {
	// Same architectural bug as advanceImporting, but in confirmImports's
	// AlbumStatus call: one IMPORTING job's AlbumStatus error must not block a
	// second, healthy IMPORTING job in the same batch.
	music := &fakeMusic{
		albumStatusErrForAlbum: map[int64]error{
			10: errors.New("context deadline exceeded"),
		},
		albumPresent: 1, albumTotal: 1, // album 20 is fully present
	}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedImportingJobForAlbum(t, 10, now)          // will error
	job20 := st.seedImportingJobForAlbum(t, 20, now) // must still be processed
	d := NewDiscoverer(p)
	if err := d.confirmImports(ctx, now); err != nil {
		t.Fatalf("confirmImports: %v", err)
	}
	if state := jobState(t, st, job20); state != core.StateCompleted {
		t.Errorf("healthy job should have completed, got state %v", state)
	}
}

func TestConfirmImportsEscalatesStuckAlbumStatusError(t *testing.T) {
	// An IMPORTING job whose AlbumStatus call keeps erroring forever must
	// eventually be failed and cooled down, same as the existing
	// present<total timeout branch, rather than being stuck in IMPORTING
	// forever because the error path returns before that check ever runs.
	music := &fakeMusic{
		albumStatusErrForAlbum: map[int64]error{10: errors.New("context deadline exceeded")},
	}
	p, st := newDiscoParams(t, music, &fakeSearcher{})
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedImportingJobForAlbum(t, 10, now.Add(-p.ImportConfirmTimeout-time.Second))
	d := NewDiscoverer(p)
	if err := d.confirmImports(ctx, now); err != nil {
		t.Fatalf("confirmImports: %v", err)
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 {
		t.Errorf("AlbumStatus error stuck past the timeout should cool down, got %d COOLDOWN jobs", len(jobs))
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
