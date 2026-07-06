package pipeline

import (
	"context"
	"os"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/lidarr"
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

// fakeMusic is a MusicSource fake trimmed to what WantedSync needs. Later
// pipeline module tasks (Discovery, etc.) extend it with the other
// MusicSource methods as they're needed.
type fakeMusic struct {
	wanted    []lidarr.WantedAlbum
	wantedErr error
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error) {
	if f.wantedErr != nil {
		return nil, f.wantedErr
	}
	return f.wanted, nil
}
