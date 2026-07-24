// Package observ: albumlive.go aggregates live (in-memory, non-persisted)
// transfer data from the peer backend's ListDownloads up to album level for
// the jobs list (GET /api/jobs, issue #157): per-file speed/queue-position is
// summed/mined across every live transfer belonging to a job's current
// candidate, so the list shows one album-level speed and ETA rather than
// requiring the frontend to open the per-job detail panel to see progress.
package observ

import (
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// maxETASeconds clamps etaSeconds so a momentary 1 B/s blip cannot render a
// multi-year ETA; 30 days is generous for a stalled-but-recovering transfer
// while still being an obviously-bogus number the frontend could choose to
// treat specially.
const maxETASeconds = 30 * 24 * 3600

// etaSeconds estimates remaining transfer time from remaining bytes and an
// average speed, or 0 (meaning: omit the field, see jobDTO.ETASeconds) when
// either input makes the estimate meaningless: avgSpeed <= 0 (no live
// throughput to divide by) or remaining <= 0 (nothing left, or unknown).
func etaSeconds(remaining, avgSpeed int64) int64 {
	if avgSpeed <= 0 || remaining <= 0 {
		return 0
	}
	eta := remaining / avgSpeed
	if eta > maxETASeconds {
		return maxETASeconds
	}
	return eta
}

// liveAlbumIndex groups a ListDownloads snapshot by peer username, so
// aggregateLiveAlbum can look up one candidate's live transfers without
// rescanning the whole snapshot for every job in the list. The zero value
// (nil map) is a valid "no live data" index: a map lookup on a nil map
// simply reports no match.
type liveAlbumIndex struct {
	byUsername map[string][]core.RemoteTransfer
}

// newLiveAlbumIndex builds a liveAlbumIndex from a ListDownloads snapshot.
func newLiveAlbumIndex(live []core.RemoteTransfer) liveAlbumIndex {
	idx := liveAlbumIndex{byUsername: make(map[string][]core.RemoteTransfer, len(live))}
	for _, t := range live {
		idx.byUsername[t.Username] = append(idx.byUsername[t.Username], t)
	}
	return idx
}

// aggregateLiveAlbum sums the live transfers belonging to candidate's own
// files — matched exactly on (username, filename ∈ candidate.Files), never
// on username alone, since a peer serving files for two different albums at
// once would otherwise have both counted on both album rows. Returns the
// summed instantaneous speed and EWMA-smoothed average speed (for ETA),
// total remaining bytes across matched transfers, and the minimum queue
// position among matched transfers that report one (the album's download
// effectively starts once its first file starts). Files not yet enqueued
// (the per-peer in-flight throttle, see issue #20) have no live entry and so
// are simply not counted: the result describes only work currently in
// flight, not the album's full remaining size. candidate == nil (a job with
// no candidate yet) yields all zeros / hasQueuePosition false.
func aggregateLiveAlbum(candidate *core.Candidate, idx liveAlbumIndex) (speed, speedAvg, remaining int64, queuePosition uint32, hasQueuePosition bool) {
	if candidate == nil {
		return 0, 0, 0, 0, false
	}
	files := make(map[string]struct{}, len(candidate.Files))
	for _, f := range candidate.Files {
		files[f.Filename] = struct{}{}
	}
	for _, lt := range idx.byUsername[candidate.Username] {
		if _, ok := files[lt.Filename]; !ok {
			continue
		}
		speed += lt.Speed
		speedAvg += lt.SpeedAverage
		if left := lt.Size - lt.BytesDone; left > 0 {
			remaining += left
		}
		if lt.QueuePosition > 0 && (!hasQueuePosition || lt.QueuePosition < queuePosition) {
			queuePosition = lt.QueuePosition
			hasQueuePosition = true
		}
	}
	return speed, speedAvg, remaining, queuePosition, hasQueuePosition
}
