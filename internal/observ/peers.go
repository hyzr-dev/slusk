// Package observ: peers.go serves the dashboard's Peers view (GET
// /api/peers): every known Soulseek peer's reliability history and the
// decayed score the ranker uses to rank them (see
// matcher.ReliabilityHistoryScore, reused here rather than duplicated).
package observ

import (
	"context"
	"sort"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
)

// peerArtistDTO is one peer's reliability history for a single artist.
type peerArtistDTO struct {
	ArtistID      int64   `json:"artistId"`
	SuccessCount  int     `json:"successCount"`
	FailCount     int     `json:"failCount"`
	LastSuccessAt string  `json:"lastSuccessAt"`
	LastFailAt    string  `json:"lastFailAt"`
	Score         float64 `json:"score"`
}

// peerDTO is the JSON shape served at /api/peers.
type peerDTO struct {
	Username      string          `json:"username"`
	SuccessCount  int             `json:"successCount"`
	FailCount     int             `json:"failCount"`
	LastSuccessAt string          `json:"lastSuccessAt"`
	LastFailAt    string          `json:"lastFailAt"`
	Score         float64         `json:"score"`
	Artists       []peerArtistDTO `json:"artists"`
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(timeFormat)
}

// toPeerDTO flattens a core.PeerRow into the Peers view's display-ready
// shape. Scores are computed with matcher.ReliabilityHistoryScore — the exact
// function the ranker calls — so the number shown always matches what
// actually drove candidate ranking. The top-level Score uses only the peer's
// global history (no artist context), matching how the ranker scores a peer
// for an artist it has no artist-specific history with; each entry in
// Artists additionally folds in that artist's own history, matching how the
// ranker scores a peer it does have artist-specific history with.
func toPeerDTO(row core.PeerRow, now time.Time) peerDTO {
	out := peerDTO{
		Username:      row.Username,
		SuccessCount:  row.Global.SuccessCount,
		FailCount:     row.Global.FailCount,
		LastSuccessAt: formatOptionalTime(row.Global.LastSuccessAt),
		LastFailAt:    formatOptionalTime(row.Global.LastFailAt),
		Score:         matcher.ReliabilityHistoryScore(core.PeerReliability{Global: row.Global}, now),
		Artists:       make([]peerArtistDTO, 0, len(row.Artists)),
	}
	for artistID, c := range row.Artists {
		out.Artists = append(out.Artists, peerArtistDTO{
			ArtistID:      artistID,
			SuccessCount:  c.SuccessCount,
			FailCount:     c.FailCount,
			LastSuccessAt: formatOptionalTime(c.LastSuccessAt),
			LastFailAt:    formatOptionalTime(c.LastFailAt),
			Score:         matcher.ReliabilityHistoryScore(core.PeerReliability{Artist: c, Global: row.Global}, now),
		})
	}
	// Map iteration order is random; sort for a deterministic response.
	sort.Slice(out.Artists, func(i, j int) bool { return out.Artists[i].ArtistID < out.Artists[j].ArtistID })
	return out
}

// PeersFunc produces every known peer's reliability history (typically backed
// by the store's Peers).
type PeersFunc func(ctx context.Context) ([]core.PeerRow, error)
