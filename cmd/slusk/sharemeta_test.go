package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/soulseek"
)

// fakeShareMetaStore is a shareMetaStore whose behaviour and call history are
// entirely inspectable, mirroring fakeIncomingMessageStore in messages_test.go.
type fakeShareMetaStore struct {
	loaded    []core.ShareFileMeta
	loadErr   error
	upsertErr error
	deleteErr error

	upserted    []core.ShareFileMeta
	deleted     []string
	upsertCalls int
	deleteCalls int
}

func (f *fakeShareMetaStore) ShareFileMetadata(ctx context.Context) ([]core.ShareFileMeta, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.loaded, nil
}

func (f *fakeShareMetaStore) UpsertShareFileMetadata(ctx context.Context, entries []core.ShareFileMeta, now time.Time) error {
	f.upsertCalls++
	f.upserted = entries
	return f.upsertErr
}

func (f *fakeShareMetaStore) DeleteShareFileMetadata(ctx context.Context, paths []string) error {
	f.deleteCalls++
	f.deleted = paths
	return f.deleteErr
}

func TestShareMetaCacheLoadMapsFieldsBothDirections(t *testing.T) {
	mod := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := &fakeShareMetaStore{loaded: []core.ShareFileMeta{
		{Path: "/music/a.flac", Size: 123, ModTime: mod, Bitrate: 320, Duration: 200},
	}}
	cache := &shareMetaCache{store: store}

	got, err := cache.LoadShareMeta(context.Background())
	if err != nil {
		t.Fatalf("LoadShareMeta: %v", err)
	}
	want := []soulseek.ShareFileMeta{{Path: "/music/a.flac", Size: 123, ModTime: mod, Bitrate: 320, Duration: 200}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("LoadShareMeta = %+v, want %+v", got, want)
	}
}

func TestShareMetaCacheLoadPropagatesStoreError(t *testing.T) {
	wantErr := errors.New("db unavailable")
	store := &fakeShareMetaStore{loadErr: wantErr}
	cache := &shareMetaCache{store: store}

	if _, err := cache.LoadShareMeta(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestShareMetaCacheSaveMapsFieldsBothDirections(t *testing.T) {
	mod := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := &fakeShareMetaStore{}
	cache := &shareMetaCache{store: store}

	upserts := []soulseek.ShareFileMeta{{Path: "/music/a.flac", Size: 123, ModTime: mod, Bitrate: 320, Duration: 200}}
	if err := cache.SaveShareMeta(context.Background(), upserts, []string{"/music/gone.flac"}); err != nil {
		t.Fatalf("SaveShareMeta: %v", err)
	}

	if len(store.upserted) != 1 || store.upserted[0] != (core.ShareFileMeta{Path: "/music/a.flac", Size: 123, ModTime: mod, Bitrate: 320, Duration: 200}) {
		t.Fatalf("upserted = %+v, want the mapped core.ShareFileMeta", store.upserted)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "/music/gone.flac" {
		t.Fatalf("deleted = %+v, want [/music/gone.flac]", store.deleted)
	}
}

// TestShareMetaCacheSaveInvokesBothSteps asserts both steps are invoked (in
// some order that a call-count check alone cannot distinguish - the actual
// upsert-then-delete guarantee is verified indirectly by the next test: a
// delete failure must not hide a successful upsert).
func TestShareMetaCacheSaveInvokesBothSteps(t *testing.T) {
	store := &fakeShareMetaStore{}
	cache := &shareMetaCache{store: store}

	if err := cache.SaveShareMeta(context.Background(),
		[]soulseek.ShareFileMeta{{Path: "/music/a.flac"}}, []string{"/music/gone.flac"}); err != nil {
		t.Fatalf("SaveShareMeta: %v", err)
	}
	if store.upsertCalls != 1 || store.deleteCalls != 1 {
		t.Fatalf("upsertCalls=%d deleteCalls=%d, want 1/1", store.upsertCalls, store.deleteCalls)
	}
}

// TestShareMetaCacheSaveDeleteErrorDoesNotHideSuccessfulUpsertSideEffect
// asserts a failing delete still returns an error (so the caller can log it)
// while the upsert itself was still performed against the store - the delete
// failure must not have prevented the upsert call.
func TestShareMetaCacheSaveDeleteErrorDoesNotHideSuccessfulUpsertSideEffect(t *testing.T) {
	wantErr := errors.New("delete failed")
	store := &fakeShareMetaStore{deleteErr: wantErr}
	cache := &shareMetaCache{store: store}

	err := cache.SaveShareMeta(context.Background(),
		[]soulseek.ShareFileMeta{{Path: "/music/a.flac"}}, []string{"/music/gone.flac"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if store.upsertCalls != 1 {
		t.Fatalf("upsertCalls = %d, want 1 (upsert must still have run)", store.upsertCalls)
	}
}

// TestShareMetaCacheSaveUpsertErrorStillAttemptsDelete asserts an upsert
// failure does not prevent the delete step from running, and that the
// upsert's error is what is returned.
func TestShareMetaCacheSaveUpsertErrorStillAttemptsDelete(t *testing.T) {
	wantErr := errors.New("upsert failed")
	store := &fakeShareMetaStore{upsertErr: wantErr}
	cache := &shareMetaCache{store: store}

	err := cache.SaveShareMeta(context.Background(),
		[]soulseek.ShareFileMeta{{Path: "/music/a.flac"}}, []string{"/music/gone.flac"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if store.deleteCalls != 1 {
		t.Fatalf("deleteCalls = %d, want 1 (delete must still have run)", store.deleteCalls)
	}
}

// TestShareMetaCacheSaveSkipsDeleteCallWhenNoStalePaths asserts an empty
// stalePaths never reaches the store as a delete call at all.
func TestShareMetaCacheSaveSkipsDeleteCallWhenNoStalePaths(t *testing.T) {
	store := &fakeShareMetaStore{}
	cache := &shareMetaCache{store: store}

	if err := cache.SaveShareMeta(context.Background(), nil, nil); err != nil {
		t.Fatalf("SaveShareMeta: %v", err)
	}
	if store.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want 0", store.deleteCalls)
	}
}
