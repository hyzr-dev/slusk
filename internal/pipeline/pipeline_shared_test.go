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
// Discovery additionally uses AlbumStatus. ManualImportCandidates and
// ExecuteManualImport are stubbed no-ops since no module up to Discovery
// calls them - they exist only so fakeMusic satisfies the full MusicSource
// interface.
type fakeMusic struct {
	wanted    []lidarr.WantedAlbum
	wantedErr error

	albumPresent   int
	albumTotal     int
	albumStatusErr error
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error) {
	if f.wantedErr != nil {
		return nil, f.wantedErr
	}
	return f.wanted, nil
}

func (f *fakeMusic) ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error) {
	return nil, nil
}

func (f *fakeMusic) ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error {
	return nil
}

func (f *fakeMusic) AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error) {
	if f.albumStatusErr != nil {
		return 0, 0, f.albumStatusErr
	}
	return f.albumPresent, f.albumTotal, nil
}

// fakeSearcher is a PeerSearcher fake; ported from
// internal/engine/discovery_test.go's fakeSearcher. Cancel/DeleteDownloadFolder
// are stubbed no-ops so fakeSearcher satisfies the full PeerSearcher
// interface - no module up to Selecting calls them.
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

func (f *fakeSearcher) Cancel(ctx context.Context, username, id string) error { return nil }

func (f *fakeSearcher) DeleteDownloadFolder(ctx context.Context, name string) error { return nil }
