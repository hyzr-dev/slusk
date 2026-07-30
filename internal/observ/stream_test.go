package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// --- pure function tests ---------------------------------------------------

// buildJobsDelta must produce whole jobDTOs built by the same toJobDTO REST
// uses, only for jobs whose jobDTO differs from what this subscriber was
// last sent (issue #258/#268): a job's presence in the delta tracks whether
// its DTO changed, not whether it's still "relevant" — a scoped
// subscriber's set (sub.jobIDs) never changes for the life of the
// connection (issue #268), so there is no "leaving the set" case to
// correct for; see buildJobsDelta's doc comment.
func TestBuildJobsDeltaOnlyChangedJobsSent(t *testing.T) {
	view := core.JobView{
		Job:                 core.AlbumJob{ID: 7, Title: "Rounds"},
		Attempt:             &core.Candidate{ID: 1, Username: "alice", Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}},
		AlbumBytesTotal:     1000,
		AlbumBytesRemaining: 1000,
	}
	views := map[int64]core.JobView{7: view}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	withLive := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50, BytesDone: 300},
	})
	first := buildJobsDelta(sub, views, withLive, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 || first[0].Speed != 50 {
		t.Fatalf("first tick: expected job 7 at speed 50, got %+v", first)
	}

	// Unchanged: no live data change, nothing rebuilt differently -> empty delta.
	second := buildJobsDelta(sub, views, withLive, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(second) != 0 {
		t.Fatalf("unchanged tick must produce an empty delta, got %+v", second)
	}

	// The transfer's speed changes -> job 7 is still in sub.jobIDs (it never
	// leaves), but its dto differs from what was last sent, so it's included
	// again.
	changed := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 75, BytesDone: 300},
	})
	third := buildJobsDelta(sub, views, changed, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(third) != 1 || third[0].ID != 7 || third[0].Speed != 75 {
		t.Fatalf("changed tick: expected job 7 at speed 75, got %+v", third)
	}
}

// TestBuildJobsDeltaSameNowUnchangedJobStaysOmitted proves that FramedAt
// being part of jobDTO doesn't defeat buildJobsDelta's reflect.DeepEqual
// change-detection: passing the exact same now on two consecutive ticks for
// an otherwise-unchanged job must not resend it (issue #285).
func TestBuildJobsDeltaSameNowUnchangedJobStaysOmitted(t *testing.T) {
	views := map[int64]core.JobView{
		7: {Job: core.AlbumJob{ID: 7, Title: "Rounds"}},
	}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	first := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 {
		t.Fatalf("first tick: expected job 7, got %+v", first)
	}

	second := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(second) != 0 {
		t.Fatalf("same now, unchanged job: expected an empty delta, got %+v", second)
	}
}

// TestBuildJobsDeltaDifferentNowResendsUnchangedJob is the converse: two
// different now values on an otherwise-identical job must resend it — a
// FramedAt-only refresh is exactly what should re-freshen the client's
// freshness signal (issue #285), not something the delta should suppress.
func TestBuildJobsDeltaDifferentNowResendsUnchangedJob(t *testing.T) {
	views := map[int64]core.JobView{
		7: {Job: core.AlbumJob{ID: 7, Title: "Rounds"}},
	}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	first := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 {
		t.Fatalf("first tick: expected job 7, got %+v", first)
	}

	later := testNow.Add(time.Second)
	second := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, later)
	if len(second) != 1 || second[0].ID != 7 || second[0].FramedAt != later.Format(timeFormat) {
		t.Fatalf("different now: expected job 7 resent with the new FramedAt, got %+v", second)
	}
}

// A scoped (?jobs=) subscriber must receive an entry for every requested id
// present in viewByJob, live-matched or not — sub.jobIDs is the relevant set
// outright (issue #268), not filtered by live match.
func TestBuildJobsDeltaScopedIncludesNonLiveMatchedRequestedIDs(t *testing.T) {
	views := map[int64]core.JobView{
		1: {Job: core.AlbumJob{ID: 1, Title: "Queued job"}},
	}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{1: {}}}
	got := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("expected the requested job even with no candidate/live match, got %+v", got)
	}
}

// TestBuildJobsDeltaEmptyForUnscopedSubscriber is issue #268's explicit
// decision: a subscriber that never sent ?jobs= gets no job frames at all,
// even when jobs are live-matched — every job-list surface now knows its own
// page and must publish it via ?jobs=.
func TestBuildJobsDeltaEmptyForUnscopedSubscriber(t *testing.T) {
	views := map[int64]core.JobView{
		7: {
			Job:     core.AlbumJob{ID: 7},
			Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}},
		},
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50},
	})
	sub := &streamSubscriber{} // no jobIDs
	got := buildJobsDelta(sub, views, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(got) != 0 {
		t.Fatalf("expected no job frames for an unscoped subscriber, got %+v", got)
	}
}

func TestMergeJobsDeltaUnionsByIDNewerWins(t *testing.T) {
	old := []jobDTO{{ID: 1, BytesDone: 100}, {ID: 2, BytesDone: 200}}
	next := []jobDTO{{ID: 2, BytesDone: 250}, {ID: 3, BytesDone: 300}}
	got := mergeJobsDelta(old, next)
	want := map[int64]int64{1: 100, 2: 250, 3: 300}
	if len(got) != len(want) {
		t.Fatalf("mergeJobsDelta(...) = %+v, want 3 entries", got)
	}
	for _, dto := range got {
		if bytesDone, ok := want[dto.ID]; !ok || bytesDone != dto.BytesDone {
			t.Errorf("job %d BytesDone = %d, want %d", dto.ID, dto.BytesDone, want[dto.ID])
		}
	}
}

func TestMergeJobsDeltaEmptyOldReturnsNextUnchanged(t *testing.T) {
	next := []jobDTO{{ID: 1}}
	got := mergeJobsDelta(nil, next)
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("mergeJobsDelta(nil, next) = %+v, want next unchanged", got)
	}
}

// buildStreamDetail must produce exactly what GET /api/jobs/{id}/detail
// produces for the same inputs — that identity is the whole point of #258's
// alternative A, since it is what lets the frontend replace rather than
// merge. Needs both a view (for the embedded jobDTO header, issue #268) and
// a detail to be cached.
func TestBuildStreamDetailMatchesRESTHandlerOutput(t *testing.T) {
	view := core.JobView{Job: core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet"}}
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "alice"},
			Transfers: []core.Transfer{
				{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 100, BytesTotal: 1000},
			},
		}},
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 400, Speed: 100},
	})

	got := buildStreamDetail(view, true, detail, true, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if got == nil {
		t.Fatal("expected a detail for a cached job")
	}
	want := toJobDetailDTO(view, detail, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("stream detail diverged from the REST handler's:\n got %+v\nwant %+v", *got, want)
	}
	// And the live overlay actually applied, so the comparison above is not
	// two identically-wrong values.
	if got.Attempts[0].Transfers[0].BytesDone != 400 {
		t.Errorf("BytesDone = %d, want 400 from live", got.Attempts[0].Transfers[0].BytesDone)
	}
}

// A job missing EITHER the cached detail or the cached view omits the field
// entirely rather than sending an incomplete object the frontend would
// mistake for "this job has no attempts" (issue #268: both are now needed).
func TestBuildStreamDetailUncachedIsNil(t *testing.T) {
	view := core.JobView{Job: core.AlbumJob{ID: 7}}
	detail := core.JobDetail{Job: core.AlbumJob{ID: 7}}
	if got := buildStreamDetail(view, false, detail, true, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow); got != nil {
		t.Errorf("expected nil when the view isn't cached, got %+v", got)
	}
	if got := buildStreamDetail(view, true, detail, false, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow); got != nil {
		t.Errorf("expected nil when the detail isn't cached, got %+v", got)
	}
	if got := buildStreamDetail(core.JobView{}, false, core.JobDetail{}, false, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow); got != nil {
		t.Errorf("expected nil when neither is cached, got %+v", got)
	}
}

func TestChangedSinceLastTableCases(t *testing.T) {
	detailA := &jobDetailDTO{Job: jobDTO{ID: 1}, Attempts: []attemptDetailDTO{{ID: 1, Transfers: []transferDetailDTO{{Filename: "a.flac", BytesDone: 10}}}}}
	detailB := &jobDetailDTO{Job: jobDTO{ID: 1}, Attempts: []attemptDetailDTO{{ID: 1, Transfers: []transferDetailDTO{{Filename: "a.flac", BytesDone: 20}}}}}

	cases := []struct {
		name                                          string
		prev, next                                    livePayload
		newJobCount, newDownloadCount, newUploadCount int
		want                                          bool
	}{
		{"nothing changed", livePayload{Detail: detailA, Down: 5, Up: 6}, livePayload{Detail: detailA, Down: 5, Up: 6}, 0, 0, 0, false},
		{"jobs changed", livePayload{}, livePayload{}, 1, 0, 0, true},
		{"down changed", livePayload{Down: 5}, livePayload{Down: 6}, 0, 0, 0, true},
		{"up changed", livePayload{Up: 5}, livePayload{Up: 6}, 0, 0, 0, true},
		{"detail nested transfer bytes changed", livePayload{Detail: detailA}, livePayload{Detail: detailB}, 0, 0, 0, true},
		{"detail appeared", livePayload{}, livePayload{Detail: detailA}, 0, 0, 0, true},
		{"only new download throughput", livePayload{Detail: detailA, Down: 5}, livePayload{Detail: detailA, Down: 5}, 0, 1, 0, true},
		{"only new upload throughput", livePayload{Detail: detailA, Up: 5}, livePayload{Detail: detailA, Up: 5}, 0, 0, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := changedSinceLast(tc.prev, tc.next, tc.newJobCount, tc.newDownloadCount, tc.newUploadCount)
			if got != tc.want {
				t.Errorf("changedSinceLast(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildLiveSnapshotDetailScopingByJobID(t *testing.T) {
	live := []core.RemoteTransfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 500}}
	idx := newLiveTransferIndex(live)
	view := core.JobView{Job: core.AlbumJob{ID: 7}}
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 7},
		Attempts: []core.AttemptDetail{{
			Attempt:   core.Candidate{ID: 1, Username: "alice"},
			Transfers: []core.Transfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesTotal: 1000}},
		}},
	}
	series := core.ThroughputSeries{
		Download: []core.ThroughputSample{{BytesPerSecond: 100}, {BytesPerSecond: 200}},
		Upload:   []core.ThroughputSample{{BytesPerSecond: 300}, {BytesPerSecond: 400}},
	}

	unscoped := buildLiveSnapshot(live, 0, series, view, true, detail, true, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if unscoped.Detail != nil {
		t.Errorf("expected nil Detail without ?job=, got %+v", unscoped.Detail)
	}

	scoped := buildLiveSnapshot(live, 7, series, view, true, detail, true, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if scoped.Detail == nil || len(scoped.Detail.Attempts) != 1 {
		t.Fatalf("expected a detail for ?job=7, got %+v", scoped.Detail)
	}
	if got := scoped.Detail.Attempts[0].Transfers[0].BytesDone; got != 500 {
		t.Errorf("BytesDone = %d, want 500 merged in from live", got)
	}
	if scoped.Down != 200 || scoped.Up != 400 || scoped.Down != unscoped.Down || scoped.Up != unscoped.Up {
		t.Errorf("global rates changed under ?job=: scoped down/up=%d/%d, unscoped=%d/%d", scoped.Down, scoped.Up, unscoped.Down, unscoped.Up)
	}

	// Scoped, but the hub has no cached detail (or view) for that id yet —
	// the field is omitted rather than sent empty, and the frontend keeps
	// its REST copy.
	missing := buildLiveSnapshot(live, 999, series, core.JobView{}, false, core.JobDetail{}, false, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if missing.Detail != nil {
		t.Errorf("expected nil Detail for an uncached job id, got %+v", missing.Detail)
	}
	if missing.Down != 200 || missing.Up != 400 {
		t.Errorf("missing scoped detail affected global rates: down/up=%d/%d", missing.Down, missing.Up)
	}
}

func TestLivePayloadExactBidirectionalJSON(t *testing.T) {
	// Jobs is deliberately left unset: it's delta-encoded (see buildJobsDelta)
	// and omitempty, so a frame with nothing job-related to report omits the
	// key entirely rather than emitting an empty array.
	payload := livePayload{
		Throughput:       []throughputSampleDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 200, ActiveTransfers: 2}},
		UploadThroughput: []throughputSampleDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 400, ActiveTransfers: 4}},
		Down:             200,
		Up:               400,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"throughput":[{"at":"2026-01-01T00:00:00Z","bytesPerSecond":200,"activeTransfers":2}],"uploadThroughput":[{"at":"2026-01-01T00:00:00Z","bytesPerSecond":400,"activeTransfers":4}],"down":200,"up":400}`
	if string(body) != want {
		t.Errorf("live JSON = %s\nwant      = %s", body, want)
	}
}

func TestStreamHubScopedSubscriptionKeepsGlobalRatesAndSeries(t *testing.T) {
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: at, BytesPerSecond: 200}},
			Upload:   []core.ThroughputSample{{At: at, BytesPerSecond: 400}},
		}, nil
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, noopJobDetail, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	unscopedID, _, unscoped := hub.subscribe(context.Background(), 0, nil)
	defer hub.unsubscribe(unscopedID)
	scopedID, _, scoped := hub.subscribe(context.Background(), 7, nil)
	defer hub.unsubscribe(scopedID)

	if scoped.Down != unscoped.Down || scoped.Up != unscoped.Up {
		t.Errorf("scoped rates = %d/%d, global = %d/%d", scoped.Down, scoped.Up, unscoped.Down, unscoped.Up)
	}
	if !reflect.DeepEqual(scoped.Throughput, unscoped.Throughput) || !reflect.DeepEqual(scoped.UploadThroughput, unscoped.UploadThroughput) {
		t.Errorf("scoped series differ from global: scoped=%+v/%+v global=%+v/%+v", scoped.Throughput, scoped.UploadThroughput, unscoped.Throughput, unscoped.UploadThroughput)
	}
}

// TestSendLatestReplacesRatesUnionsJobsAccumulatesThroughput asserts mailbox
// replacement: Down/Up (ordinary snapshot fields) are superseded outright,
// while Jobs (delta-encoded, unioned by id) and the two throughput series
// (delta-encoded, concatenated) both preserve what the superseded frame
// carried.
func TestSendLatestReplacesRatesUnionsJobsAccumulatesThroughput(t *testing.T) {
	ch := make(chan livePayload, 1)

	first := livePayload{
		Jobs:             []jobDTO{{ID: 1, BytesDone: 100}, {ID: 2, BytesDone: 200}},
		Down:             10,
		Up:               11,
		Throughput:       []throughputSampleDTO{{At: "d1"}},
		UploadThroughput: []throughputSampleDTO{{At: "u1"}},
	}
	if got := sendLatest(ch, first); len(got.Throughput) != 1 || len(got.UploadThroughput) != 1 {
		t.Fatalf("first send: got %+v", got)
	}

	second := livePayload{
		Jobs:             []jobDTO{{ID: 2, BytesDone: 250}, {ID: 3, BytesDone: 300}},
		Down:             20,
		Up:               21,
		Throughput:       []throughputSampleDTO{{At: "d2"}},
		UploadThroughput: []throughputSampleDTO{{At: "u2"}},
	}
	merged := sendLatest(ch, second)
	if merged.Down != 20 || merged.Up != 21 {
		t.Errorf("merged Down/Up = %d/%d, want second's 20/21", merged.Down, merged.Up)
	}
	wantJobs := map[int64]int64{1: 100, 2: 250, 3: 300}
	if len(merged.Jobs) != len(wantJobs) {
		t.Fatalf("merged.Jobs = %+v, want 3 entries (union of both ticks, job 1 not dropped)", merged.Jobs)
	}
	for _, dto := range merged.Jobs {
		if want, ok := wantJobs[dto.ID]; !ok || want != dto.BytesDone {
			t.Errorf("job %d BytesDone = %d, want %d", dto.ID, dto.BytesDone, wantJobs[dto.ID])
		}
	}
	if len(merged.Throughput) != 2 || merged.Throughput[0].At != "d1" || merged.Throughput[1].At != "d2" {
		t.Errorf("merged.Throughput = %+v, want [d1, d2]", merged.Throughput)
	}
	if len(merged.UploadThroughput) != 2 || merged.UploadThroughput[0].At != "u1" || merged.UploadThroughput[1].At != "u2" {
		t.Errorf("merged.UploadThroughput = %+v, want [u1, u2]", merged.UploadThroughput)
	}

	queued := <-ch
	if len(queued.Jobs) != 3 || len(queued.Throughput) != 2 || len(queued.UploadThroughput) != 2 {
		t.Errorf("queued = %+v, want 3 jobs and 2/2 directional samples", queued)
	}
}

func TestSendLatestDeliversDirectlyToEmptyChannel(t *testing.T) {
	ch := make(chan livePayload, 1)
	payload := livePayload{Down: 5}
	got := sendLatest(ch, payload)
	if got.Down != 5 {
		t.Errorf("got = %+v, want the payload unchanged", got)
	}
	select {
	case queued := <-ch:
		if queued.Down != 5 {
			t.Errorf("queued = %+v, want Down=5", queued)
		}
	default:
		t.Fatal("expected a payload queued in ch")
	}
}

// --- ?jobs= parsing ----------------------------------------------------

func TestParseStreamJobIDs(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    map[int64]struct{}
		wantErr bool
	}{
		{"absent", "", nil, false},
		{"single", "5", map[int64]struct{}{5: {}}, false},
		{"multiple", "1,2,3", map[int64]struct{}{1: {}, 2: {}, 3: {}}, false},
		{"deduplicated", "1,1,2", map[int64]struct{}{1: {}, 2: {}}, false},
		{"non-numeric", "1,abc", nil, true},
		{"zero", "0", nil, true},
		{"negative", "-5", nil, true},
		{"empty entry", "1,,2", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStreamJobIDs(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got %v", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseStreamJobIDs(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseStreamJobIDsRejectsOverMax(t *testing.T) {
	ids := make([]string, streamMaxJobScope+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	_, err := parseStreamJobIDs(strings.Join(ids, ","))
	if err == nil {
		t.Fatalf("expected an error for %d distinct ids (max %d)", len(ids), streamMaxJobScope)
	}
}

// --- hub tests ---------------------------------------------------------

// TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe covers the shared-
// broadcaster lifecycle directly: started on the first subscriber, still
// running with two, and stopped only once the last one leaves.
func TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe(t *testing.T) {
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)

	id1, _, _ := hub.subscribe(context.Background(), 0, nil)
	if !hubRunning(hub) {
		t.Fatal("expected ticker running after first subscribe")
	}

	id2, _, _ := hub.subscribe(context.Background(), 0, nil)
	if n := hubSubCount(hub); n != 2 {
		t.Fatalf("expected 2 subs, got %d", n)
	}
	if !hubRunning(hub) {
		t.Fatal("expected ticker still running with two subscribers")
	}

	hub.unsubscribe(id1)
	if !hubRunning(hub) {
		t.Fatal("expected ticker still running with one subscriber left")
	}
	if n := hubSubCount(hub); n != 1 {
		t.Fatalf("expected 1 sub left, got %d", n)
	}

	hub.unsubscribe(id2)
	if hubRunning(hub) {
		t.Fatal("expected ticker stopped after last unsubscribe")
	}
	if n := hubSubCount(hub); n != 0 {
		t.Fatalf("expected 0 subs left, got %d", n)
	}
}

// hubRunning/hubSubCount read streamHub's mutex-guarded fields under its own
// lock — a bare field read here would itself be a data race in a
// concurrency test.
func hubRunning(h *streamHub) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancel != nil
}

func hubSubCount(h *streamHub) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// TestStreamHubTickNoOpAfterContextCancelled is issue #161 review finding #4:
// a tick running against an already-cancelled context (the run goroutine was
// parked on h.mu when unsubscribe cancelled it, and a new subscriber
// registered before this stale tick got the lock) must not send anything or
// touch subscriber state.
func TestStreamHubTickNoOpAfterContextCancelled(t *testing.T) {
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	_, ch, initial := hub.subscribe(context.Background(), 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hub.tick(ctx)

	select {
	case got := <-ch:
		t.Fatalf("tick with a cancelled context must not send a frame, got %+v", got)
	default:
	}

	hub.mu.Lock()
	sub := hub.subs[0]
	hub.mu.Unlock()
	if sub.last.Down != initial.Down {
		t.Errorf("subscriber state changed after a no-op tick: last=%+v", sub.last)
	}
}

// TestStreamHubTickSendsChangedDataAndSuppressesUnchanged drives the
// broadcaster's tick() directly across multiple calls with changing and
// unchanged live data, and asserts on what a subscriber actually receives —
// covering the gap the #161 review's finding #10 called out: every prior
// HTTP-level test used tick = time.Hour, so tick() itself was never
// exercised on the non-empty path.
func TestStreamHubTickSendsChangedDataAndSuppressesUnchanged(t *testing.T) {
	var down int64 = 100
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: atomic.LoadInt64(&down)},
		}, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job:                 core.AlbumJob{ID: 7},
			Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
			AlbumBytesTotal:     1000,
			AlbumBytesRemaining: 1000,
		}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	// subscribe() itself does a synchronous correlation refresh, so the
	// fixture above is already loaded before the first tick. Scoped to job 7
	// (issue #268: unscoped subscribers get no job frames at all).
	id, ch, initial := hub.subscribe(context.Background(), 0, map[int64]struct{}{7: {}})
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].Speed != 100 {
		t.Fatalf("initial payload = %+v, want job 7 at speed 100", initial)
	}

	// Unchanged data: tick must not enqueue a second frame.
	hub.tick(context.Background())
	select {
	case got := <-ch:
		t.Fatalf("unchanged tick must not send a frame, got %+v", got)
	default:
	}

	// Changed data: tick must enqueue a frame reflecting it.
	atomic.StoreInt64(&down, 250)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 || got.Jobs[0].Speed != 250 {
			t.Errorf("changed tick payload = %+v, want job 7 at speed 250", got)
		}
	default:
		t.Fatal("changed tick must send a frame")
	}
}

func TestStreamHubTickSendsUploadOnlyDeltaWithIndependentWatermark(t *testing.T) {
	t1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	var uploadAdvanced atomic.Bool
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		series := core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: t1, BytesPerSecond: 100}},
			Upload:   []core.ThroughputSample{{At: t1, BytesPerSecond: 300}},
		}
		if uploadAdvanced.Load() {
			series.Upload = append(series.Upload, core.ThroughputSample{At: t2, BytesPerSecond: 300})
		}
		return series, nil
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	id, ch, initial := hub.subscribe(context.Background(), 0, nil)
	defer hub.unsubscribe(id)
	if initial.Down != 100 || initial.Up != 300 {
		t.Fatalf("initial down/up = %d/%d, want 100/300", initial.Down, initial.Up)
	}

	uploadAdvanced.Store(true)
	id2, ch2, secondInitial := hub.subscribe(context.Background(), 0, nil)
	defer hub.unsubscribe(id2)
	if len(secondInitial.UploadThroughput) != 2 {
		t.Fatalf("second subscriber initial upload series = %+v, want t1 and t2", secondInitial.UploadThroughput)
	}

	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Throughput) != 0 {
			t.Errorf("download delta = %+v, want none", got.Throughput)
		}
		if len(got.UploadThroughput) != 1 || got.UploadThroughput[0].At != t2.Format(timeFormat) {
			t.Errorf("upload delta = %+v, want only t2", got.UploadThroughput)
		}
	default:
		t.Fatal("upload-only sample must trigger a frame")
	}
	select {
	case got := <-ch2:
		t.Fatalf("newer subscriber must not receive already-seen upload sample, got %+v", got)
	default:
	}

	hub.mu.Lock()
	sub1, sub2 := hub.subs[id], hub.subs[id2]
	downloadAt1, uploadAt1 := sub1.lastDownloadThroughputAt, sub1.lastUploadThroughputAt
	downloadAt2, uploadAt2 := sub2.lastDownloadThroughputAt, sub2.lastUploadThroughputAt
	hub.mu.Unlock()
	if !downloadAt1.Equal(t1) || !uploadAt1.Equal(t2) || !downloadAt2.Equal(t1) || !uploadAt2.Equal(t2) {
		t.Errorf("subscriber watermarks = first %s/%s second %s/%s, want %s/%s for both", downloadAt1, uploadAt1, downloadAt2, uploadAt2, t1, t2)
	}
}

// TestStreamHubScopedJobPicksUpLiveMatchWithinOneTick covers a scoped
// subscriber's requested job gaining a live match: its jobDTO reflects the
// live Speed the very next tick. Unlike the pre-#268 unscoped design, this
// no longer depends on the event-driven correlation refresh at all — a
// scoped job is already in viewByJob (it's in "wanted" the moment it's
// requested, live-matched or not — see refreshCorrelation), and Speed is
// always computed from that tick's own fresh live fetch (toJobDTO's idx
// parameter), never from a cached value. correlationInterval is still
// time.Hour here specifically to prove that.
func TestStreamHubScopedJobPicksUpLiveMatchWithinOneTick(t *testing.T) {
	var liveOn atomic.Bool
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		if !liveOn.Load() {
			return nil, nil
		}
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: 42},
		}, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job:                 core.AlbumJob{ID: 9},
			Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
			AlbumBytesTotal:     1000,
			AlbumBytesRemaining: 1000,
		}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	id, ch, initial := hub.subscribe(context.Background(), 0, map[int64]struct{}{9: {}})
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].Speed != 0 {
		t.Fatalf("initial payload = %+v, want job 9 present with no live match yet", initial)
	}

	liveOn.Store(true)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 || got.Jobs[0].ID != 9 || got.Jobs[0].Speed != 42 {
			t.Errorf("expected job 9's speed picked up within one tick, got %+v", got)
		}
	default:
		t.Fatal("expected a frame once the candidate became live-matched")
	}
}

// TestStreamHubFileLevelRefreshKeepsMultiFileAlbumTotalMonotonic is issue
// #258 review finding B1: the event-driven refresh trigger must compare
// live-matched FILES, not live-matched CANDIDATES. A multi-file candidate
// stays "matched" at candidate granularity as long as ANY of its files is
// still live, so a candidate-granular trigger would never fire when one file
// among several completes and leaves ListDownloads — serving that one
// file's stale persisted bytes (from the last correlationInterval refresh)
// and producing a visible backwards step in the album total, exactly the bug
// commit 99fc7aa's now-removed byte floor existed to paper over.
func TestStreamHubFileLevelRefreshKeepsMultiFileAlbumTotalMonotonic(t *testing.T) {
	var aLive atomic.Bool
	aLive.Store(true)
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		// Speed is set on both files so that a.flac leaving the live list
		// entirely always changes the album's aggregated Speed (900 -> 400)
		// and so always produces a frame to assert on, regardless of
		// whether BytesDone itself happens to move — Speed changing is not
		// what this test is about, but it's what makes the frame arrive
		// deterministically so the streamed BytesDone can be checked
		// directly instead of only inferred from the cache.
		live := []core.RemoteTransfer{
			{Username: "alice", Filename: "b.flac", State: core.TransferInProgress, BytesDone: 3_000_000, Speed: 400},
		}
		if aLive.Load() {
			live = append(live, core.RemoteTransfer{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 9_000_000, Speed: 500})
		}
		return live, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job: core.AlbumJob{ID: 11},
			Attempt: &core.Candidate{ID: 101, Username: "alice", Files: []core.CandidateFile{
				{Filename: "a.flac", Size: 9_000_000},
				{Filename: "b.flac", Size: 3_000_000},
			}},
			AlbumBytesTotal:     12_000_000,
			AlbumBytesRemaining: 12_000_000,
		}}, nil
	}
	// Simulates reconcile: while a.flac is still live it hasn't been
	// persisted at its final size yet (5MB, an arbitrary partial figure);
	// the instant it completes and leaves the live list, its persisted bytes
	// are already final (9MB) — reconcile always persists before purging
	// (jobBytesDone's doc comment). A correctly-firing per-file refresh picks
	// that up in the very same tick; a candidate-granular one (still
	// "matched" via b.flac) would keep serving the stale 5MB.
	transferBytesFn := func(ctx context.Context, ids []int64) (map[int64]map[string]int64, error) {
		if aLive.Load() {
			return map[int64]map[string]int64{101: {"a.flac": 5_000_000, "b.flac": 3_000_000}}, nil
		}
		return map[int64]map[string]int64{101: {"a.flac": 9_000_000, "b.flac": 3_000_000}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, transferBytesFn, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	id, ch, initial := hub.subscribe(context.Background(), 0, map[int64]struct{}{11: {}})
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 {
		t.Fatalf("initial payload = %+v, want job 11 present", initial)
	}
	if before := initial.Jobs[0].BytesDone; before != 12_000_000 {
		t.Fatalf("initial BytesDone = %d, want 12_000_000 (both files live)", before)
	}

	// a.flac completes and leaves ListDownloads; b.flac stays live, so the
	// candidate as a whole is still "matched" at candidate granularity — only
	// a file-granular trigger notices this transition at all. Speed dropping
	// from 900 (both files) to 400 (b.flac alone) guarantees a frame is sent
	// regardless of what BytesDone does, so the streamed BytesDone can be
	// asserted directly rather than only inferred from the cache.
	aLive.Store(false)
	hub.tick(context.Background())

	select {
	case got := <-ch:
		if len(got.Jobs) != 1 {
			t.Fatalf("expected job 11 in the frame after a.flac completes, got %+v", got)
		}
		if got.Jobs[0].BytesDone != 12_000_000 {
			t.Errorf("streamed BytesDone = %d, want 12_000_000 (a.flac's now-final persisted bytes + b.flac's live bytes) — a candidate-granular trigger would regress this to 8_000_000 by keeping a.flac's stale 5MB persisted figure", got.Jobs[0].BytesDone)
		}
	default:
		t.Fatal("expected a frame once a.flac left the live list (Speed alone must have changed)")
	}

	// Corroborating evidence at the cache level: the refresh must actually
	// have run, not just happened to leave BytesDone unchanged.
	if got := hub.bytesSnapshot()[101]["a.flac"]; got != 9_000_000 {
		t.Errorf("bytesByCandidate[101][\"a.flac\"] = %d, want 9_000_000 — the event-driven refresh must fire on a.flac's own live-match transition even though candidate 101 (via b.flac) stays live-matched throughout", got)
	}
}

// TestStreamHubPreservesBytesCacheWhenTransferBytesFailsDuringEventDrivenRefresh
// is issue #258 review finding C1: refreshCorrelation now runs under tick's
// own tighter streamFetchTimeout budget (already partly consumed by
// fetchLive/fetchThroughput) whenever the event-driven trigger fires, not
// only from subscribe/run's untimed contexts as on origin/main — a real risk
// of a mid-refresh deadline exceeded that didn't exist before. A failed
// transferBytes call must leave h.bytesByCandidate as it was, not nil: nil
// would make jobBytesDone fall back to the store's own (possibly much lower
// or stale) AlbumBytesDone, producing exactly the kind of backwards step
// this whole PR exists to remove.
func TestStreamHubPreservesBytesCacheWhenTransferBytesFailsDuringEventDrivenRefresh(t *testing.T) {
	var aLive atomic.Bool
	aLive.Store(true)
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		live := []core.RemoteTransfer{
			{Username: "dave", Filename: "b.flac", State: core.TransferInProgress, BytesDone: 3_000_000, Speed: 400},
		}
		if aLive.Load() {
			live = append(live, core.RemoteTransfer{Username: "dave", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 9_000_000, Speed: 500})
		}
		return live, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job: core.AlbumJob{ID: 41},
			Attempt: &core.Candidate{ID: 301, Username: "dave", Files: []core.CandidateFile{
				{Filename: "a.flac", Size: 9_000_000},
				{Filename: "b.flac", Size: 3_000_000},
			}},
			// Deliberately far below the correct 12M, so a fallback to this
			// value on a failed transferBytes fetch is unmistakable.
			AlbumBytesDone:      4_000_000,
			AlbumBytesTotal:     12_000_000,
			AlbumBytesRemaining: 12_000_000,
		}}, nil
	}
	transferBytesFn := func(ctx context.Context, ids []int64) (map[int64]map[string]int64, error) {
		if aLive.Load() {
			return map[int64]map[string]int64{301: {"a.flac": 9_000_000, "b.flac": 3_000_000}}, nil
		}
		// The event-driven refresh triggered by a.flac's own transition runs
		// out of budget (see this test's doc comment).
		return nil, context.DeadlineExceeded
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, transferBytesFn, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	id, ch, initial := hub.subscribe(context.Background(), 0, map[int64]struct{}{41: {}})
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].BytesDone != 12_000_000 {
		t.Fatalf("initial payload = %+v, want job 41 at 12_000_000 bytes", initial)
	}

	// a.flac completes and leaves ListDownloads at the same moment
	// transferBytes starts failing; b.flac stays live.
	aLive.Store(false)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 {
			t.Fatalf("expected job 41 in the frame, got %+v", got)
		}
		if got.Jobs[0].BytesDone != 12_000_000 {
			t.Errorf("streamed BytesDone = %d, want 12_000_000 (preserved cache) — a failed transferBytes fetch must not fall back to the stale AlbumBytesDone (4_000_000)", got.Jobs[0].BytesDone)
		}
	default:
		t.Fatal("expected a frame once a.flac left the live list (Speed alone must have changed)")
	}

	if got := hub.bytesSnapshot()[301]; got == nil {
		t.Error("bytesByCandidate[301] = nil, want the preserved pre-failure map")
	} else if got["a.flac"] != 9_000_000 || got["b.flac"] != 3_000_000 {
		t.Errorf("bytesByCandidate[301] = %+v, want the preserved {a.flac: 9_000_000, b.flac: 3_000_000}", got)
	}
}

// TestStreamHubScopedJobViewRefreshesPromptlyOnLiveMatchChange is issue
// #268's simplification of the old B3/C2 mechanism: a SCOPED job never
// leaves viewByJob's "wanted" set (it's requested, live-matched or not — see
// refreshCorrelation), so there is no "eviction race" to guard against
// anymore, and scopedJobIDs' trackedIDs (which existed only for that race)
// is gone. What remains is simpler and still worth covering: the
// event-driven refresh (triggered by the live-matched file set changing)
// still refreshes this job's view promptly, so a DB state transition
// (DOWNLOADING -> IMPORTING) that happens in the same window a live transfer
// disappears is reflected without waiting a full correlationInterval.
func TestStreamHubScopedJobViewRefreshesPromptlyOnLiveMatchChange(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		if !live.Load() {
			return nil, nil
		}
		return []core.RemoteTransfer{
			{Username: "carol", Filename: "01.flac", State: core.TransferInProgress, Speed: 999, BytesDone: 44_000_000},
		}, nil
	}
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		job := core.AlbumJob{ID: 30, State: core.StateDownloading, UpdatedAt: t0}
		albumBytesDone := int64(44_000_000)
		if !live.Load() {
			// Simulates reconcile finishing the download in the SAME window
			// the live transfer disappears: the persisted row moves on to
			// IMPORTING with its final byte count, not just a reverted
			// DOWNLOADING snapshot.
			job.State = core.StateImporting
			job.UpdatedAt = t1
			albumBytesDone = 49_000_000
		}
		return []core.JobView{{
			Job:                 job,
			Attempt:             &core.Candidate{Username: "carol", Files: []core.CandidateFile{{Filename: "01.flac", Size: 49_000_000}}},
			AlbumBytesDone:      albumBytesDone,
			AlbumBytesTotal:     49_000_000,
			AlbumBytesRemaining: 49_000_000 - albumBytesDone,
		}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour)
	id, ch, initial := hub.subscribe(context.Background(), 0, map[int64]struct{}{30: {}})
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].Speed != 999 {
		t.Fatalf("initial payload = %+v, want job 30 at speed 999", initial)
	}

	// The transfer disappears entirely (finished) at the same moment
	// reconcile commits the job's transition to IMPORTING.
	live.Store(false)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 || got.Jobs[0].ID != 30 {
			t.Fatalf("expected an updated frame for job 30, got %+v", got)
		}
		dto := got.Jobs[0]
		if dto.Speed != 0 {
			t.Errorf("Speed = %d, want 0 (reverted)", dto.Speed)
		}
		if dto.State != string(core.StateImporting) {
			t.Errorf("State = %q, want %q (fresh view picked up promptly)", dto.State, core.StateImporting)
		}
		if dto.BytesDone != 49_000_000 {
			t.Errorf("BytesDone = %d, want 49_000_000 (fresh view's final AlbumBytesDone)", dto.BytesDone)
		}
		if dto.UpdatedAt != t1.Format(timeFormat) {
			t.Errorf("UpdatedAt = %q, want %q (fresh view's timestamp)", dto.UpdatedAt, t1.Format(timeFormat))
		}
	default:
		t.Fatal("expected an updated frame reflecting the state transition")
	}
}

// --- HTTP handler tests --------------------------------------------------

// testStreamRecorder wraps httptest.ResponseRecorder to also satisfy the
// SetWriteDeadline hook http.ResponseController looks for. A bare
// httptest.ResponseRecorder doesn't implement it, and registerStream must
// clear the write deadline to survive past the server's 30s WriteTimeout
// (see its own doc comment) — exercising it against a plain
// httptest.NewRecorder() would always 500 with "streaming not supported".
// Write/Flush/String are synchronized because the handler goroutine writes
// concurrently with the test goroutine polling the body.
type testStreamRecorder struct {
	rec *httptest.ResponseRecorder
	mu  sync.Mutex
}

func newTestStreamRecorder() *testStreamRecorder {
	return &testStreamRecorder{rec: httptest.NewRecorder()}
}

func (r *testStreamRecorder) Header() http.Header { return r.rec.Header() }

func (r *testStreamRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Write(b)
}

func (r *testStreamRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.WriteHeader(code)
}

func (r *testStreamRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.Flush()
}

func (r *testStreamRecorder) SetWriteDeadline(time.Time) error { return nil }

func (r *testStreamRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Body.String()
}

func (r *testStreamRecorder) Code() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Code
}

// newStreamTestServer builds a bare mux with only GET /api/stream registered
// (via registerStream directly, not NewServer), so tests can pass short
// tick/correlation/heartbeat intervals instead of the real constants.
func newStreamTestServer(deps ServerDeps, tick, correlation, heartbeat time.Duration) http.Handler {
	mux := http.NewServeMux()
	registerStream(mux, deps, tick, correlation, heartbeat)
	return mux
}

// waitForBody polls rec's recorded body until it contains substr or the
// deadline passes, since the handler writes from its own goroutine.
func waitForBody(t *testing.T, rec *testStreamRecorder, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for body to contain %q; got %q", substr, rec.String())
}

func TestStreamEndpointSetsHeadersAndSendsFirstPayload(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	// Long tick/correlation/heartbeat: this test only cares about the
	// immediate first frame sent at subscribe time, not the periodic timers.
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
	rec := newTestStreamRecorder()

	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: live")
	cancel()
	<-done

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if xa := rec.Header().Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xa)
	}
	body := rec.String()
	if !strings.Contains(body, "retry:") {
		t.Errorf("body missing retry: line, got %q", body)
	}
	if !strings.Contains(body, "event: live\ndata:") {
		t.Errorf("body missing first event: live frame, got %q", body)
	}
}

func TestStreamEndpointSendsHeartbeatWhenIdle(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	mux := newStreamTestServer(deps, time.Hour, time.Hour, 15*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, ": heartbeat")
	cancel()
	<-done
}

// TestStreamEndpointNilLiveTransfersAndThroughputDoesNotPanic covers both
// optional funcs being nil at once: LiveTransfers/Throughput are documented
// as optional on ServerDeps (issue #157/#161), and the stream must degrade
// to reporting nothing live rather than panic.
func TestStreamEndpointNilLiveTransfersAndThroughputDoesNotPanic(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = nil
	deps.Throughput = nil
	mux := newStreamTestServer(deps, 5*time.Millisecond, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: live")
	time.Sleep(30 * time.Millisecond) // let a few ticks pass without panicking
	cancel()
	<-done

	if rec.Code() != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code())
	}
	// jobs is omitted (omitempty, no live-matched jobs) but down/up are
	// explicit zeroes, never omitted — see downSpeed/upSpeed's fallbacks.
	if body := rec.String(); !strings.Contains(body, `{"down":0,"up":0}`) {
		t.Errorf("nil live sources must emit explicit zero rates with jobs omitted, got %q", body)
	}
}

// TestStreamEndpointTwoClientsBothGetInitialFrame is the HTTP-level
// counterpart of TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe: two
// concurrent connections against the same mux both get served (and both get
// their own first payload) without either blocking the other. It does not
// itself prove loop sharing — that's TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe's
// job — only that the HTTP layer doesn't serialize or drop either client.
func TestStreamEndpointTwoClientsBothGetInitialFrame(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	req1 := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx1)
	req2 := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx2)
	rec1 := newTestStreamRecorder()
	rec2 := newTestStreamRecorder()

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() { mux.ServeHTTP(rec1, req1); close(done1) }()
	go func() { mux.ServeHTTP(rec2, req2); close(done2) }()

	waitForBody(t, rec1, "event: live")
	waitForBody(t, rec2, "event: live")
	cancel1()
	cancel2()
	<-done1
	<-done2
}

// TestStreamEndpointShutdownClosesConnection covers the optional
// ServerDeps.Shutdown wiring (issue #161): closing it must end an open
// stream promptly rather than waiting for the client to disconnect.
func TestStreamEndpointShutdownClosesConnection(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	shutdown := make(chan struct{})
	deps.Shutdown = shutdown
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: live")
	close(shutdown)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after Shutdown was closed")
	}
}

func TestStreamEndpointInvalidJobIDReturns400(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	for _, raw := range []string{"not-a-number", "0", "-5"} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/stream?job="+raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestStreamEndpointInvalidJobsParamReturns400 covers ?jobs= validation
// (issue #258): non-numeric, non-positive, or empty entries, and an array
// over streamMaxJobScope, must all 400 rather than silently degrading.
func TestStreamEndpointInvalidJobsParamReturns400(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	over := make([]string, streamMaxJobScope+1)
	for i := range over {
		over[i] = strconv.Itoa(i + 1)
	}

	for _, raw := range []string{"abc", "0", "-1", "1,,2", strings.Join(over, ",")} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/stream?jobs="+raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestStreamEndpointJobsScopeRestrictsToRequestedIDs covers the ?jobs=
// scoping contract itself end to end: a subscriber that names an id set only
// ever gets jobs from that set, even when other jobs exist and are
// live-matched.
func TestStreamEndpointJobsScopeRestrictsToRequestedIDs(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 10},
			{Username: "bob", Filename: "02.flac", State: core.TransferInProgress, BytesDone: 20},
		}, nil
	}
	deps.Jobs = func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{Job: core.AlbumJob{ID: 1}, Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 100}}}},
			{Job: core.AlbumJob{ID: 2}, Attempt: &core.Candidate{Username: "bob", Files: []core.CandidateFile{{Filename: "02.flac", Size: 100}}}},
		}, nil
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/stream?jobs=1", nil)
	rec := newTestStreamRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { mux.ServeHTTP(rec, req); close(done) }()

	waitForBody(t, rec, "event: live")
	cancel()
	<-done

	body := rec.String()
	if !strings.Contains(body, `"id":1`) {
		t.Errorf("expected requested job 1 in body, got %q", body)
	}
	if strings.Contains(body, `"id":2`) {
		t.Errorf("job 2 must not appear for a subscriber scoped to ?jobs=1, got %q", body)
	}
}

// TestStreamEndpointFedByOwnCorrelationRefresh covers the job<->candidate
// correlation path end to end without any GET /api/jobs call: the hub's
// deps.Jobs and deps.JobDetail are queried directly by subscribe()'s
// synchronous refresh, so ?job=<id> reports live per-file data on the very
// first frame rather than after a whole correlationInterval.
func TestStreamEndpointFedByOwnCorrelationRefresh(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 250},
		}, nil
	}
	deps.Jobs = func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:                 core.AlbumJob{ID: 42},
				Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
				AlbumBytesTotal:     1000,
				AlbumBytesRemaining: 1000,
			},
		}, nil
	}
	deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
		t.Fatal("GET /api/stream must never call PagedJobs")
		return PagedJobsResult{}, nil
	}
	deps.JobDetail = func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{
			Job: core.AlbumJob{ID: jobID},
			Attempts: []core.AttemptDetail{{
				Attempt:   core.Candidate{ID: 1, Username: "alice"},
				Transfers: []core.Transfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesTotal: 1000}},
			}},
		}, true, nil
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream?job=42", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "01.flac")
	cancel()
	<-done

	if !strings.Contains(rec.String(), `"bytesDone":250`) {
		t.Errorf("expected the live bytesDone (250) in the first payload, got %q", rec.String())
	}
}

// TestStreamEndpointRejectsOverCapacity is issue #161 review finding #7: past
// streamMaxSubscribers open connections, a new one must get a proper 503
// rather than an unbounded broadcaster fan-out.
func TestStreamEndpointRejectsOverCapacity(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour)

	var cancels []context.CancelFunc
	var dones []chan struct{}
	for i := 0; i < streamMaxSubscribers; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
		rec := newTestStreamRecorder()
		done := make(chan struct{})
		dones = append(dones, done)
		go func() { mux.ServeHTTP(rec, req); close(done) }()
		waitForBody(t, rec, "event: live")
	}
	defer func() {
		for i, cancel := range cancels {
			cancel()
			<-dones[i]
		}
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 once at streamMaxSubscribers", rec.Code)
	}
}

// TestLivePayloadHasNoDBOnlyFieldsAtTopLevel is the design's explicit
// requirement: the stream must never carry DB-only fields like
// status/state/events/peers at the TOP level (down/up/detail/jobs itself).
// Since #258 a job entry legitimately carries the whole jobDTO, DB-owned
// fields included — that's the point of streaming whole objects — so this
// only asserts about the payload's own top-level keys, not jobDTO's shape.
func TestLivePayloadHasNoDBOnlyFieldsAtTopLevel(t *testing.T) {
	live := []core.RemoteTransfer{{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 10, Speed: 5, QueuePosition: 2}}
	payload := buildLiveSnapshot(live, 1, core.ThroughputSeries{}, core.JobView{}, false, core.JobDetail{}, false, newLiveTransferIndex(live), nil, testFailedRetryAfter, testMaxCandidates, testNow)
	payload.Jobs = []jobDTO{}
	payload.Throughput = []throughputSampleDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 100, ActiveTransfers: 1}}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"status", "state", "events", "peers"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("payload top level must not contain %q (DB-only field)", forbidden)
		}
	}
}
