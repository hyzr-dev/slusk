package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// storeErrJobImporting aliases store.ErrJobImporting at package scope: most
// test funcs below shadow the store package name with a local `store :=
// &fakeJobStore{...}` variable, so store.ErrJobImporting is unreachable
// inside them.
var storeErrJobImporting = store.ErrJobImporting

// fakeJobStore is a JobStore fake. jobs, when set, is looked up by
// JobWithTransfer; a missing id reports not-found. advanceErr/retryErr, when
// set, fail the corresponding call. advancedTo/retryCalled record what was
// actually invoked so tests can assert on it. createErr/createJob configure
// CreateManualJob's result; createCalled records its args for assertions, and
// lookupIDs records JobWithTransfer readbacks.
type fakeJobStore struct {
	jobs map[int64]core.JobView

	jobErr         error
	advanceErr     error
	retryErr       error
	retryOK        bool
	createErr      error
	createJob      core.AlbumJob
	forceSearchErr error
	forceSearchOK  bool
	deleteErr      error
	deleteOK       bool

	advancedTo        core.AlbumJobState
	retryCalled       bool
	forceSearchCalled bool
	deleteCalled      bool
	lookupIDs         []int64
	createCalled      struct {
		title, artistName, peer string
		files                   []store.ManualJobFile
	}
}

func (f *fakeJobStore) JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error) {
	f.lookupIDs = append(f.lookupIDs, jobID)
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

func (f *fakeJobStore) CreateManualJob(ctx context.Context, title, artistName, peer string, files []store.ManualJobFile, now time.Time) (core.AlbumJob, error) {
	f.createCalled.title, f.createCalled.artistName, f.createCalled.peer, f.createCalled.files = title, artistName, peer, files
	if f.createErr != nil {
		return core.AlbumJob{}, f.createErr
	}
	return f.createJob, nil
}

func (f *fakeJobStore) ForceSearchJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	f.forceSearchCalled = true
	if f.forceSearchErr != nil {
		return false, f.forceSearchErr
	}
	return f.forceSearchOK, nil
}

func (f *fakeJobStore) DeleteJob(ctx context.Context, jobID int64) (bool, error) {
	f.deleteCalled = true
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return f.deleteOK, nil
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

func TestJobsCreateReturnsCanonicalView(t *testing.T) {
	files := []store.ManualJobFile{{Filename: "track.flac", Size: 111}}
	want := core.JobView{
		Job:             core.AlbumJob{ID: 42, Title: "Persisted Title", Source: core.SourceManual},
		Peer:            "persisted_peer",
		AlbumBytesDone:  0,
		AlbumBytesTotal: 111,
	}
	store := &fakeJobStore{
		createJob: core.AlbumJob{ID: 42},
		jobs:      map[int64]core.JobView{42: want},
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	got, err := j.Create(context.Background(), "Requested Title", "Requested Artist", "requested_peer", files)
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if got.Job.ID != want.Job.ID || got.Job.Title != want.Job.Title || got.Peer != want.Peer || got.AlbumBytesTotal != want.AlbumBytesTotal {
		t.Errorf("Create() = %+v, want canonical view %+v", got, want)
	}
	if len(store.lookupIDs) != 1 || store.lookupIDs[0] != 42 {
		t.Errorf("JobWithTransfer lookup IDs = %v, want [42]", store.lookupIDs)
	}
	if store.createCalled.title != "Requested Title" || store.createCalled.artistName != "Requested Artist" || store.createCalled.peer != "requested_peer" {
		t.Errorf("CreateManualJob args = title %q, artist %q, peer %q", store.createCalled.title, store.createCalled.artistName, store.createCalled.peer)
	}
	if len(store.createCalled.files) != 1 || store.createCalled.files[0] != files[0] {
		t.Errorf("CreateManualJob files = %+v, want %+v", store.createCalled.files, files)
	}
}

func TestJobsCreateReadbackError(t *testing.T) {
	lookupErr := errors.New("readback failed")
	fakeStore := &fakeJobStore{createJob: core.AlbumJob{ID: 42}, jobErr: lookupErr}
	j := &Jobs{Store: fakeStore, Peers: &fakePeerCanceller{}}

	_, err := j.Create(context.Background(), "Title", "Artist", "peer", []store.ManualJobFile{{Filename: "track.flac", Size: 1}})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("Create() = %v, want readback error", err)
	}
	if len(fakeStore.lookupIDs) != 1 || fakeStore.lookupIDs[0] != 42 {
		t.Errorf("JobWithTransfer lookup IDs = %v, want [42]", fakeStore.lookupIDs)
	}
}

func TestJobsCreateReadbackNotFound(t *testing.T) {
	fakeStore := &fakeJobStore{createJob: core.AlbumJob{ID: 42}, jobs: map[int64]core.JobView{}}
	j := &Jobs{Store: fakeStore, Peers: &fakePeerCanceller{}}

	_, err := j.Create(context.Background(), "Title", "Artist", "peer", []store.ManualJobFile{{Filename: "track.flac", Size: 1}})
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Create() = %v, want ErrJobNotFound", err)
	}
	if len(fakeStore.lookupIDs) != 1 || fakeStore.lookupIDs[0] != 42 {
		t.Errorf("JobWithTransfer lookup IDs = %v, want [42]", fakeStore.lookupIDs)
	}
}

func TestJobsCreateFailureSkipsReadback(t *testing.T) {
	tests := []struct {
		name     string
		storeErr error
		wantErr  error
	}{
		{name: "create error", storeErr: errors.New("create failed")},
		{name: "remote file busy", storeErr: store.ErrRemoteFileBusy, wantErr: ErrRemoteFileBusy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := &fakeJobStore{createErr: tt.storeErr}
			j := &Jobs{Store: fakeStore, Peers: &fakePeerCanceller{}}

			_, err := j.Create(context.Background(), "Title", "Artist", "peer", []store.ManualJobFile{{Filename: "track.flac", Size: 1}})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Create() = %v, want %v", err, tt.wantErr)
				}
			} else if !errors.Is(err, tt.storeErr) {
				t.Fatalf("Create() = %v, want creation error", err)
			}
			if len(fakeStore.lookupIDs) != 0 {
				t.Errorf("JobWithTransfer called after failed creation with IDs %v", fakeStore.lookupIDs)
			}
		})
	}
}

func TestJobsForceSearchNotFound(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{}}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.ForceSearch(context.Background(), 99); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("ForceSearch() = %v, want ErrJobNotFound", err)
	}
	if store.forceSearchCalled {
		t.Errorf("ForceSearchJob must not be called for a job that doesn't exist")
	}
}

// TestJobsForceSearchActiveState covers the fast path: the initial lookup
// already shows DOWNLOADING/IMPORTING, so the store is never called.
func TestJobsForceSearchActiveState(t *testing.T) {
	for _, state := range []core.AlbumJobState{core.StateDownloading, core.StateImporting} {
		t.Run(string(state), func(t *testing.T) {
			store := &fakeJobStore{jobs: map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, State: state}}}}
			j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

			if err := j.ForceSearch(context.Background(), 1); !errors.Is(err, ErrJobActive) {
				t.Fatalf("ForceSearch() = %v, want ErrJobActive", err)
			}
			if store.forceSearchCalled {
				t.Errorf("ForceSearchJob must not be called when the job is already known to be active")
			}
		})
	}
}

// TestJobsForceSearchRacedIntoActive covers the slow path: the initial lookup
// showed an inactive state, but the store's guarded UPDATE lost the race to a
// concurrent transition into DOWNLOADING/IMPORTING.
func TestJobsForceSearchRacedIntoActive(t *testing.T) {
	store := &fakeJobStore{
		jobs:          map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, State: core.StateFailed}}},
		forceSearchOK: false,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.ForceSearch(context.Background(), 1); !errors.Is(err, ErrJobActive) {
		t.Fatalf("ForceSearch() = %v, want ErrJobActive", err)
	}
	if !store.forceSearchCalled {
		t.Errorf("expected ForceSearchJob to have been called")
	}
}

func TestJobsForceSearchSuccess(t *testing.T) {
	store := &fakeJobStore{
		jobs:          map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, State: core.StateFailed}}},
		forceSearchOK: true,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.ForceSearch(context.Background(), 1); err != nil {
		t.Fatalf("ForceSearch() = %v, want nil", err)
	}
}

func TestJobsForceSearchLookupError(t *testing.T) {
	store := &fakeJobStore{jobErr: errors.New("db down")}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	err := j.ForceSearch(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrJobActive) {
		t.Fatalf("ForceSearch() = %v, want the underlying store error", err)
	}
}

func TestJobsForceSearchStoreError(t *testing.T) {
	store := &fakeJobStore{
		jobs:           map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, State: core.StateFailed}}},
		forceSearchErr: errors.New("db exploded"),
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	err := j.ForceSearch(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrJobActive) {
		t.Fatalf("ForceSearch() = %v, want the wrapped store error", err)
	}
}

func TestJobsDeleteNotFound(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{}}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 42); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Delete() = %v, want ErrJobNotFound", err)
	}
	if peers.called {
		t.Errorf("Delete must not touch the remote peer for a job that doesn't exist")
	}
	if store.deleteCalled {
		t.Errorf("DeleteJob must not be called for a job that doesn't exist")
	}
}

func TestJobsDeleteImporting(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, State: core.StateImporting}}}}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, ErrJobImporting) {
		t.Fatalf("Delete() = %v, want ErrJobImporting", err)
	}
	if store.deleteCalled {
		t.Errorf("DeleteJob must not be called once the fast-path IMPORTING check already refused")
	}
}

// TestJobsDeleteRemoteCancelBestEffort ensures a failed remote cancel does
// not block the delete: the job is still removed locally, matching Cancel's
// best-effort semantics.
func TestJobsDeleteRemoteCancelBestEffort(t *testing.T) {
	store := &fakeJobStore{
		jobs: map[int64]core.JobView{
			7: {
				Job:      core.AlbumJob{ID: 7, State: core.StateDownloading},
				Transfer: &core.Transfer{Username: "bob", SlskdID: "g1"},
			},
		},
		deleteOK: true,
	}
	peers := &fakePeerCanceller{cancelErr: errors.New("slskd unreachable")}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 7); err != nil {
		t.Fatalf("Delete() = %v, want nil (remote failure must not block delete)", err)
	}
	if !peers.called {
		t.Errorf("expected the remote cancel to have been attempted")
	}
	if !store.deleteCalled {
		t.Errorf("expected DeleteJob to have been called")
	}
}

func TestJobsDeleteSkipsRemoteWhenNoLiveTransfer(t *testing.T) {
	store := &fakeJobStore{
		jobs:     map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		deleteOK: true,
	}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 1); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
	if peers.called {
		t.Errorf("Delete must skip the remote call when there is no live SlskdID")
	}
}

func TestJobsDeleteSuccess(t *testing.T) {
	store := &fakeJobStore{
		jobs:     map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		deleteOK: true,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Delete(context.Background(), 1); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
}

// TestJobsDeleteRacedIntoNotFound covers the store's FOR UPDATE re-check
// returning (false, nil) - the job was deleted or vanished between the app's
// lookup and the store's atomic delete.
func TestJobsDeleteRacedIntoNotFound(t *testing.T) {
	store := &fakeJobStore{
		jobs:     map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		deleteOK: false,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Delete() = %v, want ErrJobNotFound", err)
	}
}

// TestJobsDeleteRacedIntoImporting covers the store's FOR UPDATE re-check
// discovering IMPORTING after the app's fast-path check already passed (the
// job transitioned in between).
func TestJobsDeleteRacedIntoImporting(t *testing.T) {
	store := &fakeJobStore{
		jobs:      map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		deleteErr: storeErrJobImporting,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, ErrJobImporting) {
		t.Fatalf("Delete() = %v, want ErrJobImporting", err)
	}
}

func TestJobsDeleteLookupError(t *testing.T) {
	store := &fakeJobStore{jobErr: errors.New("db down")}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	err := j.Delete(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrJobImporting) {
		t.Fatalf("Delete() = %v, want the underlying store error", err)
	}
}

func TestJobsDeleteStoreError(t *testing.T) {
	store := &fakeJobStore{
		jobs:      map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1}}},
		deleteErr: errors.New("db exploded"),
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	err := j.Delete(context.Background(), 1)
	if err == nil || errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrJobImporting) {
		t.Fatalf("Delete() = %v, want the wrapped store error", err)
	}
}
