package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/store"
)

// storeErrJobImporting aliases store.ErrJobImporting at package scope: most
// test funcs below shadow the store package name with a local `store :=
// &fakeJobStore{...}` variable, so store.ErrJobImporting is unreachable
// inside them.
var storeErrJobImporting = store.ErrJobImporting

// fakeJobStore is a JobStore fake. Cancellation preparation returns the same
// all-transfer work list that the production transaction captures. lookupIDs
// records JobWithTransfer readbacks for manual-job creation assertions.
type fakeJobStore struct {
	jobs             map[int64]core.JobView
	barrierTransfers map[int64][]core.Transfer

	jobErr           error
	cancelErr        error
	cancelFound      bool
	prepareDeleteErr error
	prepareFound     bool
	retryErr         error
	retryOK          bool
	retryManualErr   error
	retryManualOK    bool
	createErr        error
	createJob        core.AlbumJob
	forceSearchErr   error
	forceSearchOK    bool
	deleteErr        error
	deleteOK         bool

	cancelCalled      bool
	cancelJobID       int64
	prepareCalled     bool
	retryCalled       bool
	retryManualCalled bool
	forceSearchCalled bool
	deleteCalled      bool
	lookupIDs         []int64
	operations        *[]string
	createCalled      struct {
		title, artistName, peer, albumMBID string
		files                              []store.ManualJobFile
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

func (f *fakeJobStore) CancelJob(ctx context.Context, jobID int64, now time.Time) ([]core.Transfer, bool, error) {
	f.cancelCalled = true
	f.cancelJobID = jobID
	if f.operations != nil {
		*f.operations = append(*f.operations, "barrier")
	}
	if f.cancelErr != nil {
		return nil, false, f.cancelErr
	}
	return f.barrierTransfers[jobID], f.cancelFound, nil
}

func (f *fakeJobStore) PrepareDeleteJob(ctx context.Context, jobID int64, now time.Time) ([]core.Transfer, bool, error) {
	f.prepareCalled = true
	if f.operations != nil {
		*f.operations = append(*f.operations, "barrier")
	}
	if f.prepareDeleteErr != nil {
		return nil, false, f.prepareDeleteErr
	}
	return f.barrierTransfers[jobID], f.prepareFound, nil
}

func (f *fakeJobStore) RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	f.retryCalled = true
	if f.retryErr != nil {
		return false, f.retryErr
	}
	return f.retryOK, nil
}

func (f *fakeJobStore) RetryManualJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	f.retryManualCalled = true
	if f.retryManualErr != nil {
		return false, f.retryManualErr
	}
	return f.retryManualOK, nil
}

func (f *fakeJobStore) CreateManualJob(ctx context.Context, title, artistName, peer, albumMBID string, files []store.ManualJobFile, now time.Time) (core.AlbumJob, error) {
	f.createCalled.title, f.createCalled.artistName, f.createCalled.peer, f.createCalled.albumMBID, f.createCalled.files = title, artistName, peer, albumMBID, files
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
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete")
	}
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return f.deleteOK, nil
}

type peerCancelCall struct {
	username string
	id       string
}

// fakePeerCanceller records every identity pair and can fail selected remote
// IDs, allowing tests to prove that a middle failure does not stop iteration.
type fakePeerCanceller struct {
	errors     map[string]error
	calls      []peerCancelCall
	operations *[]string
}

func (f *fakePeerCanceller) Cancel(ctx context.Context, username, id string) error {
	f.calls = append(f.calls, peerCancelCall{username: username, id: id})
	if f.operations != nil {
		*f.operations = append(*f.operations, "remote:"+id)
	}
	return f.errors[id]
}

func TestJobsCancelNotFound(t *testing.T) {
	store := &fakeJobStore{}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Cancel(context.Background(), 42); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Cancel() = %v, want ErrJobNotFound", err)
	}
	if !store.cancelCalled || len(peers.calls) != 0 {
		t.Errorf("not-found cancel: barrier=%v remote=%v", store.cancelCalled, peers.calls)
	}
}

func TestJobsCancelBarrierErrorStopsRemoteCancellation(t *testing.T) {
	barrierErr := errors.New("atomic cancel failed")
	store := &fakeJobStore{
		barrierTransfers: map[int64][]core.Transfer{1: {{ID: 1, Username: "bob", SlskdID: "g1"}}},
		cancelErr:        barrierErr,
	}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Cancel(context.Background(), 1); !errors.Is(err, barrierErr) {
		t.Fatalf("Cancel() = %v, want barrier error", err)
	}
	if len(peers.calls) != 0 {
		t.Fatalf("remote cancellation ran before failed barrier: %v", peers.calls)
	}
}

func TestJobsCancelCommitsBarrierBeforeAllRemoteAttempts(t *testing.T) {
	operations := []string{}
	store := &fakeJobStore{
		barrierTransfers: map[int64][]core.Transfer{7: {
			{ID: 10, Username: "pending-peer", State: core.TransferPending},
			{ID: 11, Username: "alice", SlskdID: "g1", State: core.TransferQueued},
			{ID: 12, Username: "bob", SlskdID: "g2", State: core.TransferInProgress},
			{ID: 13, Username: "carol", SlskdID: "g3", State: core.TransferStalled},
		}},
		cancelFound: true,
		operations:  &operations,
	}
	peers := &fakePeerCanceller{
		errors:     map[string]error{"g2": errors.New("peer unreachable")},
		operations: &operations,
	}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Cancel(context.Background(), 7); err != nil {
		t.Fatalf("Cancel() = %v, want nil", err)
	}
	wantCalls := []peerCancelCall{{username: "alice", id: "g1"}, {username: "bob", id: "g2"}, {username: "carol", id: "g3"}}
	if !reflect.DeepEqual(peers.calls, wantCalls) {
		t.Errorf("remote calls = %#v, want %#v", peers.calls, wantCalls)
	}
	if !store.cancelCalled || store.cancelJobID != 7 {
		t.Errorf("CancelJob call = called %v job %d, want true/7", store.cancelCalled, store.cancelJobID)
	}
	wantOperations := []string{"barrier", "remote:g1", "remote:g2", "remote:g3"}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Errorf("operation order = %v, want %v", operations, wantOperations)
	}
}

func TestJobsCancelSkipsEmptyRemoteIDAfterBarrier(t *testing.T) {
	store := &fakeJobStore{
		barrierTransfers: map[int64][]core.Transfer{1: {{ID: 1, Username: "bob", State: core.TransferPending}}},
		cancelFound:      true,
	}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Cancel(context.Background(), 1); err != nil {
		t.Fatalf("Cancel() = %v, want nil", err)
	}
	if len(peers.calls) != 0 || !store.cancelCalled {
		t.Errorf("empty remote ID handling: calls=%v barrier=%v", peers.calls, store.cancelCalled)
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

// TestJobsRetryRoutesBySource covers issue #347: Retry must call
// RetryManualJob (not RetryFailedJob) for a manual job, and vice versa for a
// lidarr job, using the Source the initial JobWithTransfer lookup already
// carries.
func TestJobsRetryRoutesBySource(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source core.JobSource
	}{
		{"manual", core.SourceManual},
		{"lidarr", core.SourceLidarr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeJobStore{
				jobs:          map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, Source: tt.source}}},
				retryOK:       true,
				retryManualOK: true,
			}
			j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

			if err := j.Retry(context.Background(), 1); err != nil {
				t.Fatalf("Retry() = %v, want nil", err)
			}
			if tt.source == core.SourceManual {
				if !store.retryManualCalled {
					t.Error("expected RetryManualJob called for a manual job")
				}
				if store.retryCalled {
					t.Error("expected RetryFailedJob NOT called for a manual job")
				}
			} else {
				if !store.retryCalled {
					t.Error("expected RetryFailedJob called for a lidarr job")
				}
				if store.retryManualCalled {
					t.Error("expected RetryManualJob NOT called for a lidarr job")
				}
			}
		})
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

	got, err := j.Create(context.Background(), "Requested Title", "Requested Artist", "requested_peer", "a1b2c3d4-e5f6-4789-a012-3456789abcde", files)
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
	if store.createCalled.albumMBID != "a1b2c3d4-e5f6-4789-a012-3456789abcde" {
		t.Errorf("CreateManualJob albumMBID = %q, want a1b2c3d4-e5f6-4789-a012-3456789abcde", store.createCalled.albumMBID)
	}
	if len(store.createCalled.files) != 1 || store.createCalled.files[0] != files[0] {
		t.Errorf("CreateManualJob files = %+v, want %+v", store.createCalled.files, files)
	}
}

func TestJobsCreateReadbackError(t *testing.T) {
	lookupErr := errors.New("readback failed")
	fakeStore := &fakeJobStore{createJob: core.AlbumJob{ID: 42}, jobErr: lookupErr}
	j := &Jobs{Store: fakeStore, Peers: &fakePeerCanceller{}}

	_, err := j.Create(context.Background(), "Title", "Artist", "peer", "", []store.ManualJobFile{{Filename: "track.flac", Size: 1}})
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

	_, err := j.Create(context.Background(), "Title", "Artist", "peer", "", []store.ManualJobFile{{Filename: "track.flac", Size: 1}})
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

			_, err := j.Create(context.Background(), "Title", "Artist", "peer", "", []store.ManualJobFile{{Filename: "track.flac", Size: 1}})
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

// TestJobsForceSearchManualJob covers issue #347: a manual job cannot be
// force-searched (it has no lidarr_album_id to search for), and the rejection
// must happen before the store call - store.ForceSearchJob has no source
// guard of its own and would delete the candidate unconditionally.
func TestJobsForceSearchManualJob(t *testing.T) {
	store := &fakeJobStore{jobs: map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, Source: core.SourceManual, State: core.StateFailed}}}}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.ForceSearch(context.Background(), 1); !errors.Is(err, ErrJobNotSearchable) {
		t.Fatalf("ForceSearch() = %v, want ErrJobNotSearchable", err)
	}
	if store.forceSearchCalled {
		t.Errorf("ForceSearchJob must not be called for a manual job")
	}
}

// TestJobsForceSearchLidarrJobUnaffected guards the routing the other way: a
// lidarr-sourced job's ForceSearch behaviour must be unchanged by the manual-
// job guard above.
func TestJobsForceSearchLidarrJobUnaffected(t *testing.T) {
	store := &fakeJobStore{
		jobs:          map[int64]core.JobView{1: {Job: core.AlbumJob{ID: 1, Source: core.SourceLidarr, State: core.StateFailed}}},
		forceSearchOK: true,
	}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.ForceSearch(context.Background(), 1); err != nil {
		t.Fatalf("ForceSearch() = %v, want nil", err)
	}
	if !store.forceSearchCalled {
		t.Errorf("expected ForceSearchJob to have been called for a lidarr job")
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
	store := &fakeJobStore{}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 42); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Delete() = %v, want ErrJobNotFound", err)
	}
	if !store.prepareCalled || store.deleteCalled || len(peers.calls) != 0 {
		t.Fatalf("not-found delete: prepare=%v delete=%v remote=%v", store.prepareCalled, store.deleteCalled, peers.calls)
	}
}

func TestJobsDeleteImportingRejectedByPreparation(t *testing.T) {
	store := &fakeJobStore{prepareDeleteErr: storeErrJobImporting}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, ErrJobImporting) {
		t.Fatalf("Delete() = %v, want ErrJobImporting", err)
	}
	if store.deleteCalled || len(peers.calls) != 0 {
		t.Fatalf("IMPORTING preparation performed later work: delete=%v remote=%v", store.deleteCalled, peers.calls)
	}
}

func TestJobsDeleteBarrierThenAllRemoteTransfersThenHardDelete(t *testing.T) {
	operations := []string{}
	store := &fakeJobStore{
		barrierTransfers: map[int64][]core.Transfer{7: {
			{ID: 10, Username: "pending-peer"},
			{ID: 11, Username: "alice", SlskdID: "g1"},
			{ID: 12, Username: "bob", SlskdID: "g2"},
			{ID: 13, Username: "carol", SlskdID: "g3"},
		}},
		prepareFound: true,
		deleteOK:     true,
		operations:   &operations,
	}
	peers := &fakePeerCanceller{
		errors:     map[string]error{"g2": errors.New("peer unreachable")},
		operations: &operations,
	}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 7); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
	wantCalls := []peerCancelCall{{username: "alice", id: "g1"}, {username: "bob", id: "g2"}, {username: "carol", id: "g3"}}
	if !reflect.DeepEqual(peers.calls, wantCalls) {
		t.Errorf("remote calls = %#v, want %#v", peers.calls, wantCalls)
	}
	wantOperations := []string{"barrier", "remote:g1", "remote:g2", "remote:g3", "delete"}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Errorf("operation order = %v, want %v", operations, wantOperations)
	}
}

func TestJobsDeleteHardDeleteFailureLeavesPreparedCancellation(t *testing.T) {
	deleteErr := errors.New("hard delete failed")
	store := &fakeJobStore{
		barrierTransfers: map[int64][]core.Transfer{1: {{ID: 1, Username: "bob", SlskdID: "g1"}}},
		prepareFound:     true,
		deleteErr:        deleteErr,
	}
	peers := &fakePeerCanceller{}
	j := &Jobs{Store: store, Peers: peers}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, deleteErr) {
		t.Fatalf("Delete() = %v, want hard-delete error", err)
	}
	if !store.prepareCalled || !store.deleteCalled || len(peers.calls) != 1 {
		t.Fatalf("failure semantics: prepare=%v remote=%v delete=%v", store.prepareCalled, peers.calls, store.deleteCalled)
	}
	// The production PrepareDeleteJob has already committed CANCELLED here;
	// Delete deliberately does not attempt to roll that barrier back.
}

func TestJobsDeleteRacedIntoNotFoundAfterPreparation(t *testing.T) {
	store := &fakeJobStore{prepareFound: true, deleteOK: false}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Delete() = %v, want ErrJobNotFound", err)
	}
}

func TestJobsDeleteRacedIntoImportingAfterPreparation(t *testing.T) {
	store := &fakeJobStore{prepareFound: true, deleteErr: storeErrJobImporting}
	j := &Jobs{Store: store, Peers: &fakePeerCanceller{}}

	if err := j.Delete(context.Background(), 1); !errors.Is(err, ErrJobImporting) {
		t.Fatalf("Delete() = %v, want ErrJobImporting", err)
	}
}
