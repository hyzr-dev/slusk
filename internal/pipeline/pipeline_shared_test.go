package pipeline

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
	"github.com/samuelenocsson/slskdarr/internal/store/storetest"
)

// TestMain starts one embedded Postgres instance for this package's
// store-backed tests (see newBackedStore). Later pipeline module tasks reuse
// this same instance rather than starting their own.
func TestMain(m *testing.M) {
	os.Exit(storetest.Run(m))
}

// newBackedStore opens a *store.Store against a fresh per-test database,
// closed automatically at test cleanup. A real store-backed fixture is
// simpler and more faithful than a hand-written fake for the wide store
// interfaces pipeline modules consume.
func newBackedStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeMusic is a MusicSource fake. WantedSync only ever uses WantedMissing;
// Discovery additionally uses AlbumStatus. Importing additionally uses
// ManualImportCandidates and ExecuteManualImport: manualImportItems/Err drive
// the verify phase's Lidarr manual-import-preview call, and
// executeManualImportErr/executedFolders let tests inject an
// ExecuteManualImport failure and assert what was actually submitted.
type fakeMusic struct {
	wanted    []lidarr.WantedAlbum
	wantedErr error

	albumPresent   int
	albumTotal     int
	albumStatusErr error

	// albumReleases/albumReleasesErr drive AlbumReleases, Discovery's source
	// for the album's valid track-count band.
	albumReleases    []lidarr.AlbumRelease
	albumReleasesErr error

	// manualImportItems/manualImportErr drive ManualImportCandidates; folders
	// records every folder it was called with, in order, so tests can assert
	// AlbumFolder computed the expected path.
	manualImportItems []lidarr.ManualImportItem
	manualImportErr   error
	manualImportCalls []string

	// executeManualImportErr, when set, fails ExecuteManualImport; executedItems
	// records the items it was actually called with.
	executeManualImportErr error
	executedItems          []lidarr.ManualImportItem
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error) {
	if f.wantedErr != nil {
		return nil, f.wantedErr
	}
	return f.wanted, nil
}

func (f *fakeMusic) ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error) {
	f.manualImportCalls = append(f.manualImportCalls, folder)
	if f.manualImportErr != nil {
		return nil, f.manualImportErr
	}
	return f.manualImportItems, nil
}

func (f *fakeMusic) ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error {
	if f.executeManualImportErr != nil {
		return f.executeManualImportErr
	}
	f.executedItems = append(f.executedItems, items...)
	return nil
}

func (f *fakeMusic) AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error) {
	if f.albumStatusErr != nil {
		return 0, 0, f.albumStatusErr
	}
	return f.albumPresent, f.albumTotal, nil
}

func (f *fakeMusic) AlbumReleases(ctx context.Context, albumID int64) ([]lidarr.AlbumRelease, error) {
	if f.albumReleasesErr != nil {
		return nil, f.albumReleasesErr
	}
	return f.albumReleases, nil
}

// fakeNetwork is an in-memory PeerNetwork fake for the Downloading module's
// reconcile phase; ported from internal/engine/reconciler_test.go's fakePeers.
// downloads is what ListDownloads returns (including retained terminal
// records); cancelled records every id Cancel was called with, in order and a
// successful call changes the matching record to production's terminal
// "Completed, Cancelled" state. cancelErr, when set, fails every Cancel call.
// removed records every id Remove was called with, in order, so tests can
// assert reconcile purges terminal transfers from slskd.
type fakeNetwork struct {
	downloads []slskd.Transfer
	cancelled []string
	cancelErr error
	removed   []string
}

func (f *fakeNetwork) ListDownloads(ctx context.Context) ([]slskd.Transfer, error) {
	return f.downloads, nil
}

func (f *fakeNetwork) Cancel(ctx context.Context, username, id string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	for i := range f.downloads {
		if f.downloads[i].ID == id && f.downloads[i].Username == username {
			f.downloads[i].State = "Completed, Cancelled"
		}
	}
	return nil
}

func (f *fakeNetwork) Remove(ctx context.Context, username, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

// fakeSearcher is a PeerSearcher fake; ported from
// internal/engine/discovery_test.go's fakeSearcher. Cancel/DeleteDownloadFolder
// record their calls (and honour cancelErr) so Downloading's two-phase fail
// path and cleanup can be asserted; no module up to Selecting exercises them.
type fakeSearcher struct {
	// queries records every query Search was called with, in order, so tests
	// can assert how many searches were issued and what they were.
	queries []string
	// resultsForQuery, when set, overrides results on a per-query basis
	// (falling back to results for queries not present in the map). Used to
	// give the primary and normalized fallback query different results.
	resultsForQuery map[string][]slskd.Result
	// results is the default returned for a query absent from resultsForQuery.
	results []slskd.Result
	// searchErrForQuery injects a Search error for a specific query, so tests
	// can fail e.g. only the fallback search while the primary succeeds.
	searchErrForQuery map[string]error

	// enqueued records every filename Enqueue was called with, in call order,
	// so Selecting/Downloading tests can assert how many (and which) files
	// were actually handed to slskd.
	enqueued []string
	// enqueueErr, when set, fails every Enqueue call.
	enqueueErr error

	// cancelled records every id Cancel was called with, in order, and
	// deletedFolders every folder DeleteDownloadFolder was called with, so
	// Downloading's two-phase fail path and cleanup can be asserted.
	cancelled      []string
	deletedFolders []string
	// cancelErr, when set, fails every Cancel call (an slskd.IsNotFound error
	// exercises the treat-as-already-terminal branch; any other error the
	// leave-active-and-retry branch).
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
	f.enqueued = append(f.enqueued, filename)
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
	return nil
}
