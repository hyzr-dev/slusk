package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
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
		jobID  int64
		event  core.JobEventType
		detail string
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

func (f *fakeBackoffStore) AddJobEvent(_ context.Context, jobID int64, event core.JobEventType, detail string, _ time.Time) error {
	if f.failAddJobEvent {
		return errBoom
	}
	f.events = append(f.events, struct {
		jobID  int64
		event  core.JobEventType
		detail string
	}{jobID, event, detail})
	return nil
}

func TestFailOrBackoffMarksFailedAtMaxRetries(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 1, Retries: 2}
	now := time.Now()

	failed, err := failOrBackoff(context.Background(), st, discardLogger(), job, 3, 15*time.Minute, 24*time.Hour, false, "test reason", now)
	if err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if !failed {
		t.Error("failed = false, want true for the terminal transition")
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

// The terminal event must explain itself: issue #318 was that job_failed was
// always written with an empty detail, so the one event that marks the
// pipeline giving up said nothing about why, and the reason had to be derived
// from earlier events several retry cycles back.
func TestFailOrBackoffJobFailedEventCarriesRetriesAndReason(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 1, Retries: 2}
	now := time.Now()

	if _, err := failOrBackoff(context.Background(), st, discardLogger(), job, 3, 15*time.Minute, 24*time.Hour, false, "candidate cache exhausted", now); err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.events) != 1 {
		t.Fatalf("events = %v, want one event", st.events)
	}
	got := st.events[0].detail
	if got == "" {
		t.Fatal("job_failed detail is empty, want an explanation")
	}
	if !strings.Contains(got, "3") {
		t.Errorf("detail = %q, want it to mention the attempt count 3", got)
	}
	if !strings.Contains(got, "candidate cache exhausted") {
		t.Errorf("detail = %q, want it to carry the caller's reason", got)
	}
}

// A caller with nothing to add must still get a non-empty detail: the attempt
// count alone is more than the empty string #318 shipped.
func TestFailOrBackoffJobFailedEventWithoutReason(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 1, Retries: 0}
	now := time.Now()

	if _, err := failOrBackoff(context.Background(), st, discardLogger(), job, 1, 15*time.Minute, 24*time.Hour, false, "", now); err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.events) != 1 {
		t.Fatalf("events = %v, want one event", st.events)
	}
	if got := st.events[0].detail; got != "gave up after 1 attempt" {
		t.Errorf("detail = %q, want %q", got, "gave up after 1 attempt")
	}
}

// maxRetries=0 is Discovery's excluded-phrase path: the job is failed on the
// first hit by policy, so the attempts it happens to have accumulated for
// unrelated reasons must not be reported as a retry budget that ran out.
func TestFailOrBackoffJobFailedDropsAttemptCountWithoutRetryBudget(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 1, Retries: 2}
	now := time.Now()

	if _, err := failOrBackoff(context.Background(), st, discardLogger(), job, 0, 15*time.Minute, 24*time.Hour, false, "search excluded by server phrase list", now); err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.events) != 1 {
		t.Fatalf("events = %v, want one event", st.events)
	}
	got := st.events[0].detail
	if got != "search excluded by server phrase list" {
		t.Errorf("detail = %q, want the bare reason with no attempt count", got)
	}
}

// The count is only dropped when there is a reason to print instead - an
// empty detail is exactly what #318 set out to remove.
func TestFailOrBackoffJobFailedKeepsAttemptCountWhenReasonIsEmpty(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 1, Retries: 2}
	now := time.Now()

	if _, err := failOrBackoff(context.Background(), st, discardLogger(), job, 0, 15*time.Minute, 24*time.Hour, false, "", now); err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if len(st.events) != 1 {
		t.Fatalf("events = %v, want one event", st.events)
	}
	if got := st.events[0].detail; got != "gave up after 3 attempts" {
		t.Errorf("detail = %q, want %q", got, "gave up after 3 attempts")
	}
}

func TestFailOrBackoffSetsBackoffWhenNotResettingToWanted(t *testing.T) {
	st := &fakeBackoffStore{}
	job := core.AlbumJob{ID: 5, Retries: 0}
	now := time.Now()

	failed, err := failOrBackoff(context.Background(), st, discardLogger(), job, 5, 15*time.Minute, 24*time.Hour, false, "test reason", now)
	if err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if failed {
		t.Error("failed = true, want false when only backing off")
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

	failed, err := failOrBackoff(context.Background(), st, discardLogger(), job, 5, 15*time.Minute, 24*time.Hour, true, "test reason", now)
	if err != nil {
		t.Fatalf("failOrBackoff returned error: %v", err)
	}
	if failed {
		t.Error("failed = true, want false when only backing off")
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

	failed, err := failOrBackoff(context.Background(), st, discardLogger(), job, 3, 15*time.Minute, 24*time.Hour, false, "test reason", now)
	if err != nil {
		t.Fatalf("failOrBackoff should swallow AddJobEvent errors, got: %v", err)
	}
	if !failed {
		t.Error("failed = false, want true for the terminal transition")
	}
	if len(st.failedCalls) != 1 {
		t.Errorf("MarkJobFailed should still be called, got %v", st.failedCalls)
	}
}
