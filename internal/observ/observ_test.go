package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

const testFailedRetryAfter = time.Hour
const testMaxCandidates = 5

// testNow is a fixed instant used wherever a test needs to pass toJobDTO's
// (and friends') now parameter, so FramedAt assertions are deterministic.
var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// noopJobDetail, noopJobEvents, noopRecentEvents, and noopPeers are no-op
// implementations of the newer dashboard funcs, for tests that only care
// about routes unrelated to job detail/events/peers.
func noopJobDetail(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
	return core.JobDetail{}, false, nil
}
func noopJobView(ctx context.Context, jobID int64) (core.JobView, bool, error) {
	return core.JobView{}, false, nil
}
func noopJobEvents(ctx context.Context, jobID int64) ([]core.JobEvent, error)  { return nil, nil }
func noopRecentEvents(ctx context.Context, limit int) ([]core.JobEvent, error) { return nil, nil }
func noopPeers(ctx context.Context) ([]core.PeerRow, error)                    { return nil, nil }
func noopHealthy() bool                                                        { return true }
func noopModules() map[string]ModuleStatus                                     { return nil }
func noopRetry(ctx context.Context, jobID int64) error                         { return nil }
func noopConfig() AppConfig                                                    { return AppConfig{} }
func noopLiveTransfers(ctx context.Context) ([]core.RemoteTransfer, error)     { return nil, nil }
func noopTransferBytes(ctx context.Context, candidateIDs []int64) (map[int64]map[string]int64, error) {
	return nil, nil
}
func noopJobs(ctx context.Context) ([]core.JobView, error) { return nil, nil }
func noopPagedJobs(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
	return PagedJobsResult{Jobs: []core.JobView{}}, nil
}
func noopCharts(ctx context.Context) (ChartsData, error) { return ChartsData{}, nil }
func noopShares() ShareStatsReport                       { return ShareStatsReport{} }
func noopRescanShares() error                            { return nil }
func noopUploads() UploadReport                          { return UploadReport{} }
func noopThroughput(ctx context.Context) (core.ThroughputSeries, error) {
	return core.ThroughputSeries{}, nil
}
func noopConfigWriter(ConfigUpdate) error { return nil }
func noopRestart()                        {}
func noopCreateJob(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
	return core.JobView{}, nil
}
func noopSearchJob(ctx context.Context, jobID int64) error               { return nil }
func noopDeleteJob(ctx context.Context, jobID int64) error               { return nil }
func noopConversations(ctx context.Context) ([]core.Conversation, error) { return nil, nil }
func noopThread(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
	return nil, nil
}
func noopSendMessage(ctx context.Context, username, body string) (core.PrivateMessage, error) {
	return core.PrivateMessage{}, nil
}
func noopMarkRead(ctx context.Context, username string) (int, error) { return 0, nil }

// testServerDeps returns ServerDeps wired to noop implementations. Tests
// override only the fields they exercise.
func testServerDeps(reg *prometheus.Registry) ServerDeps {
	return ServerDeps{
		Registry:         reg,
		Status:           func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil },
		Jobs:             func(ctx context.Context) ([]core.JobView, error) { return nil, nil },
		PagedJobs:        noopPagedJobs,
		Cancel:           func(ctx context.Context, jobID int64) error { return nil },
		JobDetail:        noopJobDetail,
		JobView:          noopJobView,
		JobEvents:        noopJobEvents,
		RecentEvents:     noopRecentEvents,
		Peers:            noopPeers,
		Live:             noopHealthy,
		Modules:          noopModules,
		Retry:            noopRetry,
		FailedRetryAfter: testFailedRetryAfter,
		MaxCandidates:    testMaxCandidates,
		Config:           noopConfig,
		LiveTransfers:    noopLiveTransfers,
		TransferBytes:    noopTransferBytes,
		ConnectionTester: ConnectionTester{},
		Charts:           noopCharts,
		Shares:           noopShares,
		RescanShares:     noopRescanShares,
		Uploads:          noopUploads,
		Throughput:       noopThroughput,
		ConfigWriter:     noopConfigWriter,
		Restart:          noopRestart,
		CreateJob:        noopCreateJob,
		SearchJob:        noopSearchJob,
		DeleteJob:        noopDeleteJob,
		Conversations:    noopConversations,
		Thread:           noopThread,
		Send:             noopSendMessage,
		MarkRead:         noopMarkRead,
	}
}

// newTestHandler builds a NewServer with no-op status/jobs/cancel funcs, for
// tests that only care about routes unrelated to those three.
func newTestHandler(reg *prometheus.Registry) http.Handler {
	deps := testServerDeps(reg)
	return NewServer(deps)
}

func TestStatusEndpointReturnsParkedAndDeprecatedAlias(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) {
		return StatusReport{Queued: 3, Active: 1, Stalled: 0, Parked: 2}, nil
	}
	deps := testServerDeps(reg)
	deps.Status = status
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw JSON: %v", err)
	}
	for _, key := range []string{"parked", "orphaned"} {
		value, ok := raw[key]
		if !ok {
			t.Fatalf("raw JSON missing %q: %s", key, rec.Body.String())
		}
		var count int
		if err := json.Unmarshal(value, &count); err != nil || count != 2 {
			t.Errorf("%s = %s, want 2 (decode error %v)", key, value, err)
		}
	}
	var got StatusReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode StatusReport: %v", err)
	}
	if got.Queued != 3 || got.Parked != 2 {
		t.Errorf("unexpected report: %+v", got)
	}
}

func TestStatusEndpointReportsBuildVersion(t *testing.T) {
	reg := prometheus.NewRegistry()
	deps := testServerDeps(reg)
	deps.Version = "v1.2.3"
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", got.Version)
	}
}

// A caller with nothing to report must still produce valid JSON with the key
// present — the UI distinguishes "" (say nothing) from a missing field only by
// treating both as absent, and an omitted key would make that distinction
// depend on the encoder rather than on the deps.
func TestStatusEndpointVersionIsEmptyWhenUnset(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewServer(testServerDeps(reg))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := got["version"]
	if !ok {
		t.Fatal("version key missing from /status")
	}
	if v != "" {
		t.Errorf("version = %v, want empty", v)
	}
}

func TestStatusEndpointIncludesModuleRuntimeState(t *testing.T) {
	reg := prometheus.NewRegistry()
	attempted := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	completed := attempted.Add(4 * time.Second)
	succeeded := attempted.Add(-time.Minute)
	errored := completed
	staleDeadline := attempted.Add(90 * time.Second)
	modules := func() map[string]ModuleStatus {
		return map[string]ModuleStatus{
			"wanted_sync": {
				LastAttempt: attempted, LastCompleted: completed, LastSuccess: succeeded, LastErrorAt: errored,
				LastError: "temporary failure", ConsecutiveFailures: 2,
				StaleDeadline: staleDeadline, Live: true, Ready: true,
			},
			"discovery": {},
		}
	}
	deps := testServerDeps(reg)
	deps.Modules = modules
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	type moduleDetail struct {
		LastAttempt         string `json:"lastAttempt"`
		LastCompleted       string `json:"lastCompleted"`
		LastSuccess         string `json:"lastSuccess"`
		LastErrorAt         string `json:"lastErrorAt"`
		LastError           string `json:"lastError"`
		ConsecutiveFailures int    `json:"consecutiveFailures"`
		StaleDeadline       string `json:"staleDeadline"`
		Live                bool   `json:"live"`
		Ready               bool   `json:"ready"`
	}
	var got struct {
		Modules       map[string]string       `json:"modules"`
		ModuleDetails map[string]moduleDetail `json:"moduleDetails"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantCompatibility := map[string]string{
		"wanted_sync": completed.Format(timeFormat),
		"discovery":   "",
	}
	if !reflect.DeepEqual(got.Modules, wantCompatibility) {
		t.Errorf("modules compatibility map = %#v, want exactly %#v", got.Modules, wantCompatibility)
	}
	module := got.ModuleDetails["wanted_sync"]
	if module.LastAttempt != attempted.Format(timeFormat) || module.LastCompleted != completed.Format(timeFormat) || module.LastSuccess != succeeded.Format(timeFormat) || module.LastErrorAt != errored.Format(timeFormat) {
		t.Errorf("unexpected module timestamps: %+v", module)
	}
	if module.LastError != "temporary failure" || module.ConsecutiveFailures != 2 {
		t.Errorf("unexpected module failure state: %+v", module)
	}
	if module.StaleDeadline != staleDeadline.Format(timeFormat) || !module.Live || !module.Ready {
		t.Errorf("unexpected authoritative health: %+v", module)
	}
	if discovery := got.ModuleDetails["discovery"]; discovery.LastAttempt != "" || discovery.LastCompleted != "" {
		t.Errorf("never-attempted module should have empty timestamps: %+v", discovery)
	}
}

func TestHealthEndpointsSeparateLivenessAndReadiness(t *testing.T) {
	reg := prometheus.NewRegistry()

	live, ready := true, false
	deps := testServerDeps(reg)
	deps.Live = func() bool { return live }
	deps.Ready = func() bool { return ready }
	h := NewServer(deps)

	for _, tt := range []struct {
		path string
		want int
	}{{"/healthz", http.StatusOK}, {"/readyz", http.StatusServiceUnavailable}} {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s status = %d, want %d", tt.path, rec.Code, tt.want)
		}
	}

	live, ready = false, true
	for _, tt := range []struct {
		path string
		want int
	}{{"/healthz", http.StatusServiceUnavailable}, {"/readyz", http.StatusOK}} {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s status = %d, want %d", tt.path, rec.Code, tt.want)
		}
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ReconcileTotal.Inc()
	deps := testServerDeps(reg)
	deps.Status = func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
}

func TestPagedJobsEndpointReturnsPageAndFacets(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotQuery PagedJobsQuery
	deps := testServerDeps(reg)
	deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
		gotQuery = query
		return PagedJobsResult{
			Jobs: []core.JobView{{
				Job:    core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet", State: core.StateImporting, Source: core.SourceLidarr},
				Peer:   "flac_hoarder",
				Status: "importing",
			}},
			Total: 25,
			Facets: JobFacets{
				Status: JobStatusFacets{All: 30, Active: 3, Importing: 2, Queued: 4, Stalled: 5, Failed: 6, Parked: 7, Done: 3},
				Source: JobSourceFacets{All: 8, Manual: 2, Lidarr: 6},
			},
		}, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs?page=2&sort=album&dir=desc&filter=importing&source=lidarr&q=%20Four%20%20", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantQuery := PagedJobsQuery{Page: 2, Sort: "album", Dir: "desc", Filter: "importing", Source: "lidarr", Query: "Four", PageSize: jobsPageSize}
	if !reflect.DeepEqual(gotQuery, wantQuery) {
		t.Fatalf("query = %+v, want %+v", gotQuery, wantQuery)
	}
	var got struct {
		Jobs   []jobDTO  `json:"jobs"`
		Total  int64     `json:"total"`
		Facets JobFacets `json:"facets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].ID != 7 || got.Jobs[0].Status != "importing" || got.Jobs[0].State != string(core.StateImporting) {
		t.Errorf("jobs = %+v, want IMPORTING job serialized with importing status", got.Jobs)
	}
	if got.Total != 25 || got.Facets.Status.Importing != 2 || got.Facets.Source.Lidarr != 6 {
		t.Errorf("page metadata = total %d facets %+v", got.Total, got.Facets)
	}
}

func TestPagedJobsEndpointDefaultsAndEmptyJobsArray(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	var gotQuery PagedJobsQuery
	deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
		gotQuery = query
		return PagedJobsResult{Jobs: nil}, nil
	}
	h := NewServer(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))

	wantQuery := PagedJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all", PageSize: jobsPageSize}
	if !reflect.DeepEqual(gotQuery, wantQuery) {
		t.Fatalf("query = %+v, want %+v", gotQuery, wantQuery)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"jobs":[]`) {
		t.Fatalf("response = %d %s, want jobs as []", rec.Code, rec.Body.String())
	}
}

func TestPagedJobsEndpointRejectsInvalidQueries(t *testing.T) {
	invalid := []string{
		"?page=-1",
		"?page=not-a-number",
		"?page=999999999999999999999999",
		"?page=9223372036854775807",
		"?sort=nope",
		"?dir=sideways",
		"?filter=nope",
		"?source=nope",
		"?unknown=x",
		"?page=1&page=2",
		"?q=a&q=b",
		"?q=%zz",
		// Issue #268: pageSize bounds and the sort=transfer/dir=desc rejection.
		"?pageSize=0",
		"?pageSize=51",
		"?pageSize=-1",
		"?pageSize=not-a-number",
		"?pageSize=1&pageSize=2",
		"?sort=transfer&dir=desc",
		// Issue #287: transferring is gone now that inflight/finished replace it.
		"?filter=transferring",
	}
	for _, suffix := range invalid {
		t.Run(suffix, func(t *testing.T) {
			deps := testServerDeps(prometheus.NewRegistry())
			called := false
			deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
				called = true
				return PagedJobsResult{}, nil
			}
			rec := httptest.NewRecorder()
			NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs"+suffix, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if called {
				t.Fatal("PagedJobs called for invalid query")
			}
		})
	}
}

// TestPagedJobsEndpointParsesInflightFinishedAndFacets covers filter=inflight,
// filter=finished, sort=recent and the facets=0 opt-out reaching PagedJobsFunc
// as parsed.
func TestPagedJobsEndpointParsesInflightFinishedAndFacets(t *testing.T) {
	cases := []struct {
		suffix string
		want   PagedJobsQuery
	}{
		{
			suffix: "?filter=inflight&sort=transfer&dir=asc&pageSize=8",
			want:   PagedJobsQuery{Page: 0, Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 8},
		},
		{
			suffix: "?filter=finished&sort=recent&dir=desc&pageSize=5&facets=0",
			want:   PagedJobsQuery{Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 5, SkipFacets: true},
		},
		{
			// Overview's FAILED JOBS panel (issue #310). This case exists
			// because parsePagedJobsQuery keeps its own filter allowlist,
			// separate from store.validateDashboardJobsQuery's: #310 added
			// "failures" to the store's copy only, and every store and
			// frontend test still passed while the real endpoint answered
			// 400. Only a request that actually crosses the URL parser
			// catches that, so every filter Overview sends needs a case here.
			suffix: "?filter=failures&sort=recent&dir=desc&pageSize=8&facets=0",
			want:   PagedJobsQuery{Page: 0, Sort: "recent", Dir: "desc", Filter: "failures", Source: "all", PageSize: 8, SkipFacets: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.suffix, func(t *testing.T) {
			deps := testServerDeps(prometheus.NewRegistry())
			var got PagedJobsQuery
			deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
				got = query
				return PagedJobsResult{}, nil
			}
			rec := httptest.NewRecorder()
			NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs"+tc.suffix, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if got != tc.want {
				t.Fatalf("query = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPagedJobsEndpointRejectsRecentAscendingAndBadFacets covers the
// dir=asc/sort=recent conflict and every rejected shape of facets=.
func TestPagedJobsEndpointRejectsRecentAscendingAndBadFacets(t *testing.T) {
	invalid := []string{
		"?sort=recent&dir=asc",
		"?sort=recent", // dir defaults to asc, which sort=recent rejects
		"?facets=2",
		"?facets=yes",
		"?facets=",
		"?facets=0&facets=1",
	}
	for _, suffix := range invalid {
		t.Run(suffix, func(t *testing.T) {
			deps := testServerDeps(prometheus.NewRegistry())
			called := false
			deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
				called = true
				return PagedJobsResult{}, nil
			}
			rec := httptest.NewRecorder()
			NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs"+suffix, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if called {
				t.Fatal("PagedJobs called for invalid query")
			}
		})
	}
}

func TestPagedJobsEndpointReturns500OnStoreError(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
		return PagedJobsResult{}, errors.New("db exploded")
	}
	rec := httptest.NewRecorder()
	NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestAllJobsEndpointRemoved is issue #268: GET /api/jobs/all is gone —
// Overview and JobDetail were its only consumers, and both now request
// exactly what they need from GET /api/jobs and GET /api/jobs/{id}/detail
// respectively. deps.Jobs itself stays (the stream hub's viewByJob still
// needs it), so this only asserts the REST route no longer resolves.
func TestAllJobsEndpointRemoved(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	rec := httptest.NewRecorder()
	NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs/all", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route removed)", rec.Code)
	}
}

// pagedJobsFromFunc adapts a JobsFunc into a PagedJobsFunc that serves the
// whole result as one page (Total = len(views)), for tests migrated off the
// removed GET /api/jobs/all endpoint (issue #268) that only care about
// individual jobDTO fields, not pagination/facets behavior.
func pagedJobsFromFunc(jobs JobsFunc) PagedJobsFunc {
	return func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
		views, err := jobs(ctx)
		if err != nil {
			return PagedJobsResult{}, err
		}
		return PagedJobsResult{Jobs: views, Total: int64(len(views))}, nil
	}
}

// decodeJobsList decodes a GET /api/jobs response and returns just its Jobs
// slice, for tests that only care about individual jobDTO fields.
func decodeJobsList(t *testing.T, body []byte) []jobDTO {
	t.Helper()
	var resp struct {
		Jobs []jobDTO `json:"jobs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Jobs
}

// TestJobsEndpointReturnsJobList also verifies BytesDone/BytesTotal come
// from the JobView's album totals (AlbumBytesDone/AlbumBytesTotal) rather
// than the single latest Transfer — the fixture deliberately gives them
// different values so a regression that reverts to Transfer's numbers is
// caught (issue #174).
func TestJobsEndpointReturnsJobList(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:                 core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet", State: core.StateDownloading, Source: core.SourceLidarr},
				Status:              "active",
				Peer:                "flac_hoarder",
				AlbumBytesDone:      900,
				AlbumBytesTotal:     2400,
				AlbumBytesRemaining: 1500,
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	if len(got) != 1 {
		t.Fatalf("expected 1 job, got %d", len(got))
	}
	if got[0].ID != 7 || got[0].Title != "Rounds" || got[0].Artist != "Four Tet" {
		t.Errorf("unexpected job DTO: %+v", got[0])
	}
	if got[0].Status != "active" {
		t.Errorf("Status = %q, want active", got[0].Status)
	}
	if got[0].Peer != "flac_hoarder" {
		t.Errorf("Peer = %q, want flac_hoarder", got[0].Peer)
	}
	if got[0].BytesDone != 900 || got[0].BytesTotal != 2400 {
		t.Errorf("bytes = %d/%d, want 900/2400 (album totals, not the latest transfer's 100/200)", got[0].BytesDone, got[0].BytesTotal)
	}
	if got[0].State != "DOWNLOADING" {
		t.Errorf("State = %q, want DOWNLOADING", got[0].State)
	}
	if got[0].MaxCandidates != testMaxCandidates {
		t.Errorf("MaxCandidates = %d, want %d", got[0].MaxCandidates, testMaxCandidates)
	}
	if got[0].FailReason != "" {
		t.Errorf("FailReason = %q, want empty", got[0].FailReason)
	}
	if got[0].NextAttemptAt != "" {
		t.Errorf("NextAttemptAt = %q, want empty", got[0].NextAttemptAt)
	}
	if got[0].Source != "lidarr" {
		t.Errorf("Source = %q, want lidarr", got[0].Source)
	}
}

// TestToJobDTOOverlaysLiveBytesRegardlessOfState is issue #161, part 1b: the
// live in-memory BytesDone for a matched transfer wins over the persisted,
// up-to-15s-stale AlbumBytesDone — even when that live match is itself
// terminal (a just-completed file lingering until the next reconcile), which
// is exactly the case the old non-terminal-only overlay missed.
func TestToJobDTOOverlaysLiveBytesRegardlessOfState(t *testing.T) {
	candidate := &core.Candidate{
		ID:       1,
		Username: "alice",
		Files:    []core.CandidateFile{{Filename: "01.flac", Size: 1000}},
	}
	view := core.JobView{
		Job:             core.AlbumJob{ID: 1, State: core.StateDownloading, Source: core.SourceLidarr},
		Attempt:         candidate,
		AlbumBytesDone:  300,
		AlbumBytesTotal: 1000,
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000},
	})
	persisted := map[int64]map[string]int64{1: {"01.flac": 300}}

	dto := toJobDTO(view, testFailedRetryAfter, testMaxCandidates, live, persisted, testNow)
	if dto.BytesDone != 1000 {
		t.Errorf("BytesDone = %d, want 1000 (live overlay wins even though the match is terminal)", dto.BytesDone)
	}
	if dto.BytesTotal != 1000 {
		t.Errorf("BytesTotal = %d, want 1000 (never overlaid, see jobDTO.BytesTotal)", dto.BytesTotal)
	}
}

// TestToJobDTOFallsBackToPersistedWithoutLiveMatch covers a candidate with
// no live data at all (e.g. LiveTransfers failed, or the peer backend
// restarted): the persisted AlbumBytesDone must be served unmodified.
func TestToJobDTOFallsBackToPersistedWithoutLiveMatch(t *testing.T) {
	candidate := &core.Candidate{
		ID:       1,
		Username: "alice",
		Files:    []core.CandidateFile{{Filename: "01.flac", Size: 1000}},
	}
	view := core.JobView{
		Job:             core.AlbumJob{ID: 1, State: core.StateDownloading, Source: core.SourceLidarr},
		Attempt:         candidate,
		AlbumBytesDone:  300,
		AlbumBytesTotal: 1000,
	}

	dto := toJobDTO(view, testFailedRetryAfter, testMaxCandidates, liveTransferIndex{}, nil, testNow)
	if dto.BytesDone != 300 {
		t.Errorf("BytesDone = %d, want 300 (persisted fallback, no live match)", dto.BytesDone)
	}
}

// TestToJobDTOSumsPersistedForUnmatchedFilesInLiveMatchedCandidate covers a
// multi-file album where one file has a live match and another does not
// (backend just restarted, or not yet enqueued): the unmatched file's exact
// persisted bytes (from the TransferBytes dep) must still be counted,
// alongside the matched file's live bytes.
func TestToJobDTOSumsPersistedForUnmatchedFilesInLiveMatchedCandidate(t *testing.T) {
	candidate := &core.Candidate{
		ID:       1,
		Username: "alice",
		Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000}, // live match
			{Filename: "02.flac", Size: 1000}, // no live match, persisted-final
		},
	}
	view := core.JobView{
		Job:             core.AlbumJob{ID: 1, State: core.StateDownloading, Source: core.SourceLidarr},
		Attempt:         candidate,
		AlbumBytesDone:  1500, // stale: 500 (file1) + 1000 (file2)
		AlbumBytesTotal: 2000,
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 750},
	})
	persisted := map[int64]map[string]int64{1: {"01.flac": 500, "02.flac": 1000}}

	dto := toJobDTO(view, testFailedRetryAfter, testMaxCandidates, live, persisted, testNow)
	if dto.BytesDone != 750+1000 {
		t.Errorf("BytesDone = %d, want %d (750 live + 1000 persisted)", dto.BytesDone, 750+1000)
	}
}

// TestToJobDTOSetsFramedAtFromCallerSuppliedNow covers issue #285: FramedAt
// must be exactly the caller-supplied now, formatted the same way as
// UpdatedAt/CreatedAt — not derived internally from time.Now() and not left
// as the DB row's UpdatedAt.
func TestToJobDTOSetsFramedAtFromCallerSuppliedNow(t *testing.T) {
	view := core.JobView{
		Job: core.AlbumJob{ID: 1, State: core.StateDownloading, Source: core.SourceLidarr},
	}
	dto := toJobDTO(view, testFailedRetryAfter, testMaxCandidates, liveTransferIndex{}, nil, testNow)
	if want := testNow.Format(timeFormat); dto.FramedAt != want {
		t.Errorf("FramedAt = %q, want %q", dto.FramedAt, want)
	}
}

// TestJobsEndpointReturnsCandidateMetadata verifies year/tracks/format
// serialize when set (a job past selection) and as JSON null when unset (a
// job with no candidate yet), see issue #156.
func TestJobsEndpointReturnsCandidateMetadata(t *testing.T) {
	reg := prometheus.NewRegistry()
	year := 2024
	tracks := 12
	format := "FLAC"
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 9, Title: "With Metadata", ArtistName: "Someone", State: core.StateDownloading,
					Year: &year, Tracks: &tracks, Format: &format,
				},
			},
			{
				Job: core.AlbumJob{ID: 10, Title: "No Candidate Yet", ArtistName: "Nobody", State: core.StateWanted},
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(got))
	}
	if got[0].Year == nil || *got[0].Year != 2024 {
		t.Errorf("Year = %v, want 2024", got[0].Year)
	}
	if got[0].Tracks == nil || *got[0].Tracks != 12 {
		t.Errorf("Tracks = %v, want 12", got[0].Tracks)
	}
	if got[0].Format == nil || *got[0].Format != "FLAC" {
		t.Errorf("Format = %v, want FLAC", got[0].Format)
	}
	if got[1].Year != nil || got[1].Tracks != nil || got[1].Format != nil {
		t.Errorf("expected nil year/tracks/format for job without candidate, got %+v", got[1])
	}
}

func TestJobsEndpointReturnsFailReasonAndNextAttemptForFailedJob(t *testing.T) {
	reg := prometheus.NewRegistry()
	updatedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 8, Title: "Doomed", ArtistName: "Nobody",
					State: core.StateFailed, CandidatesTried: 5, UpdatedAt: updatedAt,
				},
				Attempt: &core.Candidate{FailReason: "transfer failed"},
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := decodeJobsList(t, rec.Body.Bytes())
	if got[0].FailReason != "transfer failed" {
		t.Errorf("FailReason = %q, want %q", got[0].FailReason, "transfer failed")
	}
	if got[0].CandidatesTried != 5 {
		t.Errorf("CandidatesTried = %d, want 5", got[0].CandidatesTried)
	}
	wantNextAttempt := updatedAt.Add(testFailedRetryAfter).Format(timeFormat)
	if got[0].NextAttemptAt != wantNextAttempt {
		t.Errorf("NextAttemptAt = %q, want %q", got[0].NextAttemptAt, wantNextAttempt)
	}
}

// TestJobsEndpointReturnsFailDetailForFailedJob is issue #310: a failed job's
// jobDTO should carry the pipeline's own recorded failure explanation, not
// just the candidate's generic FailReason.
func TestJobsEndpointReturnsFailDetailForFailedJob(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:    core.AlbumJob{ID: 9, Title: "Doomed Again", ArtistName: "Nobody"},
				Status: "failed",
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.FailureDetails = func(ctx context.Context, jobIDs []int64) (map[int64]string, error) {
		if len(jobIDs) != 1 || jobIDs[0] != 9 {
			t.Fatalf("FailureDetails called with %v, want [9]", jobIDs)
		}
		return map[int64]string{9: "Lidarr rejected: track count mismatch"}, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := decodeJobsList(t, rec.Body.Bytes())
	if got[0].FailDetail != "Lidarr rejected: track count mismatch" {
		t.Errorf("FailDetail = %q, want %q", got[0].FailDetail, "Lidarr rejected: track count mismatch")
	}
}

// TestJobsEndpointToleratesNilAndErroringFailureDetails is issue #310: a nil
// FailureDetails dep (every pre-existing ServerDeps) and one that errors must
// both still produce a 200 with the rest of the payload intact — the
// enrichment is best-effort, exactly like LiveTransfers.
func TestJobsEndpointToleratesNilAndErroringFailureDetails(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{Job: core.AlbumJob{ID: 9, Title: "Doomed Again", ArtistName: "Nobody"}, Status: "failed"},
		}, nil
	}

	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.FailureDetails = nil
	h := NewServer(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil FailureDetails: status = %d, want 200", rec.Code)
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	if got[0].FailDetail != "" || got[0].Title != "Doomed Again" {
		t.Errorf("nil FailureDetails: got %+v", got[0])
	}

	deps2 := testServerDeps(reg)
	deps2.PagedJobs = pagedJobsFromFunc(jobs)
	deps2.FailureDetails = func(ctx context.Context, jobIDs []int64) (map[int64]string, error) {
		return nil, errors.New("boom")
	}
	h2 := NewServer(deps2)
	req2 := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("erroring FailureDetails: status = %d, want 200", rec2.Code)
	}
	got2 := decodeJobsList(t, rec2.Body.Bytes())
	if got2[0].FailDetail != "" || got2[0].Title != "Doomed Again" {
		t.Errorf("erroring FailureDetails: got %+v", got2[0])
	}
}

// TestJobsEndpointAssignsFailDetailToTheCorrectJobInAMixedPage is issue #310's
// review follow-up: every prior FailDetail test seeds exactly one job, and it
// is failed, so a mutant that writes FailDetail by looping over failedIDs and
// indexing straight into dtos (rather than looking each dto up by its own
// job ID, as enrichJobDTOs actually does) still passes them. Here a
// NON-failed job is ordered BEFORE the failed one, so the two loops (over all
// views vs. over failed-only IDs) walk different indices — the buggy form
// would write the failed job's detail onto the non-failed job's row.
func TestJobsEndpointAssignsFailDetailToTheCorrectJobInAMixedPage(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{Job: core.AlbumJob{ID: 1, Title: "Still Going", ArtistName: "Someone"}, Status: "active"},
			{Job: core.AlbumJob{ID: 2, Title: "Doomed", ArtistName: "Nobody"}, Status: "failed"},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.FailureDetails = func(ctx context.Context, jobIDs []int64) (map[int64]string, error) {
		if len(jobIDs) != 1 || jobIDs[0] != 2 {
			t.Fatalf("FailureDetails called with %v, want [2]", jobIDs)
		}
		return map[int64]string{2: "Lidarr rejected: track count mismatch"}, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := decodeJobsList(t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(got))
	}
	if got[0].ID != 1 || got[0].FailDetail != "" {
		t.Errorf("non-failed job (index 0) = %+v, want FailDetail empty", got[0])
	}
	if got[1].ID != 2 || got[1].FailDetail != "Lidarr rejected: track count mismatch" {
		t.Errorf("failed job (index 1) = %+v, want FailDetail %q", got[1], "Lidarr rejected: track count mismatch")
	}
}

// TestJobsEndpointSerializesFailDetailUnderItsWireKey is issue #310's review
// follow-up: decodeJobsList unmarshals into jobDTO itself, so every other
// FailDetail test round-trips the struct tag against itself and would stay
// green even if the tag were renamed away from what web/src/api/types.ts
// actually expects. This decodes the raw response body generically instead,
// so it checks the literal wire key.
func TestJobsEndpointSerializesFailDetailUnderItsWireKey(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{Job: core.AlbumJob{ID: 9, Title: "Doomed Again", ArtistName: "Nobody"}, Status: "failed"},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.FailureDetails = func(ctx context.Context, jobIDs []int64) (map[int64]string, error) {
		return map[int64]string{9: "Lidarr rejected: track count mismatch"}, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var raw struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(raw.Jobs))
	}
	if got, want := raw.Jobs[0]["failDetail"], "Lidarr rejected: track count mismatch"; got != want {
		t.Errorf(`wire key "failDetail" = %v, want %q`, got, want)
	}
}

// CreatedAt must reflect core.AlbumJob.CreatedAt distinctly from UpdatedAt —
// the frontend sorts the TRANSFERS panel by createdAt specifically because it
// does NOT change on progress/state updates (#233), so this guards against
// the two fields accidentally being wired to the same source timestamp.
func TestJobsEndpointReturnsCreatedAtDistinctFromUpdatedAt(t *testing.T) {
	reg := prometheus.NewRegistry()
	createdAt := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 10, Title: "Started Earlier", ArtistName: "Someone",
					State: core.StateDownloading, CreatedAt: createdAt, UpdatedAt: updatedAt,
				},
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := decodeJobsList(t, rec.Body.Bytes())
	wantCreatedAt := createdAt.Format(timeFormat)
	wantUpdatedAt := updatedAt.Format(timeFormat)
	if got[0].CreatedAt != wantCreatedAt {
		t.Errorf("CreatedAt = %q, want %q", got[0].CreatedAt, wantCreatedAt)
	}
	if got[0].UpdatedAt != wantUpdatedAt {
		t.Errorf("UpdatedAt = %q, want %q", got[0].UpdatedAt, wantUpdatedAt)
	}
}

func TestJobsEndpointReturnsRetriesAndNotBeforeForWantedJob(t *testing.T) {
	reg := prometheus.NewRegistry()
	notBefore := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 9, Title: "Waiting", ArtistName: "Someone",
					State: core.StateWanted, Retries: 2, NotBefore: &notBefore,
				},
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := decodeJobsList(t, rec.Body.Bytes())
	wantNotBefore := notBefore.Format(timeFormat)
	if got[0].NotBefore != wantNotBefore {
		t.Errorf("NotBefore = %q, want %q", got[0].NotBefore, wantNotBefore)
	}
	if got[0].Retries != 2 {
		t.Errorf("Retries = %d, want 2", got[0].Retries)
	}
	if got[0].NextAttemptAt != "" {
		t.Errorf("NextAttemptAt = %q, want empty (only set for FAILED jobs)", got[0].NextAttemptAt)
	}
	if got[0].FailReason != "" {
		t.Errorf("FailReason = %q, want empty", got[0].FailReason)
	}
}

func TestJobsEndpointReturns500OnStoreError(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, errors.New("db exploded") }
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

// TestJobsEndpointIncludesLiveAlbumSpeedQueuePositionAndETA asserts a job
// whose candidate has a matching live transfer gets queuePosition/speed
// populated from LiveTransfers (issue #157), and etaSeconds computed from the
// store-provided AlbumBytesRemaining (issue #174) combined with that live
// average speed.
func TestJobsEndpointIncludesLiveAlbumSpeedQueuePositionAndETA(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:     core.AlbumJob{ID: 1, Title: "Live One", ArtistName: "X", State: core.StateDownloading},
				Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
				// Deliberately different from the live transfer's own
				// remaining (Size 1000 - BytesDone 500 = 500): if etaSeconds
				// were computed from the live transfer's remaining instead of
				// this store-provided value, this test would still pass at
				// eta = 5, silently losing coverage of issue #174.
				AlbumBytesRemaining: 900,
			},
		}, nil
	}
	live := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: 200, SpeedAverage: 100, Size: 1000, BytesDone: 500, QueuePosition: 4},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.LiveTransfers = live
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	if got[0].Speed != 200 {
		t.Errorf("Speed = %d, want 200", got[0].Speed)
	}
	if got[0].QueuePosition != 4 {
		t.Errorf("QueuePosition = %d, want 4", got[0].QueuePosition)
	}
	// remaining (store AlbumBytesRemaining) = 900, avgSpeed (live) = 100 -> eta = 9s.
	if got[0].ETASeconds != 9 {
		t.Errorf("ETASeconds = %d, want 9", got[0].ETASeconds)
	}
}

// TestJobsEndpointETAUsesAlbumBytesRemainingNotLiveOnly asserts etaSeconds
// accounts for bytes not yet released to the peer backend by the per-peer
// throttle (issue #20): AlbumBytesRemaining is larger than the sum of live
// transfers' own remaining bytes, and etaSeconds must reflect the larger,
// store-provided figure rather than only the live transfer's 500 remaining
// bytes (issue #174).
func TestJobsEndpointETAUsesAlbumBytesRemainingNotLiveOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{ID: 1, Title: "Throttled Album", ArtistName: "X", State: core.StateDownloading},
				Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{
					{Filename: "01.flac", Size: 1000},
					{Filename: "02.flac", Size: 1000}, // not yet enqueued: no live entry
				}},
				// Album-wide remaining across every file, including the
				// not-yet-enqueued 02.flac (1000 bytes untouched) plus
				// 01.flac's own 500 remaining.
				AlbumBytesRemaining: 1500,
			},
		}, nil
	}
	live := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: 200, SpeedAverage: 100, Size: 1000, BytesDone: 500, QueuePosition: 4},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.LiveTransfers = live
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	// remaining = 1500 (album-wide, store), avgSpeed = 100 (live) -> eta = 15s,
	// not 5s (which is what a live-only remaining of 500 would give).
	if got[0].ETASeconds != 15 {
		t.Errorf("ETASeconds = %d, want 15 (album-wide remaining, not the live-only 500)", got[0].ETASeconds)
	}
}

// TestJobsEndpointOmitsLiveFieldsWhenNoMatch asserts a job with no matching
// live transfer serves queuePosition/speed/etaSeconds absent entirely (raw
// JSON check, since omitempty and a zero value are indistinguishable once
// decoded into a Go struct).
func TestJobsEndpointOmitsLiveFieldsWhenNoMatch(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{Job: core.AlbumJob{ID: 1, Title: "No Live Data", ArtistName: "X", State: core.StateDownloading}},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.LiveTransfers = noopLiveTransfers
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, field := range []string{`"queuePosition"`, `"speed"`, `"etaSeconds"`} {
		if strings.Contains(body, field) {
			t.Errorf("expected %s absent from body, got %s", field, body)
		}
	}
}

// TestJobsEndpointDegradesUnenrichedWhenLiveTransfersErrors asserts a
// LiveTransfers failure still serves 200 with unenriched jobs, matching the
// job detail endpoint's own best-effort enrichment contract.
func TestJobsEndpointDegradesUnenrichedWhenLiveTransfersErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:     core.AlbumJob{ID: 1, Title: "Live One", ArtistName: "X", State: core.StateDownloading},
				Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return nil, errors.New("listdownloads boom")
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (best-effort degrade), body = %s", rec.Code, rec.Body.String())
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	if got[0].Speed != 0 || got[0].QueuePosition != 0 || got[0].ETASeconds != 0 {
		t.Errorf("expected unenriched job on LiveTransfers error, got %+v", got[0])
	}
}

// TestJobsEndpointBytesDoneUsesTransferBytesForLiveMatchedCandidate is the
// HTTP-level counterpart of TestToJobDTOSumsPersistedForUnmatchedFilesInLiveMatchedCandidate:
// it proves the /api/jobs handler actually collects the live-matched
// candidate ids, calls deps.TransferBytes with exactly those, and threads the
// result through to BytesDone (issue #161). A second, live-unmatched job is
// included to prove TransferBytes is called with only the matched candidate's
// id, not every candidate in the list.
func TestJobsEndpointBytesDoneUsesTransferBytesForLiveMatchedCandidate(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:             core.AlbumJob{ID: 1, Title: "Live", ArtistName: "X", State: core.StateDownloading},
				Attempt:         &core.Candidate{ID: 11, Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}, {Filename: "02.flac", Size: 1000}}},
				AlbumBytesDone:  1500,
				AlbumBytesTotal: 2000,
			},
			{
				Job:             core.AlbumJob{ID: 2, Title: "NotLive", ArtistName: "Y", State: core.StateDownloading},
				Attempt:         &core.Candidate{ID: 22, Username: "bob", Files: []core.CandidateFile{{Filename: "b1.flac", Size: 500}}},
				AlbumBytesDone:  200,
				AlbumBytesTotal: 500,
			},
		}, nil
	}
	live := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000},
		}, nil
	}
	var gotIDs []int64
	transferBytes := func(ctx context.Context, candidateIDs []int64) (map[int64]map[string]int64, error) {
		gotIDs = candidateIDs
		return map[int64]map[string]int64{11: {"01.flac": 500, "02.flac": 1000}}, nil
	}
	deps := testServerDeps(reg)
	deps.PagedJobs = pagedJobsFromFunc(jobs)
	deps.LiveTransfers = live
	deps.TransferBytes = transferBytes
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(gotIDs) != 1 || gotIDs[0] != 11 {
		t.Fatalf("TransferBytes called with %v, want [11] (only the live-matched candidate)", gotIDs)
	}
	got := decodeJobsList(t, rec.Body.Bytes())
	if got[0].BytesDone != 1000+1000 {
		t.Errorf("job 1 BytesDone = %d, want %d (1000 live 01.flac + 1000 persisted 02.flac)", got[0].BytesDone, 1000+1000)
	}
	if got[1].BytesDone != 200 {
		t.Errorf("job 2 BytesDone = %d, want 200 (no live match, AlbumBytesDone unmodified)", got[1].BytesDone)
	}
}

func TestCreateJobEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotTitle, gotArtist, gotPeer string
	var gotFiles []core.CandidateFile
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
		gotTitle, gotArtist, gotPeer, gotFiles = title, artist, peer, files
		return core.JobView{
			Job:             core.AlbumJob{ID: 42, Title: title, ArtistName: artist, State: core.StateDownloading, Source: core.SourceManual},
			Peer:            "persisted_peer",
			AlbumBytesDone:  0,
			AlbumBytesTotal: 111,
		}, nil
	}
	deps := testServerDeps(reg)
	deps.CreateJob = create
	h := NewServer(deps)

	body := `{"title":"Some Album","artist":"Some Artist","peer":"flac_hoarder","files":[{"filename":"a.flac","size":111}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 42 || got.Source != "manual" {
		t.Errorf("unexpected job DTO: %+v", got)
	}
	if got.Peer != "persisted_peer" {
		t.Errorf("Peer = %q, want persisted_peer from the canonical view", got.Peer)
	}
	if got.BytesDone != 0 || got.BytesTotal != 111 {
		t.Errorf("bytes = %d/%d, want persisted aggregate 0/111", got.BytesDone, got.BytesTotal)
	}
	if gotTitle != "Some Album" || gotArtist != "Some Artist" || gotPeer != "flac_hoarder" {
		t.Errorf("create called with title=%q artist=%q peer=%q", gotTitle, gotArtist, gotPeer)
	}
	if len(gotFiles) != 1 || gotFiles[0].Filename != "a.flac" || gotFiles[0].Size != 111 {
		t.Errorf("create called with files=%+v", gotFiles)
	}
}

func TestCreateJobEndpointMissingPeerReturns422(t *testing.T) {
	reg := prometheus.NewRegistry()
	called := false
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
		called = true
		return core.JobView{}, nil
	}
	deps := testServerDeps(reg)
	deps.CreateJob = create
	h := NewServer(deps)

	body := `{"files":[{"filename":"a.flac","size":111}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.FieldErrors["peer"]; !ok {
		t.Errorf("fieldErrors = %+v, want a \"peer\" entry", got.FieldErrors)
	}
	if called {
		t.Error("create must not be called on a validation failure")
	}
}

func TestCreateJobEndpointEmptyFilesReturns422(t *testing.T) {
	reg := prometheus.NewRegistry()
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
		return core.JobView{}, nil
	}
	deps := testServerDeps(reg)
	deps.CreateJob = create
	h := NewServer(deps)

	body := `{"peer":"flac_hoarder","files":[]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.FieldErrors["files"]; !ok {
		t.Errorf("fieldErrors = %+v, want a \"files\" entry", got.FieldErrors)
	}
}

func TestCreateJobEndpointMalformedJSONReturns400(t *testing.T) {
	reg := prometheus.NewRegistry()
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
		return core.JobView{}, nil
	}
	deps := testServerDeps(reg)
	deps.CreateJob = create
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateJobEndpointRemoteFileBusyReturns409(t *testing.T) {
	reg := prometheus.NewRegistry()
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
		return core.JobView{}, app.ErrRemoteFileBusy
	}
	deps := testServerDeps(reg)
	deps.CreateJob = create
	h := NewServer(deps)

	body := `{"peer":"flac_hoarder","files":[{"filename":"a.flac","size":111}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCancelEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotID int64
	cancel := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	deps := testServerDeps(reg)
	deps.Cancel = cancel
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("cancel called with id %d, want 42", gotID)
	}
}

func TestCancelEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	cancel := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	deps := testServerDeps(reg)
	deps.Cancel = cancel
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestCancelEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	cancel := func(ctx context.Context, jobID int64) error {
		return errors.New("advance failed")
	}
	deps := testServerDeps(reg)
	deps.Cancel = cancel
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status code = %d, want 502", rec.Code)
	}
}

func TestCancelEndpointBadID(t *testing.T) {
	reg := prometheus.NewRegistry()
	deps := testServerDeps(reg)
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestRetryEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotID int64
	retry := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	deps := testServerDeps(reg)
	deps.Retry = retry
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("retry called with id %d, want 42", gotID)
	}
}

func TestRetryEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	retry := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	deps := testServerDeps(reg)
	deps.Retry = retry
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestRetryEndpointConflictWhenNotFailed(t *testing.T) {
	reg := prometheus.NewRegistry()
	retry := func(ctx context.Context, jobID int64) error { return app.ErrJobNotRetryable }
	deps := testServerDeps(reg)
	deps.Retry = retry
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
	if got, want := rec.Body.String(), "job is not FAILED or PARKED\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestRetryEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	retry := func(ctx context.Context, jobID int64) error {
		return errors.New("db exploded")
	}
	deps := testServerDeps(reg)
	deps.Retry = retry
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestRetryEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestRetryEndpointMethodNotAllowed(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/1/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want 405", rec.Code)
	}
}

func TestSearchEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotID int64
	search := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	deps := testServerDeps(reg)
	deps.SearchJob = search
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("search called with id %d, want 42", gotID)
	}
}

func TestSearchEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	search := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	deps := testServerDeps(reg)
	deps.SearchJob = search
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestSearchEndpointConflictWhenActive(t *testing.T) {
	reg := prometheus.NewRegistry()
	search := func(ctx context.Context, jobID int64) error { return app.ErrJobActive }
	deps := testServerDeps(reg)
	deps.SearchJob = search
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
}

// TestSearchEndpointConflictWhenNotSearchable covers issue #347: a manual
// job cannot be force-searched.
func TestSearchEndpointConflictWhenNotSearchable(t *testing.T) {
	reg := prometheus.NewRegistry()
	search := func(ctx context.Context, jobID int64) error { return app.ErrJobNotSearchable }
	deps := testServerDeps(reg)
	deps.SearchJob = search
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
}

func TestSearchEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	search := func(ctx context.Context, jobID int64) error {
		return errors.New("db exploded")
	}
	deps := testServerDeps(reg)
	deps.SearchJob = search
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestSearchEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestSearchEndpointMethodNotAllowed(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want 405", rec.Code)
	}
}

func TestDeleteEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotID int64
	del := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	deps := testServerDeps(reg)
	deps.DeleteJob = del
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/42", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("delete called with id %d, want 42", gotID)
	}
}

func TestDeleteEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	del := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	deps := testServerDeps(reg)
	deps.DeleteJob = del
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/999", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestDeleteEndpointConflictWhenImporting(t *testing.T) {
	reg := prometheus.NewRegistry()
	del := func(ctx context.Context, jobID int64) error { return app.ErrJobImporting }
	deps := testServerDeps(reg)
	deps.DeleteJob = del
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
}

func TestDeleteEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	del := func(ctx context.Context, jobID int64) error {
		return errors.New("db exploded")
	}
	deps := testServerDeps(reg)
	deps.DeleteJob = del
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestDeleteEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/not-a-number", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestJobDetailEndpointReturnsDetail(t *testing.T) {
	reg := prometheus.NewRegistry()
	lastProgress := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	jobDetail := func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{
			Job: core.AlbumJob{ID: jobID, Title: "Rounds", ArtistName: "Four Tet", State: core.StateFailed},
			Attempts: []core.AttemptDetail{
				{
					Attempt: core.Candidate{ID: 1, Username: "peer_one", State: core.CandidateFailed, FailReason: "transfer failed"},
					Transfers: []core.Transfer{
						{Filename: "01.flac", State: core.TransferErrored, BytesDone: 10, BytesTotal: 100, Retries: 2, LastProgressAt: &lastProgress},
					},
				},
			},
		}, true, nil
	}
	deps := testServerDeps(reg)
	deps.JobDetail = jobDetail
	deps.JobView = func(ctx context.Context, jobID int64) (core.JobView, bool, error) {
		return core.JobView{Job: core.AlbumJob{ID: jobID, Title: "Rounds", ArtistName: "Four Tet", State: core.StateFailed}}, true, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/7/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got jobDetailDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Job.ID != 7 || got.Job.Title != "Rounds" || got.Job.Artist != "Four Tet" {
		t.Errorf("unexpected job detail: %+v", got)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(got.Attempts))
	}
	a := got.Attempts[0]
	if a.Username != "peer_one" || a.FailReason != "transfer failed" || a.FileCount != 1 {
		t.Errorf("unexpected attempt: %+v", a)
	}
	if len(a.Transfers) != 1 || a.Transfers[0].Filename != "01.flac" || a.Transfers[0].Retries != 2 {
		t.Errorf("unexpected transfers: %+v", a.Transfers)
	}
	if a.Transfers[0].LastProgressAt != lastProgress.Format(timeFormat) {
		t.Errorf("LastProgressAt = %q, want %q", a.Transfers[0].LastProgressAt, lastProgress.Format(timeFormat))
	}
}

// TestJobDetailEndpointBodyNestsJobNotFlattened locks in the wire contract
// decided for issue #268: the response is { "job": {...}, "attempts": [...] },
// never a flattened union of jobDTO's and jobDetailDTO's own fields at the
// same level. jobDetailDTO nests jobDTO as a named Go field specifically to
// get this shape — encoding/json would instead flatten an EMBEDDED jobDTO's
// fields to the top level, which silently produces this same wrong shape
// while still compiling and still decoding correctly into a jobDetailDTO
// (since Go's decoder also promotes embedded fields), so neither the Go type
// checker nor a decode-based test catches that mistake — only a raw-JSON
// assertion like this one does. The frontend (commit 1223326) reads
// `detail.job` exclusively; a flattened body leaves that undefined.
func TestJobDetailEndpointBodyNestsJobNotFlattened(t *testing.T) {
	reg := prometheus.NewRegistry()
	deps := testServerDeps(reg)
	deps.JobDetail = func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{Job: core.AlbumJob{ID: jobID}}, true, nil
	}
	deps.JobView = func(ctx context.Context, jobID int64) (core.JobView, bool, error) {
		return core.JobView{Job: core.AlbumJob{ID: jobID, Title: "Rounds", ArtistName: "Four Tet"}}, true, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/7/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["attempts"]; !ok {
		t.Fatalf("body missing top-level \"attempts\": %s", rec.Body.String())
	}
	jobRaw, ok := raw["job"]
	if !ok {
		t.Fatalf("body missing nested \"job\" object — got a flattened shape instead: %s", rec.Body.String())
	}
	for _, flattened := range []string{"id", "title", "artist", "status", "state"} {
		if _, ok := raw[flattened]; ok {
			t.Errorf("body must not carry %q at the top level (flattened alongside \"job\"): %s", flattened, rec.Body.String())
		}
	}
	var job map[string]json.RawMessage
	if err := json.Unmarshal(jobRaw, &job); err != nil {
		t.Fatalf("unmarshal nested job: %v", err)
	}
	if _, ok := job["id"]; !ok {
		t.Errorf(`nested "job" object missing "id": %s`, jobRaw)
	}
	if string(job["title"]) != `"Rounds"` {
		t.Errorf(`nested "job".title = %s, want "Rounds"`, job["title"])
	}
}

func TestJobDetailEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	jobDetail := func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{}, false, nil
	}
	deps := testServerDeps(reg)
	deps.JobDetail = jobDetail
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/999/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

// TestJobDetailEndpointNotFoundWhenViewMissesAfterDetailFound is issue
// #268's race: JobDetail found the job, but the separate JobView lookup for
// the same id (needed to build the embedded jobDTO header) reports not
// found — e.g. the job was deleted between the two queries. Must 404, not
// serve a body with no header.
func TestJobDetailEndpointNotFoundWhenViewMissesAfterDetailFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	deps := testServerDeps(reg)
	deps.JobDetail = func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{Job: core.AlbumJob{ID: jobID}}, true, nil
	}
	deps.JobView = noopJobView // reports not found for any id
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/7/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestJobDetailEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/not-a-number/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestJobEventsEndpointReturnsEvents(t *testing.T) {
	reg := prometheus.NewRegistry()
	when := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobEvents := func(ctx context.Context, jobID int64) ([]core.JobEvent, error) {
		return []core.JobEvent{
			{ID: 1, AlbumJobID: jobID, Event: core.EventSearch, Detail: "searched album", CreatedAt: when},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.JobEvents = jobEvents
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/3/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []eventDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Event != "search" || got[0].JobID != 3 {
		t.Errorf("unexpected events: %+v", got)
	}
}

func TestEventsEndpointDefaultLimit(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotLimit int
	recentEvents := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		gotLimit = limit
		return nil, nil
	}
	deps := testServerDeps(reg)
	deps.RecentEvents = recentEvents
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotLimit != eventsLimitDefault {
		t.Errorf("limit = %d, want default %d", gotLimit, eventsLimitDefault)
	}
}

func TestEventsEndpointClampsLimit(t *testing.T) {
	reg := prometheus.NewRegistry()
	var gotLimit int
	recentEvents := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		gotLimit = limit
		return nil, nil
	}
	deps := testServerDeps(reg)
	deps.RecentEvents = recentEvents
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=99999", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotLimit != eventsLimitMax {
		t.Errorf("limit = %d, want clamped max %d", gotLimit, eventsLimitMax)
	}
}

func TestPeersEndpointReturnsPeersWithScore(t *testing.T) {
	reg := prometheus.NewRegistry()
	now := time.Now()
	peers := func(ctx context.Context) ([]core.PeerRow, error) {
		return []core.PeerRow{
			{
				Username: "reliable_peer",
				Global:   core.ReliabilityCounters{SuccessCount: 5, LastSuccessAt: &now},
				Artists:  map[int64]core.ReliabilityCounters{1: {SuccessCount: 2, LastSuccessAt: &now}},
			},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.Peers = peers
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/peers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []peerDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Username != "reliable_peer" {
		t.Fatalf("unexpected peers: %+v", got)
	}
	if got[0].Score <= 0.5 {
		t.Errorf("Score = %v, want > 0.5 (peer has only successes)", got[0].Score)
	}
	if len(got[0].Artists) != 1 || got[0].Artists[0].ArtistID != 1 {
		t.Errorf("unexpected artist breakdown: %+v", got[0].Artists)
	}
}

// TestMuxDoesNotSwallowUnregisteredAPIPaths exercises the real ServeMux built
// by NewServer, not the asset handler directly. It guards against a
// regression where a future "/api/" prefix handler swallows unregistered API
// paths and answers them with the SPA shell instead of 404 — assets_test.go
// only ever calls newAssetHandler() in isolation, so it can't catch that.
func TestMuxDoesNotSwallowUnregisteredAPIPaths(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, must not be HTML for an unregistered API path", ct)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/: Content-Type = %q, want text/html", ct)
	}
}
