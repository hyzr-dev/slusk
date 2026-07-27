package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// --- pure function tests ---------------------------------------------------

func TestBuildStreamJobsOnlyIncludesLiveMatchedAndSorts(t *testing.T) {
	corr := []jobCorrelation{
		{id: 5, candidateID: 105, username: "bob", files: []core.CandidateFile{{Filename: "b.flac", Size: 500}}, albumBytesTotal: 500, albumBytesRemaining: 500},
		{id: 3, candidateID: 103, username: "alice", files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}, albumBytesDone: 100, albumBytesTotal: 1000, albumBytesRemaining: 900},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50, SpeedAverage: 40, BytesDone: 300, QueuePosition: 2},
	})

	persisted := map[int64]map[string]int64{103: {"a.flac": 100}}
	got := buildStreamJobs(corr, live, persisted)
	// job 5 has no live match at all (bob never appears in `live`) and must
	// be omitted entirely — see buildStreamJobs' absence contract.
	if len(got) != 1 {
		t.Fatalf("expected 1 job (job 5 has no live match), got %d: %+v", len(got), got)
	}
	if got[0].ID != 3 {
		t.Fatalf("expected job 3, got %d", got[0].ID)
	}
	if got[0].BytesDone != 300 {
		t.Errorf("job 3 BytesDone = %d, want 300 (live overlay)", got[0].BytesDone)
	}
	if got[0].BytesTotal != 1000 {
		t.Errorf("job 3 BytesTotal = %d, want 1000 (persisted, never overlaid)", got[0].BytesTotal)
	}
	if got[0].Speed != 50 || got[0].QueuePosition != 2 {
		t.Errorf("job 3 speed/queue = %d/%d, want 50/2", got[0].Speed, got[0].QueuePosition)
	}
}

// TestBuildStreamJobsDropsJobThatLosesItsLiveTransfer is the frontend
// contract implied by the #161 review's finding #1: a job present in one
// tick's `jobs` and absent in the next means "no live data right now" — the
// client must clear its live overlay and fall back to REST-cached values.
func TestBuildStreamJobsDropsJobThatLosesItsLiveTransfer(t *testing.T) {
	corr := []jobCorrelation{
		{id: 3, username: "alice", files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}, albumBytesTotal: 1000, albumBytesRemaining: 1000},
	}
	withLive := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50, BytesDone: 300},
	})
	if got := buildStreamJobs(corr, withLive, nil); len(got) != 1 {
		t.Fatalf("expected job present while its transfer is live, got %d: %+v", len(got), got)
	}

	withoutLive := newLiveTransferIndex(nil)
	if got := buildStreamJobs(corr, withoutLive, nil); len(got) != 0 {
		t.Fatalf("expected job absent once its transfer disappears from live, got %d: %+v", len(got), got)
	}
}

func TestBuildStreamFilesOnlyIncludesLiveMatched(t *testing.T) {
	job := jobCorrelation{
		username: "alice",
		files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000},
			{Filename: "02.flac", Size: 2000}, // not yet enqueued, no live entry
		},
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 400, Speed: 100},
	})

	got := buildStreamFiles(job, idx)
	if len(got) != 1 {
		t.Fatalf("expected 1 file (02.flac has no live match), got %d: %+v", len(got), got)
	}
	if got[0].Filename != "01.flac" || got[0].State != "IN_PROGRESS" || got[0].BytesDone != 400 || got[0].BytesTotal != 1000 {
		t.Errorf("unexpected file dto: %+v", got[0])
	}
}

func TestBuildStreamFilesNoFilesYieldsEmpty(t *testing.T) {
	if got := buildStreamFiles(jobCorrelation{}, liveTransferIndex{}); len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}

func TestSumDownSpeedExcludesTerminal(t *testing.T) {
	live := []core.RemoteTransfer{
		{State: core.TransferInProgress, Speed: 100},
		{State: core.TransferQueued, Speed: 50},
		{State: core.TransferCompleted, Speed: 999}, // excluded
		{State: core.TransferErrored, Speed: 999},   // excluded
	}
	if got := sumDownSpeed(live); got != 150 {
		t.Errorf("sumDownSpeed = %d, want 150", got)
	}
}

func TestNewThroughputSinceTableCases(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	samples := []core.ThroughputSample{
		{At: t0, BytesPerSecond: 1},
		{At: t0.Add(time.Second), BytesPerSecond: 2},
		{At: t0.Add(2 * time.Second), BytesPerSecond: 3},
	}
	cases := []struct {
		name  string
		since time.Time
		want  int
	}{
		{"zero time returns everything", time.Time{}, 3},
		{"exactly at first sample excludes it (not strictly after)", t0, 2},
		{"between samples", t0.Add(500 * time.Millisecond), 2},
		{"after everything returns nothing", t0.Add(10 * time.Second), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newThroughputSince(samples, tc.since)
			if len(got) != tc.want {
				t.Errorf("newThroughputSince(...) len = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestChangedSinceLastTableCases(t *testing.T) {
	jobsA := []streamJobDTO{{ID: 1, BytesDone: 100}}
	jobsB := []streamJobDTO{{ID: 1, BytesDone: 200}}
	filesA := []streamFileDTO{{Filename: "a.flac", BytesDone: 10}}

	cases := []struct {
		name               string
		prev, next         livePayload
		newThroughputCount int
		want               bool
	}{
		{"nothing changed", livePayload{Jobs: jobsA, Files: filesA, Down: 5}, livePayload{Jobs: jobsA, Files: filesA, Down: 5}, 0, false},
		{"jobs changed", livePayload{Jobs: jobsA}, livePayload{Jobs: jobsB}, 0, true},
		{"down changed", livePayload{Jobs: jobsA, Down: 5}, livePayload{Jobs: jobsA, Down: 6}, 0, true},
		{"files changed", livePayload{Files: filesA}, livePayload{}, 0, true},
		{"only new throughput, nothing else changed", livePayload{Jobs: jobsA, Files: filesA, Down: 5}, livePayload{Jobs: jobsA, Files: filesA, Down: 5}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := changedSinceLast(tc.prev, tc.next, tc.newThroughputCount)
			if got != tc.want {
				t.Errorf("changedSinceLast(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildLivePayloadFileScopingByJobID(t *testing.T) {
	corr := []jobCorrelation{{id: 7, username: "alice", files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}, albumBytesTotal: 1000, albumBytesRemaining: 1000}}
	live := []core.RemoteTransfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 500}}

	unscoped := buildLivePayload(corr, live, 0, nil)
	if unscoped.Files != nil {
		t.Errorf("expected nil Files without ?job=, got %+v", unscoped.Files)
	}

	scoped := buildLivePayload(corr, live, 7, nil)
	if len(scoped.Files) != 1 || scoped.Files[0].Filename != "01.flac" {
		t.Errorf("expected 1 file for ?job=7, got %+v", scoped.Files)
	}

	missing := buildLivePayload(corr, live, 999, nil)
	if missing.Files != nil {
		t.Errorf("expected nil Files for unknown job id, got %+v", missing.Files)
	}
}

// TestSendLatestReplacesSnapshotButAccumulatesThroughput is issue #161
// review finding #3: dropping an undelivered payload must not lose its
// throughput samples, even though the rest of the payload (a full-set
// snapshot) is freely superseded.
func TestSendLatestReplacesSnapshotButAccumulatesThroughput(t *testing.T) {
	ch := make(chan livePayload, 1)

	first := livePayload{Jobs: []streamJobDTO{{ID: 1}}, Down: 10, Throughput: []throughputSampleDTO{{At: "t1"}}}
	if got := sendLatest(ch, first); len(got.Throughput) != 1 {
		t.Fatalf("first send: got %+v", got)
	}

	// Second send while `first` is still sitting unread in ch: its
	// throughput must survive, prepended to the new payload's.
	second := livePayload{Jobs: []streamJobDTO{{ID: 2}}, Down: 20, Throughput: []throughputSampleDTO{{At: "t2"}}}
	merged := sendLatest(ch, second)
	if merged.Down != 20 || len(merged.Jobs) != 1 || merged.Jobs[0].ID != 2 {
		t.Errorf("merged snapshot fields = %+v, want second's Jobs/Down (full-set fields are replaced, not merged)", merged)
	}
	if len(merged.Throughput) != 2 || merged.Throughput[0].At != "t1" || merged.Throughput[1].At != "t2" {
		t.Errorf("merged.Throughput = %+v, want [t1, t2] (first's sample preserved)", merged.Throughput)
	}

	queued := <-ch
	if len(queued.Throughput) != 2 {
		t.Errorf("what's actually queued in ch has %d throughput samples, want 2", len(queued.Throughput))
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

// --- hub tests ---------------------------------------------------------

// TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe covers the shared-
// broadcaster lifecycle directly: started on the first subscriber, still
// running with two, and stopped only once the last one leaves.
func TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe(t *testing.T) {
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, time.Hour, time.Hour)

	id1, _, _ := hub.subscribe(context.Background(), 0)
	if !hubRunning(hub) {
		t.Fatal("expected ticker running after first subscribe")
	}

	id2, _, _ := hub.subscribe(context.Background(), 0)
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
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, time.Hour, time.Hour)
	_, ch, initial := hub.subscribe(context.Background(), 0)

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
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, time.Hour, time.Hour)
	// subscribe() itself does a synchronous correlation refresh, so the
	// fixture above is already loaded before the first tick.
	_, ch, initial := hub.subscribe(context.Background(), 0)
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

// TestStreamEndpointFedByOwnCorrelationRefresh covers the job<->candidate
// correlation path end to end without any GET /api/jobs call: the hub's
// deps.Jobs is queried directly by subscribe()'s synchronous refresh, so
// ?job=<id> reports live per-file data on the very first frame.
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

// TestLivePayloadHasNoDBOnlyFields is the design's explicit requirement: the
// stream must never carry DB-only fields like status/state/events/peers at
// the job/top level. Asserted against buildLivePayload's real output (not a
// hand-built literal) so it actually exercises the builder, not just the
// struct tags. streamFileDTO's own "state" is exempt — it's a live,
// in-memory RemoteTransfer.State, not a persisted job/candidate state.
func TestLivePayloadHasNoDBOnlyFields(t *testing.T) {
	corr := []jobCorrelation{{id: 1, username: "alice", files: []core.CandidateFile{{Filename: "a.flac", Size: 20}}, albumBytesTotal: 20, albumBytesRemaining: 20}}
	live := []core.RemoteTransfer{{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 10, Speed: 5, QueuePosition: 2}}
	payload := buildLivePayload(corr, live, 1, nil)
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
	var jobsRaw []map[string]json.RawMessage
	if err := json.Unmarshal(raw["jobs"], &jobsRaw); err != nil {
		t.Fatalf("unmarshal jobs: %v", err)
	}
	if len(jobsRaw) != 1 {
		t.Fatalf("expected 1 job in payload, got %d", len(jobsRaw))
	}
	for _, forbidden := range []string{"status", "state", "createdAt", "updatedAt", "artist", "title"} {
		if _, ok := jobsRaw[0][forbidden]; ok {
			t.Errorf("streamJobDTO must not contain %q (DB-only field)", forbidden)
		}
	}
}
