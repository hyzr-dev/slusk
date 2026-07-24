// Package observ: charts.go serves the Overview view's charts (GET
// /api/charts, issue #88): recent Discovery search-pass history and
// completed downloads per hour over the last 24 hours.
package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// chartsHourBuckets is the fixed number of hourly buckets the completed-by-
// hour series is zero-filled to.
const chartsHourBuckets = 24

// ChartsRecentPasses caps how many recent search passes GET /api/charts
// serves. Exported so main.go's chartsFn (the RecentSearchPasses caller) and
// this package share a single source of truth for the cap.
const ChartsRecentPasses = 20

// ChartsData is the raw chart source; the handler formats and zero-fills it
// into chartsDTO. Passes is newest first (see store.RecentSearchPasses),
// capped at ChartsRecentPasses; CompletedByHour is sparse (see
// store.CompletedByHour).
type ChartsData struct {
	Passes          []core.SearchPass
	CompletedByHour []core.HourCount
}

// ChartsFunc produces the current ChartsData (typically backed by the
// store's RecentSearchPasses and CompletedByHour).
type ChartsFunc func(ctx context.Context) (ChartsData, error)

// ThroughputFunc produces the native soulseek client's recent aggregate
// download-throughput samples, oldest first (typically backed by
// soulseek.Client.ThroughputSamples, issue #157). A nil func, or one wired to
// a non-native backend, simply yields no throughput series — see
// registerCharts.
type ThroughputFunc func(ctx context.Context) ([]core.ThroughputSample, error)

// passDTO is the JSON shape of one search pass served at GET /api/charts.
type passDTO struct {
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	Searched   int    `json:"searched"`
	Matched    int    `json:"matched"`
}

// hourCountDTO is the JSON shape of one hour bucket served at GET /api/charts.
type hourCountDTO struct {
	Hour  string `json:"hour"`
	Count int    `json:"count"`
}

// throughputSampleDTO is the JSON shape of one live download-throughput
// sample served at GET /api/charts (issue #157).
type throughputSampleDTO struct {
	At              string `json:"at"`
	BytesPerSecond  int64  `json:"bytesPerSecond"`
	ActiveTransfers int    `json:"activeTransfers"`
}

// chartsDTO is the JSON shape served at GET /api/charts.
type chartsDTO struct {
	Passes          []passDTO             `json:"passes"`
	CompletedByHour []hourCountDTO        `json:"completedByHour"`
	Throughput      []throughputSampleDTO `json:"throughput"`
}

func toThroughputDTO(samples []core.ThroughputSample) []throughputSampleDTO {
	out := make([]throughputSampleDTO, len(samples))
	for i, s := range samples {
		out[i] = throughputSampleDTO{
			At:              s.At.Format(timeFormat),
			BytesPerSecond:  s.BytesPerSecond,
			ActiveTransfers: s.ActiveTransfers,
		}
	}
	return out
}

func toChartsDTO(data ChartsData, now time.Time) chartsDTO {
	// Passes arrive newest-first from the store; the chart draws oldest-first
	// (left to right, newest at the right edge).
	passes := make([]passDTO, len(data.Passes))
	for i, p := range data.Passes {
		passes[len(data.Passes)-1-i] = passDTO{
			StartedAt:  p.StartedAt.Format(timeFormat),
			FinishedAt: p.FinishedAt.Format(timeFormat),
			Searched:   p.Searched,
			Matched:    p.Matched,
		}
	}

	counts := make(map[time.Time]int, len(data.CompletedByHour))
	for _, hc := range data.CompletedByHour {
		counts[hc.Hour.UTC()] = hc.Count
	}
	end := now.UTC().Truncate(time.Hour)
	buckets := make([]hourCountDTO, chartsHourBuckets)
	for i := 0; i < chartsHourBuckets; i++ {
		hour := end.Add(-time.Duration(chartsHourBuckets-1-i) * time.Hour)
		buckets[i] = hourCountDTO{Hour: hour.Format(timeFormat), Count: counts[hour]}
	}

	return chartsDTO{Passes: passes, CompletedByHour: buckets}
}

// registerCharts wires GET /api/charts onto mux. throughput is best-effort
// (issue #157): a nil func, or one that errors, yields an empty throughput
// series and still a 200 — a Postgres outage (which does not affect the
// in-memory throughput meter at all) must not blank the live sparkline, and a
// non-native backend (which has no throughput data) must not 500 the whole
// Overview view just because it has nothing to report there.
func registerCharts(mux *http.ServeMux, charts ChartsFunc, throughput ThroughputFunc) {
	mux.HandleFunc("/api/charts", func(w http.ResponseWriter, r *http.Request) {
		data, err := charts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dto := toChartsDTO(data, time.Now())
		var samples []core.ThroughputSample
		if throughput != nil {
			if s, tErr := throughput(r.Context()); tErr == nil {
				samples = s
			}
		}
		dto.Throughput = toThroughputDTO(samples)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dto)
	})
}
