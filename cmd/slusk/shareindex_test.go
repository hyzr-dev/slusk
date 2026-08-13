package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/soulseek"
)

// fakeShareIndexStore is a shareIndexStore whose behaviour and call history are
// entirely inspectable, mirroring fakeShareMetaStore in sharemeta_test.go.
type fakeShareIndexStore struct {
	loaded  *core.ShareIndex
	loadErr error
	saveErr error

	saved core.ShareIndex
}

func (f *fakeShareIndexStore) ShareIndex(ctx context.Context) (*core.ShareIndex, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.loaded, nil
}

func (f *fakeShareIndexStore) ReplaceShareIndex(ctx context.Context, index core.ShareIndex) error {
	f.saved = index
	return f.saveErr
}

func TestShareIndexAdapterMapsFieldsBothDirections(t *testing.T) {
	scannedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	mod := scannedAt.Add(-time.Hour)
	store := &fakeShareIndexStore{loaded: &core.ShareIndex{
		ScannedAt:    scannedAt,
		ScanDuration: 90 * time.Second,
		Folders:      []core.SharedFolder{{Name: "Music", Path: "/music"}},
		Directories:  []string{"Music"},
		Files: []core.ShareIndexEntry{{
			VirtualPath: `Music\a.flac`, LocalPath: "/music/a.flac", ShareRoot: "/music",
			Size: 123, Extension: "flac", ModTime: mod, Bitrate: 320, Duration: 200,
		}},
		FileCount:  1,
		TotalBytes: 123,
	}}
	adapter := &shareIndexAdapter{store: store}

	got, err := adapter.LoadShareIndex(context.Background())
	if err != nil {
		t.Fatalf("LoadShareIndex: %v", err)
	}
	want := &soulseek.ShareIndex{
		ScannedAt:    scannedAt,
		ScanDuration: 90 * time.Second,
		Folders:      []soulseek.SharedFolder{{Name: "Music", Path: "/music"}},
		Directories:  []string{"Music"},
		Files: []soulseek.ShareIndexEntry{{
			VirtualPath: `Music\a.flac`, LocalPath: "/music/a.flac", ShareRoot: "/music",
			Size: 123, Extension: "flac", ModTime: mod, Bitrate: 320, Duration: 200,
		}},
		FileCount:  1,
		TotalBytes: 123,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadShareIndex = %+v, want %+v", got, want)
	}

	if err := adapter.SaveShareIndex(context.Background(), got); err != nil {
		t.Fatalf("SaveShareIndex: %v", err)
	}
	saved := store.saved
	// FileCount is deliberately not carried across: the store derives it from
	// the rows it writes, so the two can never disagree.
	if saved.FileCount != 0 {
		t.Fatalf("saved FileCount = %d, want it left to the store", saved.FileCount)
	}
	saved.FileCount = 1
	if !reflect.DeepEqual(saved, *store.loaded) {
		t.Fatalf("SaveShareIndex stored %+v, want %+v", saved, *store.loaded)
	}
}

// TestShareIndexAdapterLoadPassesThroughAbsence asserts "nothing stored" stays
// nil rather than becoming an empty index, which would instead mean "the last
// scan found no files".
func TestShareIndexAdapterLoadPassesThroughAbsence(t *testing.T) {
	adapter := &shareIndexAdapter{store: &fakeShareIndexStore{}}
	got, err := adapter.LoadShareIndex(context.Background())
	if err != nil || got != nil {
		t.Fatalf("LoadShareIndex = %+v, %v; want nil, nil", got, err)
	}
}

func TestShareIndexAdapterPropagatesStoreErrors(t *testing.T) {
	loadErr := errors.New("boom")
	adapter := &shareIndexAdapter{store: &fakeShareIndexStore{loadErr: loadErr}}
	if _, err := adapter.LoadShareIndex(context.Background()); !errors.Is(err, loadErr) {
		t.Fatalf("LoadShareIndex error = %v, want %v", err, loadErr)
	}

	saveErr := errors.New("disk full")
	adapter = &shareIndexAdapter{store: &fakeShareIndexStore{saveErr: saveErr}}
	if err := adapter.SaveShareIndex(context.Background(), &soulseek.ShareIndex{}); !errors.Is(err, saveErr) {
		t.Fatalf("SaveShareIndex error = %v, want %v", err, saveErr)
	}
}
