package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// scrape returns the /metrics body served from reg.
func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	deps := testServerDeps(reg)
	h := NewServer(deps)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

// hasMetricLine reports whether the scrape contains a sample line starting with
// prefix and ending with value.
func hasMetricLine(scraped, prefix, value string) bool {
	for _, line := range strings.Split(scraped, "\n") {
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, " "+value) {
			return true
		}
	}
	return false
}

func TestMetricsExposesJobProgressGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.SetJobProgress(JobProgressReport{
		States: []JobProgressState{
			{State: "DOWNLOADING", Count: 2, OldestUpdateAge: 90 * time.Second},
			{State: "IMPORTING", Count: 1, OldestUpdateAge: 30 * time.Second},
		},
		JobsWithoutActiveCandidate: 3,
	})

	body := scrape(t, reg)
	for _, want := range []struct{ prefix, value string }{
		{`slusk_jobs_in_state{state="DOWNLOADING"}`, "2"},
		{`slusk_jobs_in_state{state="IMPORTING"}`, "1"},
		{`slusk_job_oldest_update_age_seconds{state="DOWNLOADING"}`, "90"},
		{`slusk_job_oldest_update_age_seconds{state="IMPORTING"}`, "30"},
		{`slusk_jobs_without_active_candidate`, "3"},
	} {
		if !hasMetricLine(body, want.prefix, want.value) {
			t.Errorf("missing %s = %s in scrape:\n%s", want.prefix, want.value, body)
		}
	}
}

// A label set that disappears must not keep reporting its last value: a gauge
// still claiming a 30-day-old age for a state that has no jobs is worse than no
// gauge at all.
func TestMetricsDropsJobProgressSeriesWhenStateEmpties(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.SetJobProgress(JobProgressReport{States: []JobProgressState{
		{State: "DOWNLOADING", Count: 1, OldestUpdateAge: 2_592_000 * time.Second},
		{State: "IMPORTING", Count: 1, OldestUpdateAge: 5 * time.Second},
	}})
	if body := scrape(t, reg); !strings.Contains(body, `slusk_jobs_in_state{state="DOWNLOADING"}`) {
		t.Fatalf("setup: DOWNLOADING series missing:\n%s", body)
	}

	// The last DOWNLOADING job leaves; IMPORTING still has one.
	m.SetJobProgress(JobProgressReport{States: []JobProgressState{
		{State: "IMPORTING", Count: 1, OldestUpdateAge: 5 * time.Second},
	}})

	body := scrape(t, reg)
	if strings.Contains(body, `state="DOWNLOADING"`) {
		t.Errorf("DOWNLOADING series survived the state emptying:\n%s", body)
	}
	if !strings.Contains(body, `slusk_jobs_in_state{state="IMPORTING"}`) {
		t.Errorf("IMPORTING series lost:\n%s", body)
	}
}

// An empty report leaves no per-state series at all. Absent is the honest
// encoding for "no jobs"; a zero age would read as "everything is fresh".
func TestMetricsEmptyJobProgressLeavesNoSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.SetJobProgress(JobProgressReport{States: []JobProgressState{
		{State: "WANTED", Count: 4, OldestUpdateAge: time.Minute},
	}})
	m.SetJobProgress(JobProgressReport{})

	body := scrape(t, reg)
	if strings.Contains(body, "slusk_jobs_in_state{") {
		t.Errorf("per-state series survived an empty report:\n%s", body)
	}
	if strings.Contains(body, "slusk_job_oldest_update_age_seconds{") {
		t.Errorf("per-state age series survived an empty report:\n%s", body)
	}
	// The wedge count is a plain gauge, not a vector: zero is meaningful there.
	if !hasMetricLine(body, "slusk_jobs_without_active_candidate", "0") {
		t.Errorf("wedge gauge missing from scrape:\n%s", body)
	}
}

func TestStatusEndpointIncludesJobProgress(t *testing.T) {
	reg := prometheus.NewRegistry()
	deps := testServerDeps(reg)
	deps.JobProgress = func(ctx context.Context) (JobProgressReport, error) {
		return JobProgressReport{
			States: []JobProgressState{
				{State: "DOWNLOADING", Count: 2, OldestUpdateAge: 90 * time.Second},
			},
			JobsWithoutActiveCandidate: 1,
		}, nil
	}
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}

	var got struct {
		JobProgress struct {
			States []struct {
				State              string `json:"state"`
				Count              int    `json:"count"`
				OldestUpdateAgeSec int    `json:"oldestUpdateAgeSeconds"`
			} `json:"states"`
			JobsWithoutActiveCandidate int `json:"jobsWithoutActiveCandidate"`
		} `json:"jobProgress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.JobProgress.States) != 1 {
		t.Fatalf("states = %+v, want one entry", got.JobProgress.States)
	}
	s := got.JobProgress.States[0]
	if s.State != "DOWNLOADING" || s.Count != 2 || s.OldestUpdateAgeSec != 90 {
		t.Errorf("state entry = %+v, want DOWNLOADING/2/90", s)
	}
	if got.JobProgress.JobsWithoutActiveCandidate != 1 {
		t.Errorf("jobsWithoutActiveCandidate = %d, want 1", got.JobProgress.JobsWithoutActiveCandidate)
	}
}

// An empty snapshot serializes as an empty array, never null: the frontend maps
// over it directly.
func TestStatusEndpointJobProgressStatesIsAlwaysAnArray(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewServer(testServerDeps(reg))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	progress, ok := raw["jobProgress"]
	if !ok {
		t.Fatalf("missing jobProgress: %s", rec.Body.String())
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(progress, &inner); err != nil {
		t.Fatalf("decode jobProgress: %v", err)
	}
	if got := string(inner["states"]); got != "[]" {
		t.Errorf("states = %s, want []", got)
	}
}
