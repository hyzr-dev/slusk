package pipeline

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/store"
	"github.com/hyzr-dev/slusk/internal/store/storetest"
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
	wanted    []core.WantedRelease
	wantedErr error

	albumPresent   int
	albumTotal     int
	albumStatusErr error

	// albumReleases/albumReleasesErr drive AlbumReleases, Discovery's source
	// for the album's valid track-count band.
	albumReleases    []core.AlbumRelease
	albumReleasesErr error

	// albumTracks/albumTracksErr drive AlbumTracks, the relevance gate's
	// (#316) source of the album's expected track titles. A nil/empty
	// albumTracks (the zero value) exercises the gate's directory-only
	// fallback without needing an explicit error. albumTracksCalls counts
	// invocations, so tests can assert it is skipped when there is nothing
	// to relevance-check against (an empty Peers.Search result).
	albumTracks      []core.AlbumTrack
	albumTracksErr   error
	albumTracksCalls int

	// manualImportItems/manualImportErr drive ManualImportCandidates; folders
	// records every folder it was called with, in order, so tests can assert
	// AlbumFolder computed the expected path.
	manualImportItems []core.ImportItem
	manualImportErr   error
	manualImportCalls []string

	// executeManualImportErr, when set, fails ExecuteManualImport; executedItems
	// records the items it was actually called with.
	executeManualImportErr error
	executedItems          []core.ImportItem

	// albumByForeignID/albumByForeignIDFound/albumByForeignIDErr drive
	// AlbumByForeignID, Importing's MBID->Lidarr-album resolution for a
	// manual job (issue #59). albumByForeignIDCalls records every foreign id
	// it was called with, in order.
	albumByForeignID      core.LidarrAlbum
	albumByForeignIDFound bool
	albumByForeignIDErr   error
	albumByForeignIDCalls []string
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]core.WantedRelease, error) {
	if f.wantedErr != nil {
		return nil, f.wantedErr
	}
	return f.wanted, nil
}

func (f *fakeMusic) ManualImportCandidates(ctx context.Context, folder string) ([]core.ImportItem, error) {
	f.manualImportCalls = append(f.manualImportCalls, folder)
	if f.manualImportErr != nil {
		return nil, f.manualImportErr
	}
	return f.manualImportItems, nil
}

func (f *fakeMusic) ExecuteManualImport(ctx context.Context, items []core.ImportItem) error {
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

func (f *fakeMusic) AlbumReleases(ctx context.Context, albumID int64) ([]core.AlbumRelease, error) {
	if f.albumReleasesErr != nil {
		return nil, f.albumReleasesErr
	}
	return f.albumReleases, nil
}

func (f *fakeMusic) AlbumTracks(ctx context.Context, albumID int64) ([]core.AlbumTrack, error) {
	f.albumTracksCalls++
	if f.albumTracksErr != nil {
		return nil, f.albumTracksErr
	}
	return f.albumTracks, nil
}

func (f *fakeMusic) AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
	f.albumByForeignIDCalls = append(f.albumByForeignIDCalls, foreignAlbumID)
	if f.albumByForeignIDErr != nil {
		return core.LidarrAlbum{}, false, f.albumByForeignIDErr
	}
	return f.albumByForeignID, f.albumByForeignIDFound, nil
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
	downloads []core.RemoteTransfer
	cancelled []string
	cancelErr error
	removed   []string
}

func (f *fakeNetwork) ListDownloads(ctx context.Context) ([]core.RemoteTransfer, error) {
	return f.downloads, nil
}

func (f *fakeNetwork) Cancel(ctx context.Context, username, id string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	for i := range f.downloads {
		if f.downloads[i].ID == id && f.downloads[i].Username == username {
			f.downloads[i].State = core.TransferCancelled
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
	resultsForQuery map[string][]core.SearchResult
	// results is the default returned for a query absent from resultsForQuery.
	results []core.SearchResult
	// searchErrForQuery injects a Search error for a specific query, so tests
	// can fail e.g. only the fallback search while the primary succeeds.
	searchErrForQuery map[string]error

	// enqueued records every filename Enqueue was called with, in call order,
	// so Selecting/Downloading tests can assert how many (and which) files
	// were actually handed to slskd.
	enqueued []string
	// enqueueErr, when set, fails every Enqueue call. enqueueHook runs after
	// the remote identity exists but before Enqueue returns, allowing lifecycle
	// race tests to deterministically interleave cancellation/deletion.
	enqueueErr  error
	enqueueHook func()

	// cancelled records every id Cancel was called with, in order, and
	// deletedFolders every folder DeleteDownloadFolder was called with, so
	// Downloading's two-phase fail path and cleanup can be asserted.
	cancelled      []string
	deletedFolders []string
	// cancelErr, when set, fails every Cancel call (a core.ErrRemoteNotFound error
	// exercises the treat-as-already-terminal branch; any other error the
	// leave-active-and-retry branch).
	cancelErr error
}

func (f *fakeSearcher) Search(ctx context.Context, query string, timeout time.Duration) ([]core.SearchResult, error) {
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
	if f.enqueueHook != nil {
		f.enqueueHook()
	}
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
