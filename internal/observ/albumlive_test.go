package observ

import (
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestETASecondsTableCases(t *testing.T) {
	cases := []struct {
		name      string
		remaining int64
		avgSpeed  int64
		want      int64
	}{
		{"zero speed", 1000, 0, 0},
		{"negative speed", 1000, -5, 0},
		{"zero remaining", 0, 500, 0},
		{"negative remaining", -10, 500, 0},
		{"normal case", 1000, 100, 10},
		{"rounds down", 999, 100, 9},
		{"clamped at max", 1_000_000_000, 1, maxETASeconds},
		{"exactly at max boundary", maxETASeconds, 1, maxETASeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := etaSeconds(tc.remaining, tc.avgSpeed); got != tc.want {
				t.Errorf("etaSeconds(%d, %d) = %d, want %d", tc.remaining, tc.avgSpeed, got, tc.want)
			}
		})
	}
}

// TestAggregateLiveAlbumMatchesOnUsernameAndFile asserts the exact-match
// semantics (username AND filename ∈ candidate.Files): a live transfer for
// the right peer but a file not in this candidate's set must not contribute.
func TestAggregateLiveAlbumMatchesOnUsernameAndFile(t *testing.T) {
	candidate := &core.Candidate{
		Username: "alice",
		Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000},
			{Filename: "02.flac", Size: 2000},
		},
	}
	live := []core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", Speed: 100, SpeedAverage: 90, Size: 1000, BytesDone: 400, QueuePosition: 3},
		{Username: "alice", Filename: "02.flac", Speed: 50, SpeedAverage: 40, Size: 2000, BytesDone: 500},
		{Username: "alice", Filename: "not-this-albums-file.flac", Speed: 999, SpeedAverage: 999, Size: 100, BytesDone: 0},
		{Username: "bob", Filename: "01.flac", Speed: 999, SpeedAverage: 999, Size: 1000, BytesDone: 0},
	}
	idx := newLiveAlbumIndex(live)

	speed, speedAvg, remaining, queuePos, hasQueue := aggregateLiveAlbum(candidate, idx)
	if speed != 150 {
		t.Errorf("speed = %d, want 150", speed)
	}
	if speedAvg != 130 {
		t.Errorf("speedAvg = %d, want 130", speedAvg)
	}
	wantRemaining := int64((1000 - 400) + (2000 - 500))
	if remaining != wantRemaining {
		t.Errorf("remaining = %d, want %d", remaining, wantRemaining)
	}
	if !hasQueue || queuePos != 3 {
		t.Errorf("queuePos = %d, hasQueue = %v, want 3, true", queuePos, hasQueue)
	}
}

// TestAggregateLiveAlbumNilCandidateYieldsZero covers a job with no candidate
// yet.
func TestAggregateLiveAlbumNilCandidateYieldsZero(t *testing.T) {
	idx := newLiveAlbumIndex([]core.RemoteTransfer{{Username: "alice", Filename: "x", Speed: 100}})
	speed, speedAvg, remaining, queuePos, hasQueue := aggregateLiveAlbum(nil, idx)
	if speed != 0 || speedAvg != 0 || remaining != 0 || queuePos != 0 || hasQueue {
		t.Errorf("nil candidate aggregate = (%d, %d, %d, %d, %v), want all zero/false", speed, speedAvg, remaining, queuePos, hasQueue)
	}
}

// TestAggregateLiveAlbumTwoAlbumsSamePeerDoNotContaminate is the whole reason
// a.files was added to jobViewSelect (issue #157): a peer serving files for
// two different albums at once must not have both live transfers counted on
// both album rows.
func TestAggregateLiveAlbumTwoAlbumsSamePeerDoNotContaminate(t *testing.T) {
	albumA := &core.Candidate{
		Username: "sharedpeer",
		Files:    []core.CandidateFile{{Filename: "albumA/01.flac", Size: 1000}},
	}
	albumB := &core.Candidate{
		Username: "sharedpeer",
		Files:    []core.CandidateFile{{Filename: "albumB/01.flac", Size: 3000}},
	}
	live := []core.RemoteTransfer{
		{Username: "sharedpeer", Filename: "albumA/01.flac", Speed: 111, SpeedAverage: 100, Size: 1000, BytesDone: 200},
		{Username: "sharedpeer", Filename: "albumB/01.flac", Speed: 222, SpeedAverage: 200, Size: 3000, BytesDone: 300},
	}
	idx := newLiveAlbumIndex(live)

	speedA, avgA, remA, _, _ := aggregateLiveAlbum(albumA, idx)
	speedB, avgB, remB, _, _ := aggregateLiveAlbum(albumB, idx)

	if speedA != 111 || avgA != 100 || remA != 800 {
		t.Errorf("album A aggregate = (speed %d, avg %d, remaining %d), want (111, 100, 800)", speedA, avgA, remA)
	}
	if speedB != 222 || avgB != 200 || remB != 2700 {
		t.Errorf("album B aggregate = (speed %d, avg %d, remaining %d), want (222, 200, 2700)", speedB, avgB, remB)
	}
}

// TestAggregateLiveAlbumUnmatchedFilesNotCounted asserts a file not yet
// enqueued (no live entry at all) contributes nothing rather than erroring or
// counting its full size as "remaining".
func TestAggregateLiveAlbumUnmatchedFilesNotCounted(t *testing.T) {
	candidate := &core.Candidate{
		Username: "alice",
		Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000},
			{Filename: "02.flac", Size: 2000}, // never enqueued, no live entry
		},
	}
	live := []core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", Speed: 100, SpeedAverage: 90, Size: 1000, BytesDone: 400},
	}
	idx := newLiveAlbumIndex(live)

	speed, speedAvg, remaining, _, hasQueue := aggregateLiveAlbum(candidate, idx)
	if speed != 100 || speedAvg != 90 || remaining != 600 {
		t.Errorf("aggregate = (speed %d, avg %d, remaining %d), want (100, 90, 600) — only the enqueued file counted", speed, speedAvg, remaining)
	}
	if hasQueue {
		t.Errorf("hasQueue = true, want false (no matched transfer reports a queue position)")
	}
}
