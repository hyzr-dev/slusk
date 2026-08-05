// Package observ: peers.go serves the dashboard's Peers view: GET /api/peers
// lists every known Soulseek peer's global reliability, and GET
// /api/peers/{username} serves one peer's per-artist history. Both carry the
// decayed score the ranker uses (see matcher.ReliabilityHistoryScore, reused
// here rather than duplicated).
//
// The split is issue #424: the list response used to embed every peer's full
// artist history, so it grew with the number of (artist, peer) pairs ever
// recorded rather than with the number of peers — for data no list row draws
// until a row is expanded, and only one row expands at a time.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/matcher"
)

// peerArtistDTO is one peer's reliability history for a single artist.
//
// ArtistName is empty whenever no name is known — there is no artists table,
// so it is read from the denormalized album_jobs.artist_name, which an artist
// with no jobs left has no row for. Consumers must fall back to ArtistID
// rather than rendering a blank; the id is the honest answer, a placeholder
// name would be an invented one.
type peerArtistDTO struct {
	ArtistID      int64   `json:"artistId"`
	ArtistName    string  `json:"artistName"`
	SuccessCount  int     `json:"successCount"`
	FailCount     int     `json:"failCount"`
	LastSuccessAt string  `json:"lastSuccessAt"`
	LastFailAt    string  `json:"lastFailAt"`
	Score         float64 `json:"score"`
}

// peerDTO is the JSON shape of one row served at /api/peers.
type peerDTO struct {
	Username      string  `json:"username"`
	SuccessCount  int     `json:"successCount"`
	FailCount     int     `json:"failCount"`
	LastSuccessAt string  `json:"lastSuccessAt"`
	LastFailAt    string  `json:"lastFailAt"`
	Score         float64 `json:"score"`
}

// peerHistoryDTO is the JSON shape served at /api/peers/{username}. An object
// rather than a bare array so the response can gain fields (the peer's own
// counters, paging) without breaking every consumer.
type peerHistoryDTO struct {
	Username string          `json:"username"`
	Artists  []peerArtistDTO `json:"artists"`
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(timeFormat)
}

// toPeerDTO flattens a core.PeerRow into the Peers list's display-ready
// shape. Score is computed with matcher.ReliabilityHistoryScore — the exact
// function the ranker calls — so the number shown always matches what
// actually drove candidate ranking. It uses only the peer's global history
// (no artist context), matching how the ranker scores a peer for an artist it
// has no artist-specific history with.
func toPeerDTO(row core.PeerRow, now time.Time) peerDTO {
	return peerDTO{
		Username:      row.Username,
		SuccessCount:  row.Global.SuccessCount,
		FailCount:     row.Global.FailCount,
		LastSuccessAt: formatOptionalTime(row.Global.LastSuccessAt),
		LastFailAt:    formatOptionalTime(row.Global.LastFailAt),
		Score:         matcher.ReliabilityHistoryScore(core.PeerReliability{Global: row.Global}, now),
	}
}

// toPeerHistoryDTO flattens one peer's artist history for
// /api/peers/{username}. Each artist's score folds that artist's own history
// in on top of the peer's global record, matching how the ranker scores a
// peer it does have artist-specific history with — which is why the store
// hands back the global counters alongside the artist rows.
func toPeerHistoryDTO(history core.PeerHistory, now time.Time) peerHistoryDTO {
	out := peerHistoryDTO{
		Username: history.Username,
		Artists:  make([]peerArtistDTO, 0, len(history.Artists)),
	}
	for _, a := range history.Artists {
		out.Artists = append(out.Artists, peerArtistDTO{
			ArtistID:      a.ArtistID,
			ArtistName:    a.Name,
			SuccessCount:  a.Counters.SuccessCount,
			FailCount:     a.Counters.FailCount,
			LastSuccessAt: formatOptionalTime(a.Counters.LastSuccessAt),
			LastFailAt:    formatOptionalTime(a.Counters.LastFailAt),
			Score:         matcher.ReliabilityHistoryScore(core.PeerReliability{Artist: a.Counters, Global: history.Global}, now),
		})
	}
	return out
}

// peersPageSize is the default PageSize when a request omits ?pageSize=, and
// the dashboard Peers list's own page size. peersPageSizeMin/Max bound the
// explicit parameter on the same reasoning as the jobs list (issue #268): 1 so
// a page can never be empty by construction, 50 so a caller can't turn a
// paginated endpoint back into the unbounded one this replaced.
//
// These must stay in step with store.PeersPageSize{,Min,Max}; the store
// enforces its own copy, and this one exists so a bad value is a 400 rather
// than a 500.
const peersPageSize int64 = 25
const peersPageSizeMin int64 = 1
const peersPageSizeMax int64 = 50

// PeersQuery is the validated query for one page of GET /api/peers. It mirrors
// store.PeersQuery — internal/observ does not import internal/store, so
// cmd/slusk adapts between the two.
type PeersQuery struct {
	Page     int64
	PageSize int64
	Sort     string
	Dir      string
}

// PeersResult is one page of known peers plus the total number of known peers
// — the whole set, not the page.
type PeersResult struct {
	Peers []core.PeerRow
	Total int64
}

// PeersFunc produces one page of known peers' global reliability and the total
// count (typically backed by the store's Peers).
type PeersFunc func(ctx context.Context, query PeersQuery) (PeersResult, error)

// parsePeersQuery validates GET /api/peers' page/pageSize/sort/dir, following
// the vocabulary parsePagedJobsQuery established.
//
// The sort allowlist below is a second, independent copy of
// store.PeersSortKeys: this package cannot import the store, and the store's
// copy is never reached for a value rejected here, so a key added there but
// not here is a 400 that store-level tests cannot see. #310 shipped exactly
// that shape for the jobs filter. TestPeersSortKeysMatchTheStore is the guard.
func parsePeersQuery(u *url.URL) (PeersQuery, error) {
	query := PeersQuery{Sort: "score", Dir: "desc", PageSize: peersPageSize}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return PeersQuery{}, errors.New("invalid query parameters")
	}
	allowed := map[string]struct{}{"page": {}, "pageSize": {}, "sort": {}, "dir": {}}
	for key, value := range values {
		if _, ok := allowed[key]; !ok {
			return PeersQuery{}, fmt.Errorf("unknown query parameter %q", key)
		}
		if len(value) != 1 {
			return PeersQuery{}, fmt.Errorf("duplicate query parameter %q", key)
		}
	}
	// pageSize is parsed before page: page's own overflow guard divides by the
	// page size actually in effect, so it must already be resolved.
	if raw, ok := values["pageSize"]; ok {
		pageSize, parseErr := strconv.ParseInt(raw[0], 10, 64)
		if parseErr != nil || pageSize < peersPageSizeMin || pageSize > peersPageSizeMax {
			return PeersQuery{}, errors.New("invalid pageSize")
		}
		query.PageSize = pageSize
	}
	if raw, ok := values["page"]; ok {
		page, parseErr := strconv.ParseInt(raw[0], 10, 64)
		if parseErr != nil || page < 0 || page > (int64(^uint64(0)>>1)/query.PageSize) {
			return PeersQuery{}, errors.New("invalid page")
		}
		query.Page = page
	}
	if raw, ok := values["sort"]; ok {
		query.Sort = raw[0]
	}
	if raw, ok := values["dir"]; ok {
		query.Dir = raw[0]
	}
	if !oneOf(query.Sort, "score", "successCount", "failCount", "username") {
		return PeersQuery{}, errors.New("invalid sort")
	}
	if !oneOf(query.Dir, "asc", "desc") {
		return PeersQuery{}, errors.New("invalid dir")
	}
	return query, nil
}

// PeerHistoryFunc produces one peer's per-artist reliability history
// (typically backed by the store's PeerHistory). The bool is false when there
// is no such peer at all, which the handler answers 404 — distinct from a
// known peer with an empty history, which is a 200 with no artists.
type PeerHistoryFunc func(ctx context.Context, username string) (core.PeerHistory, bool, error)

// registerPeers wires the Peers view's two routes onto mux.
func registerPeers(mux *http.ServeMux, peers PeersFunc, history PeerHistoryFunc) {
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		query, err := parsePeersQuery(r.URL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := peers(r.Context(), query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		dtos := make([]peerDTO, len(result.Peers))
		for i, row := range result.Peers {
			dtos[i] = toPeerDTO(row, now)
		}
		// An envelope rather than a bare array: the page alone cannot tell a
		// caller how many peers there are, and a pager that cannot say how far
		// the set runs is a pager that cannot get back.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Peers []peerDTO `json:"peers"`
			Total int64     `json:"total"`
		}{Peers: dtos, Total: result.Total})
	})

	// Soulseek usernames are not URL-safe by assumption — spaces, slashes and
	// non-ASCII all occur. ServeMux unescapes a wildcard segment before
	// PathValue sees it, so a %2F-encoded slash arrives here as part of the
	// username rather than splitting the path.
	mux.HandleFunc("/api/peers/{username}", func(w http.ResponseWriter, r *http.Request) {
		username := r.PathValue("username")
		if strings.TrimSpace(username) == "" {
			http.NotFound(w, r)
			return
		}
		row, found, err := history(r.Context(), username)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toPeerHistoryDTO(row, time.Now()))
	})
}
