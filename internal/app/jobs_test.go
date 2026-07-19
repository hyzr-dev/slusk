package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// fakeJobStore is a JobStore fake. jobs, when set, is looked up by
// JobWithTransfer; a missing id reports not-found. advanceErr/retryErr, when
// set, fail the corresponding call. advancedTo/retryCalled record what was
// actually invoked so tests can assert on it.
type fakeJobStore struct {
	jobs map[int64]core.JobView

	jobErr     error
	advanceErr error
	retryErr   error
	retryOK    bool

	advancedTo  core.AlbumJobState
	retryCalled bool
}

func (f *fakeJobStore) JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error) {
	if f.jobErr != nil {
		return core.JobView{}, false, f.jobErr
	}
	v, ok := f.jobs[jobID]
	return v, ok, nil
}

func (f *fakeJobStore) AdvanceJobState(ctx context.Context, jobID int64, to core.AlbumJobState, now time.Time) error {
	if f.advanceErr != nil {
		return f.advanceErr
	}
	f.advancedTo = to
	return nil
}

func (f *fakeJobStore) RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	f.retryCalled = true
	if f.retryErr != nil {
		return false, f.retryErr
	}
	return f.retryOK, nil
}

// fakePeerCanceller is a TransferCanceller fake. cancelErr, when set, fails
// every Cancel call; called records whether Cancel was invoked at all.
type fakePeerCanceller struct {
	cancelErr error
	called    bool
	username  string
	id        string
}

func (f *fakePeerCanceller) Cancel(ctx context.Context, username, id string) error {
	f.called = true
	f.username, f.id = username, id
	if f.cancelErr != nil {
		return f.cancelErr
	}
	return nil
}

func TestJobsCancelNotFound(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{}}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Cancel(context.Background(), 42); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Cancel() = %v, want ErrJobNotFound", err)
	}
	if peers.called {
		t.Errorf("Cancel must not touch the remote peer for a job that doesn't exist")
	}
}

func TestJobsCancelStoreLookupError(t *testing.T) {
	store := &fakeJobStore{jobErr: errors.New("db down")}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	err := j.Cancel(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Cancel() = %v, want the underlying store error", err)
	}
}

// A failed remote cancel must not block the local state transition: the job
// still advances to CANCELLED, and any stale slskd-side entry is left for the
// next reconcile pass to clean up.
func TestJobsCancelRemoteFailureStillAdvancesLocally(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{
		7: {
			Job:      core.AlbumJob{ID: 7},
			Transfer: &core.Transfer{Username: "bob", SlskdID: "g1"},
		},
	}}
	peers := &fakePeerCanceller{cancelErr: errors.New("slskd unreachable")}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Cancel(context.Background(), 7); err != nil {
		t.Fatalf("Cancel() = %v, want nil (remote failure must not block local advance)", err)
	}
	if !peers.called {
		t.Errorf("expected the remote cancel to have been attempted")
	}
	if store.advancedTo != core.StateCancelled {
		t.Errorf("advancedTo = %v, want StateCancelled", store.advancedTo)
	}
}

func TestJobsCancelStoreAdvanceError(t *testing.T) {
	store := &fakeJobStore{
		jobs:       map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		advanceErr: errors.New("advance failed"),
	}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	err := j.Cancel(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Cancel() = %v, want the underlying store error", err)
	}
}

// A transfer with no SlskdID yet (enqueue never returned one) or no transfer
// at all must skip the remote call entirely - there is nothing in slskd to
// cancel.
func TestJobsCancelSkipsRemoteWhenNoLiveTransfer(t *testing.T) {
	tests := []struct {
		name string
		view core.JobView
	}{
		{name: "nil transfer", view: core.JobView{Job: core.AlbumJob{ID: 1}, Transfer: nil}},
		{name: "empty SlskdID", view: core.JobView{Job: core.AlbumJob{ID: 1}, Transfer: &core.Transfer{Username: "bob", SlskdID: ""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeJobStore{jobs: map[int64]core.JobView{1: tt.view}}
			peers := &fakePeerCanceller{}
			j := &Jobs{Store: store, Peers: peers}

			if err := j.Cancel(context.Background(), 1); err != nil {
				t.Fatalf("Cancel() = %v, want nil", err)
			}
			if peers.called {
				t.Errorf("Cancel must skip the remote call when there is no live SlskdID")
			}
			if store.advancedTo != core.StateCancelled {
				t.Errorf("advancedTo = %v, want StateCancelled", store.advancedTo)
			}
		})
	}
}

func TestJobsRetryNotFound(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{}}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Retry(context.Background(), 99); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Retry() = %v, want ErrJobNotFound", err)
	}
	if store.retryCalled {
		t.Errorf("RetryFailedJob must not be called for a job that doesn't exist")
	}
}

func TestJobsRetryConflictWhenNotFailed(t *testing.T) {
	store := &fakeJobStore{
		jobs:    map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		retryOK: false,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Retry(context.Background(), 1); !errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("Retry() = %v, want ErrJobNotRetryable", err)
	}
}

func TestJobsRetrySuccess(t *testing.T) {
	store := &fakeJobStore{
		jobs:    map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		retryOK: true,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Retry(context.Background(), 1); err != nil {
		t.Fatalf("Retry() = %v, want nil", err)
	}
}

func TestJobsRetryLookupError(t *testing.T) {
	store := &fakeJobStore{jobErr: errors.New("db down")}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	err := j.Retry(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("Retry() = %v, want the underlying store error", err)
	}
}

func TestJobsRetryStoreError(t *testing.T) {
	store := &fakeJobStore{
		jobs:     map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		retryErr: errors.New("db exploded"),
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	err := j.Retry(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("Retry() = %v, want the wrapped store error", err)
	}
}
