package soulseek

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

// recordingUploadSink collects the records handed to it. It is mutex-guarded
// because the sink is called from the upload's own goroutine.
type recordingUploadSink struct {
	mu      sync.Mutex
	records []UploadRecord
	err     error
	// ctxErrs captures each call's context error, so a test can prove the
	// write is still live when the upload's own context is already cancelled.
	ctxErrs []error
}

func (s *recordingUploadSink) RecordUpload(ctx context.Context, r UploadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return s.err
}

func (s *recordingUploadSink) all() []UploadRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]UploadRecord(nil), s.records...)
}

// TestRunUploadRecordsRejectedForUnsharedFile covers the one rejection path
// that needs no peer at all: the share entry vanished between enqueue and
// dispatch. It pins the zero bytes and zero rate a rejected row must carry —
// a true measurement, not a missing one.
func TestRunUploadRecordsRejectedForUnsharedFile(t *testing.T) {
	sink := &recordingUploadSink{}
	c := New(Config{UploadSink: sink}, testLogger())
	job := &uploadJob{key: uploadKey{username: "alice", filename: `Music\gone.flac`}}

	c.runUpload(context.Background(), job)

	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(records), records)
	}
	got := records[0]
	if got.Status != core.UploadRejected || got.Detail != uploadDetailFileUnavailable {
		t.Errorf("status/detail = %q/%q, want rejected/%q", got.Status, got.Detail, uploadDetailFileUnavailable)
	}
	if got.Username != "alice" || got.Filename != `Music\gone.flac` {
		t.Errorf("identity = %q/%q", got.Username, got.Filename)
	}
	if got.BytesSent != 0 || got.AvgBytesPerSecond != 0 {
		t.Errorf("bytes/rate = %d/%d, want 0/0 for a rejected upload", got.BytesSent, got.AvgBytesPerSecond)
	}
	if got.FinishedAt.Before(got.StartedAt) {
		t.Errorf("FinishedAt %v is before StartedAt %v", got.FinishedAt, got.StartedAt)
	}
}

// TestRecordUploadSurvivesCancelledContext is the reason recordUpload derives
// its context with WithoutCancel: by the time an upload's outcome is known its
// own ctx is frequently already cancelled — shutdown, or the connection going
// away is what ended the transfer — and reusing it would make exactly the
// interesting rows fail to persist.
func TestRecordUploadSurvivesCancelledContext(t *testing.T) {
	sink := &recordingUploadSink{}
	c := New(Config{UploadSink: sink}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.recordUpload(ctx, UploadRecord{Username: "alice", Filename: `Music\a.flac`, Status: core.UploadCompleted})

	if got := len(sink.all()); got != 1 {
		t.Fatalf("got %d records, want the write to happen despite the cancelled caller context", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.ctxErrs[0] != nil {
		t.Errorf("sink context error = %v, want a live context", sink.ctxErrs[0])
	}
}

// TestRecordUploadNilSinkAndSinkErrorAreHarmless asserts the two no-op cases:
// no sink configured at all, and a sink that fails. Neither may panic or
// propagate, since the peer has already been served either way.
func TestRecordUploadNilSinkAndSinkErrorAreHarmless(t *testing.T) {
	New(Config{}, testLogger()).recordUpload(context.Background(), UploadRecord{Username: "alice"})

	failing := &recordingUploadSink{err: errors.New("database is down")}
	c := New(Config{UploadSink: failing}, testLogger())
	c.recordUpload(context.Background(), UploadRecord{Username: "alice", Status: core.UploadCompleted})
	if got := len(failing.all()); got != 1 {
		t.Fatalf("sink call count = %d, want 1", got)
	}
}

// TestUploadBytesSentIsResumeAware pins the delta arithmetic. uploadJob.sent is
// seeded with the peer's requested offset, so reporting it directly would
// silently overstate a resumed upload's volume and, through it, its speed.
func TestUploadBytesSentIsResumeAware(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sent, offset uint64
		want         uint64
	}{
		{"fresh transfer", 1000, 0, 1000},
		{"resumed at 90%", 1000, 900, 100},
		{"nothing left to send", 1000, 1000, 0},
		{"impossible ordering saturates instead of wrapping", 100, 900, 0},
	} {
		if got := uploadBytesSent(tc.sent, tc.offset); got != tc.want {
			t.Errorf("%s: uploadBytesSent(%d, %d) = %d, want %d", tc.name, tc.sent, tc.offset, got, tc.want)
		}
	}
}

// TestUploadAvgBytesPerSecondUsesStreamDuration asserts the rate divides by the
// streaming phase's own duration and reports 0 — not a division result — when
// there was no stream to measure.
func TestUploadAvgBytesPerSecondUsesStreamDuration(t *testing.T) {
	if got := uploadAvgBytesPerSecond(1000, 2*time.Second); got != 500 {
		t.Errorf("uploadAvgBytesPerSecond(1000, 2s) = %d, want 500", got)
	}
	if got := uploadAvgBytesPerSecond(0, time.Second); got != 0 {
		t.Errorf("uploadAvgBytesPerSecond(0, 1s) = %d, want 0", got)
	}
	if got := uploadAvgBytesPerSecond(1000, 0); got != 0 {
		t.Errorf("uploadAvgBytesPerSecond(1000, 0) = %d, want 0, not a division by zero", got)
	}
}
