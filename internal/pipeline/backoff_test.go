package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

var errBoom = errors.New("boom")

func TestNextBackoff(t *testing.T) {
	base, maxBackoff := 15*time.Minute, 24*time.Hour
	cases := []struct {
		retries int
		want    time.Duration
	}{
		{1, 30 * time.Minute}, // 15m * 2^1
		{2, 1 * time.Hour},
		{3, 2 * time.Hour},
		{7, 24 * time.Hour},  // 15m*2^7=32h -> capped
		{50, 24 * time.Hour}, // no overflow
	}
	for _, tc := range cases {
		got := nextBackoff(tc.retries, base, maxBackoff)
		if got != tc.want {
			t.Errorf("nextBackoff(%d) = %v, want %v", tc.retries, got, tc.want)
		}
	}
}

// fakeBackoffStore records the calls failOrBackoff makes, for assertions
// without a real database.
type fakeBackoffStore struct {
	backoffCalls []struct {
		jobID     int64
		retries   int
		notBefore time.Time
	}
	failedCalls []int64
	resetCalls  []struct {
		jobID     int64
		retries   int
		notBefore *time.Time
	}
	events []struct {
		jobID int64
		event core.JobEventType
	}
	failAddJobEvent bool
}

func (f *fakeBackoffStore) SetJobBackoff(_ context.Context, jobID int64, retries int, notBefore time.Time, _ time.Time) error {
	f.backoffCalls = append(f.backoffCalls, struct {
		jobID     int64
		retries   int
		notBefore time.Time
	}{jobID, retries, notBefore})
	return nil
}

func (f *fakeBackoffStore) MarkJobFailed(_ context.Context, jobID int64, _ time.Time) error {
	f.failedCalls = append(f.failedCalls, jobID)
	return nil
}

func (f *fakeBackoffStore) ResetJobToWanted(_ context.Context, jobID int64, _ core.AlbumJobState, retries int, notBefore *time.Time, _ time.Time) error {
	f.resetCalls = append(f.resetCalls, struct {
		jobID     int64
		retries   int
		notBefore *time.Time
	}{jobID, retries, notBefore})
	return nil
}

func (f *fakeBackoffStore) AddJobEvent(_ context.Context, jobID int64, event core.JobEventType, _ string, _ time.Time) error {
	if f.failAddJobEvent {
		return errBoom
	}
	f.events = append(f.events, struct {
		jobID int64
		event core.JobEventType
	}{jobID, event})
	return nil
}

func TestFailOrBackoffMarksFailedAtMaxRetries(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 1, Retries: 2}
	now := time.Now()

	err := failOrBackoff(context.Background(), st, discardLogger(), job, 3, 15*time.Minute, 24*time.Hour, false, now)
	if err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.failedCalls) != 1 || st.failedCalls[0] != 1 {
		t.Errorf("MarkJobFailed calls = %v, want [1]", st.failedCalls)
	}
	if len(st.backoffCalls) != 0 {
		t.Errorf("SetJobBackoff should not be called, got %v", st.backoffCalls)
	}
	if len(st.resetCalls) != 0 {
		t.Errorf("ResetJobToWanted should not be called, got %v", st.resetCalls)
	}
	if len(st.events) != 1 || st.events[0].event != core.EventJobFailed {
		t.Errorf("events = %v, want one EventJobFailed", st.events)
	}
}

func TestFailOrBackoffSetsBackoffWhenNotResettingToWanted(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 5, Retries: 0}
	now := time.Now()

	err := failOrBackoff(context.Background(), st, discardLogger(), job, 5, 15*time.Minute, 24*time.Hour, false, now)
	if err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.backoffCalls) != 1 {
		t.Fatalf("SetJobBackoff calls = %v, want 1 call", st.backoffCalls)
	}
	call := st.backoffCalls[0]
	if call.jobID != 5 || call.retries != 1 {
		t.Errorf("SetJobBackoff call = %+v, want jobID=5 retries=1", call)
	}
	wantNotBefore := now.Add(30 * time.Minute)
	if !call.notBefore.Equal(wantNotBefore) {
		t.Errorf("notBefore = %v, want %v", call.notBefore, wantNotBefore)
	}
	if len(st.resetCalls) != 0 {
		t.Errorf("ResetJobToWanted should not be called, got %v", st.resetCalls)
	}
}

func TestFailOrBackoffResetsToWantedWhenRequested(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 9, Retries: 1}
	now := time.Now()

	err := failOrBackoff(context.Background(), st, discardLogger(), job, 5, 15*time.Minute, 24*time.Hour, true, now)
	if err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.resetCalls) != 1 {
		t.Fatalf("ResetJobToWanted calls = %v, want 1 call", st.resetCalls)
	}
	call := st.resetCalls[0]
	if call.jobID != 9 || call.retries != 2 {
		t.Errorf("ResetJobToWanted call = %+v, want jobID=9 retries=2", call)
	}
	wantNotBefore := now.Add(1 * time.Hour)
	if call.notBefore == nil || !call.notBefore.Equal(wantNotBefore) {
		t.Errorf("notBefore = %v, want %v", call.notBefore, wantNotBefore)
	}
	if len(st.backoffCalls) != 0 {
		t.Errorf("SetJobBackoff should not be called, got %v", st.backoffCalls)
	}
}

func TestFailOrBackoffAddJobEventIsBestEffort(t *testing.T) {
	st := &fakeBackoffStore{failAddJobEvent: true}
	job := core.AlbumJob{ID: 1, Retries: 2}
	now := time.Now()

	err := failOrBackoff(context.Background(), st, discardLogger(), job, 3, 15*time.Minute, 24*time.Hour, false, now)
	if err != nil {
		t.Fatalf("failOrBackoff should swallow AddJobEvent errors, got: %v", err)
	}
	if len(st.failedCalls) != 1 {
		t.Errorf("MarkJobFailed should still be called, got %v", st.failedCalls)
	}
}
