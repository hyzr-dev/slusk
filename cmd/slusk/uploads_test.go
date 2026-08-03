package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/soulseek"
)

type recordingUploadHistoryStore struct {
	entries []core.UploadHistoryEntry
	err     error
}

func (s *recordingUploadHistoryStore) RecordUpload(_ context.Context, e core.UploadHistoryEntry) error {
	s.entries = append(s.entries, e)
	return s.err
}

// TestUploadSinkMapsEveryFieldAndNormalizesToUTC pins the adapter: a dropped
// field here is invisible everywhere else, since the API would simply serve a
// zero that looks like a real measurement.
func TestUploadSinkMapsEveryFieldAndNormalizesToUTC(t *testing.T) {
	st := &recordingUploadHistoryStore{}
	sink := &uploadSink{store: st}
	zone := time.FixedZone("CEST", 2*60*60)
	started := time.Date(2026, 7, 31, 12, 0, 0, 0, zone)

	err := sink.RecordUpload(context.Background(), soulseek.UploadRecord{
		Username: "alice", Filename: `Music\a.flac`, Size: 5000, BytesSent: 4000,
		AvgBytesPerSecond: 2000, StartedAt: started, FinishedAt: started.Add(2 * time.Second),
		Status: core.UploadAborted, Detail: "below minimum throughput",
	})
	if err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}
	if len(st.entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(st.entries))
	}
	got := st.entries[0]
	if got.Username != "alice" || got.Filename != `Music\a.flac` || got.Size != 5000 ||
		got.BytesSent != 4000 || got.AvgBytesPerSecond != 2000 ||
		got.Status != core.UploadAborted || got.Detail != "below minimum throughput" {
		t.Errorf("entry = %+v", got)
	}
	if !got.StartedAt.Equal(started) || got.StartedAt.Location() != time.UTC {
		t.Errorf("StartedAt = %v (%v), want the same instant in UTC", got.StartedAt, got.StartedAt.Location())
	}
	if !got.FinishedAt.Equal(started.Add(2*time.Second)) || got.FinishedAt.Location() != time.UTC {
		t.Errorf("FinishedAt = %v (%v), want the same instant in UTC", got.FinishedAt, got.FinishedAt.Location())
	}
}

// TestUploadSinkPropagatesStoreError asserts the adapter does not swallow the
// failure itself — the client is what decides to log and move on, and it can
// only do that if it is told.
func TestUploadSinkPropagatesStoreError(t *testing.T) {
	want := errors.New("database is down")
	sink := &uploadSink{store: &recordingUploadHistoryStore{err: want}}
	if err := sink.RecordUpload(context.Background(), soulseek.UploadRecord{Username: "alice"}); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}
