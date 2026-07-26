package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// --- pure function tests ---------------------------------------------------

func TestBuildStreamJobsFiltersNoAttemptAndSorts(t *testing.T) {
	candidateB := &core.Candidate{Username: "bob", Files: []core.CandidateFile{{Filename: "b.flac", Size: 500}}}
	candidateA := &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}}
	jobs := []core.JobView{
		{Job: core.AlbumJob{ID: 5}, Attempt: candidateB, AlbumBytesTotal: 500, AlbumBytesRemaining: 500},
		{Job: core.AlbumJob{ID: 1}, Attempt: nil}, // no candidate: excluded
		{Job: core.AlbumJob{ID: 3}, Attempt: candidateA, AlbumBytesDone: 100, AlbumBytesDoneNonTerminal: 100, AlbumBytesTotal: 1000, AlbumBytesRemaining: 900},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50, SpeedAverage: 40, BytesDone: 300, QueuePosition: 2},
	})

	got := buildStreamJobs(jobs, live)
	if len(got) != 2 {
		t.Fatalf("expected 2 jobs (id 1 excluded, no candidate), got %d: %+v", len(got), got)
	}
	if got[0].ID != 3 || got[1].ID != 5 {
		t.Fatalf("expected sorted by id (3, 5), got (%d, %d)", got[0].ID, got[1].ID)
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
	if got[1].BytesDone != 0 || got[1].Speed != 0 {
		t.Errorf("job 5 (no live match) = %+v, want zero live fields", got[1])
	}
}

func TestBuildStreamFilesOnlyIncludesLiveMatched(t *testing.T) {
	job := core.JobView{
		Attempt: &core.Candidate{
			Username: "alice",
			Files: []core.CandidateFile{
				{Filename: "01.flac", Size: 1000},
				{Filename: "02.flac", Size: 2000}, // not yet enqueued, no live entry
			},
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

func TestBuildStreamFilesNilAttemptYieldsNil(t *testing.T) {
	if got := buildStreamFiles(core.JobView{}, liveTransferIndex{}); got != nil {
		t.Errorf("expected nil, got %+v", got)
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
		name                 string
		prevJobs, nextJobs   []streamJobDTO
		prevFiles, nextFiles []streamFileDTO
		prevDown, nextDown   int64
		newThroughputCount   int
		want                 bool
	}{
		{"nothing changed", jobsA, jobsA, filesA, filesA, 5, 5, 0, false},
		{"jobs changed", jobsA, jobsB, nil, nil, 0, 0, 0, true},
		{"down changed", jobsA, jobsA, nil, nil, 5, 6, 0, true},
		{"files changed", nil, nil, filesA, nil, 0, 0, 0, true},
		{"only new throughput, nothing else changed", jobsA, jobsA, filesA, filesA, 5, 5, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := changedSinceLast(tc.prevJobs, tc.nextJobs, tc.prevFiles, tc.nextFiles, tc.prevDown, tc.nextDown, tc.newThroughputCount)
			if got != tc.want {
				t.Errorf("changedSinceLast(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildLivePayloadFileScopingByJobID(t *testing.T) {
	candidate := &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}}
	jobs := []core.JobView{{Job: core.AlbumJob{ID: 7}, Attempt: candidate, AlbumBytesTotal: 1000, AlbumBytesRemaining: 1000}}
	live := []core.RemoteTransfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 500}}

	unscoped := buildLivePayload(jobs, live, 0)
	if unscoped.Files != nil {
		t.Errorf("expected nil Files without ?job=, got %+v", unscoped.Files)
	}

	scoped := buildLivePayload(jobs, live, 7)
	if len(scoped.Files) != 1 || scoped.Files[0].Filename != "01.flac" {
		t.Errorf("expected 1 file for ?job=7, got %+v", scoped.Files)
	}

	missing := buildLivePayload(jobs, live, 999)
	if missing.Files != nil {
		t.Errorf("expected nil Files for unknown job id, got %+v", missing.Files)
	}
}

// TestLivePayloadHasNoDBOnlyFields is the design's explicit requirement: the
// stream must never carry DB-only fields like status/state/events/peers at
// the job/top level. streamFileDTO's own "state" is exempt — it's a live,
// in-memory RemoteTransfer.State, not a persisted job/candidate state.
func TestLivePayloadHasNoDBOnlyFields(t *testing.T) {
	payload := livePayload{
		Jobs:       []streamJobDTO{{ID: 1, BytesDone: 10, BytesTotal: 20, Speed: 5, QueuePosition: 2, ETASeconds: 3}},
		Files:      []streamFileDTO{{Filename: "a.flac", State: "IN_PROGRESS", BytesDone: 10, BytesTotal: 20, Speed: 5, QueuePosition: 1}},
		Throughput: []streamThroughputDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 100, ActiveTransfers: 1}},
		Down:       5,
	}
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
	for _, forbidden := range []string{"status", "state", "createdAt", "updatedAt", "artist", "title"} {
		if _, ok := jobsRaw[0][forbidden]; ok {
			t.Errorf("streamJobDTO must not contain %q (DB-only field)", forbidden)
		}
	}
}

// --- hub tests ---------------------------------------------------------

// TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe covers the shared-
// broadcaster lifecycle directly: started on the first subscriber, still
// running with two, and stopped only once the last one leaves.
func TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe(t *testing.T) {
	jobIndex := &jobLiveIndex{}
	hub := newStreamHub(noopLiveTransfers, noopThroughput, jobIndex, time.Hour)

	id1, _, _ := hub.subscribe(context.Background(), 0)
	if hub.cancel == nil {
		t.Fatal("expected ticker running after first subscribe")
	}

	id2, _, _ := hub.subscribe(context.Background(), 0)
	if len(hub.subs) != 2 {
		t.Fatalf("expected 2 subs, got %d", len(hub.subs))
	}
	if hub.cancel == nil {
		t.Fatal("expected ticker still running with two subscribers")
	}

	hub.unsubscribe(id1)
	if hub.cancel == nil {
		t.Fatal("expected ticker still running with one subscriber left")
	}
	if len(hub.subs) != 1 {
		t.Fatalf("expected 1 sub left, got %d", len(hub.subs))
	}

	hub.unsubscribe(id2)
	if hub.cancel != nil {
		t.Fatal("expected ticker stopped after last unsubscribe")
	}
	if len(hub.subs) != 0 {
		t.Fatalf("expected 0 subs left, got %d", len(hub.subs))
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
// tick/heartbeat intervals instead of the real 1s/15s constants.
func newStreamTestServer(deps ServerDeps, tick, heartbeat time.Duration) (http.Handler, *jobLiveIndex) {
	mux := http.NewServeMux()
	jobIndex := &jobLiveIndex{}
	registerStream(mux, deps, jobIndex, deps.Shutdown, tick, heartbeat)
	return mux, jobIndex
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
	// Long tick/heartbeat: this test only cares about the immediate first
	// frame sent at subscribe time, not the periodic ticker.
	mux, _ := newStreamTestServer(deps, time.Hour, time.Hour)

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
	mux, _ := newStreamTestServer(deps, time.Hour, 15*time.Millisecond)

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
	mux, _ := newStreamTestServer(deps, 5*time.Millisecond, time.Hour)

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

// TestStreamEndpointTwoClientsShareOneLoop is the HTTP-level counterpart of
// TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe: two concurrent
// connections against the same mux both get served (and both get their own
// first payload) without either blocking the other.
func TestStreamEndpointTwoClientsShareOneLoop(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	mux, _ := newStreamTestServer(deps, time.Hour, time.Hour)

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
	mux, _ := newStreamTestServer(deps, time.Hour, time.Hour)

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
	mux, _ := newStreamTestServer(deps, time.Hour, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/stream?job=not-a-number", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestStreamEndpointFedByJobIndex covers the job<->candidate correlation
// path end to end: seeding jobIndex (as GET /api/jobs would via its side
// effect, see NewServer) makes ?job=<id> report live per-file data.
func TestStreamEndpointFedByJobIndex(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 250},
		}, nil
	}
	mux, jobIndex := newStreamTestServer(deps, time.Hour, time.Hour)
	jobIndex.set([]core.JobView{
		{
			Job:                 core.AlbumJob{ID: 42},
			Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
			AlbumBytesTotal:     1000,
			AlbumBytesRemaining: 1000,
		},
	})

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
