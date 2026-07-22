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

// ChartsData is the raw chart source; the handler formats and zero-fills it
// into chartsDTO. Passes is newest first (see store.RecentSearchPasses),
// capped at chartsRecentPasses; CompletedByHour is sparse (see
// store.CompletedByHour).
type ChartsData struct {
	Passes          []core.SearchPass
	CompletedByHour []core.HourCount
}

// ChartsFunc produces the current ChartsData (typically backed by the
// store's RecentSearchPasses and CompletedByHour).
type ChartsFunc func(ctx context.Context) (ChartsData, error)

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

// chartsDTO is the JSON shape served at GET /api/charts.
type chartsDTO struct {
	Passes          []passDTO      `json:"passes"`
	CompletedByHour []hourCountDTO `json:"completedByHour"`
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

// registerCharts wires GET /api/charts onto mux.
func registerCharts(mux *http.ServeMux, charts ChartsFunc) {
	mux.HandleFunc("/api/charts", func(w http.ResponseWriter, r *http.Request) {
		data, err := charts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toChartsDTO(data, time.Now()))
	})
}
