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
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: 100, SpeedAverage: 90, QueuePosition: 3},
		{Username: "alice", Filename: "02.flac", State: core.TransferQueued, Speed: 50, SpeedAverage: 40},
		{Username: "alice", Filename: "not-this-albums-file.flac", State: core.TransferInProgress, Speed: 999, SpeedAverage: 999},
		{Username: "bob", Filename: "01.flac", State: core.TransferInProgress, Speed: 999, SpeedAverage: 999},
	}
	idx := newLiveTransferIndex(live)

	speed, speedAvg, queuePos, hasQueue, matched := aggregateLiveAlbum(candidate, idx)
	if speed != 150 {
		t.Errorf("speed = %d, want 150", speed)
	}
	if speedAvg != 130 {
		t.Errorf("speedAvg = %d, want 130", speedAvg)
	}
	if !hasQueue || queuePos != 3 {
		t.Errorf("queuePos = %d, hasQueue = %v, want 3, true", queuePos, hasQueue)
	}
	if !matched {
		t.Error("matched = false, want true (two files matched a non-terminal live transfer)")
	}
}

// TestAggregateLiveAlbumNilCandidateYieldsZero covers a job with no candidate
// yet.
func TestAggregateLiveAlbumNilCandidateYieldsZero(t *testing.T) {
	idx := newLiveTransferIndex([]core.RemoteTransfer{{Username: "alice", Filename: "x", State: core.TransferInProgress, Speed: 100}})
	speed, speedAvg, queuePos, hasQueue, matched := aggregateLiveAlbum(nil, idx)
	if speed != 0 || speedAvg != 0 || queuePos != 0 || hasQueue || matched {
		t.Errorf("nil candidate aggregate = (%d, %d, %d, %v, %v), want all zero/false", speed, speedAvg, queuePos, hasQueue, matched)
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
		{Username: "sharedpeer", Filename: "albumA/01.flac", State: core.TransferInProgress, Speed: 111, SpeedAverage: 100},
		{Username: "sharedpeer", Filename: "albumB/01.flac", State: core.TransferInProgress, Speed: 222, SpeedAverage: 200},
	}
	idx := newLiveTransferIndex(live)

	speedA, avgA, _, _, _ := aggregateLiveAlbum(albumA, idx)
	speedB, avgB, _, _, _ := aggregateLiveAlbum(albumB, idx)

	if speedA != 111 || avgA != 100 {
		t.Errorf("album A aggregate = (speed %d, avg %d), want (111, 100)", speedA, avgA)
	}
	if speedB != 222 || avgB != 200 {
		t.Errorf("album B aggregate = (speed %d, avg %d), want (222, 200)", speedB, avgB)
	}
}

// TestAggregateLiveAlbumOnlyEnqueuedFileContributesSpeed asserts a file not
// yet enqueued (no live entry at all) is simply skipped — the loop over
// candidate.Files doesn't error or panic on the missing index lookup, and
// only the one matched file's speed is summed. Remaining-bytes accounting
// for not-yet-enqueued files is store-side (core.JobView.AlbumBytesRemaining,
// see internal/store/dashboard.go), not this function's concern.
func TestAggregateLiveAlbumOnlyEnqueuedFileContributesSpeed(t *testing.T) {
	candidate := &core.Candidate{
		Username: "alice",
		Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000},
			{Filename: "02.flac", Size: 2000}, // never enqueued, no live entry
		},
	}
	live := []core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: 100, SpeedAverage: 90},
	}
	idx := newLiveTransferIndex(live)

	speed, speedAvg, _, hasQueue, matched := aggregateLiveAlbum(candidate, idx)
	if speed != 100 || speedAvg != 90 {
		t.Errorf("aggregate = (speed %d, avg %d), want (100, 90) — only the enqueued file counted", speed, speedAvg)
	}
	if hasQueue {
		t.Errorf("hasQueue = true, want false (no matched transfer reports a queue position)")
	}
	if !matched {
		t.Error("matched = false, want true (01.flac matched a non-terminal live transfer)")
	}
}

// TestAggregateLiveAlbumIgnoresTerminalTransfers is issue #157 F3: a
// terminal-but-not-yet-reconciled transfer (errored, cancelled, completed)
// lingers in ListDownloads until the pipeline's next reconcile pass. Its
// (possibly still fresh, not-yet-stale) speed reading must not contribute —
// only transfers still able to make progress (queued, in-progress) count.
func TestAggregateLiveAlbumIgnoresTerminalTransfers(t *testing.T) {
	candidate := &core.Candidate{
		Username: "alice",
		Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000}, // errored, will never finish
			{Filename: "02.flac", Size: 1000}, // cancelled
			{Filename: "03.flac", Size: 1000}, // completed, not yet reconciled away
			{Filename: "04.flac", Size: 1000}, // the only one still actually downloading
		},
	}
	live := []core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferErrored, Speed: 999, SpeedAverage: 999},
		{Username: "alice", Filename: "02.flac", State: core.TransferCancelled, Speed: 999, SpeedAverage: 999},
		{Username: "alice", Filename: "03.flac", State: core.TransferCompleted, Speed: 0, SpeedAverage: 0},
		{Username: "alice", Filename: "04.flac", State: core.TransferInProgress, Speed: 1000, SpeedAverage: 1000},
	}
	idx := newLiveTransferIndex(live)

	speed, speedAvg, _, _, matched := aggregateLiveAlbum(candidate, idx)
	if speed != 1000 || speedAvg != 1000 {
		t.Errorf("speed/avg = (%d, %d), want (1000, 1000) — only file 04's still-progressing transfer counted", speed, speedAvg)
	}
	if !matched {
		t.Error("matched = false, want true (file 04 matched a non-terminal live transfer)")
	}
}

// TestAnyLiveMatchIncludesTerminalTransfers is the counterpart to
// TestAggregateLiveAlbumIgnoresTerminalTransfers: unlike aggregateLiveAlbum's
// matched, anyLiveMatch must NOT filter out a terminal-but-not-yet-reconciled
// transfer — that's exactly the case jobBytesDone needs to catch (issue #161).
func TestAnyLiveMatchIncludesTerminalTransfers(t *testing.T) {
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000},
	})
	files := []core.CandidateFile{{Filename: "01.flac", Size: 1000}}
	if !anyLiveMatch("alice", files, idx) {
		t.Error("anyLiveMatch = false, want true (a completed live match still counts)")
	}
	if anyLiveMatch("alice", nil, idx) {
		t.Error("anyLiveMatch with no files = true, want false")
	}
	if anyLiveMatch("bob", files, idx) {
		t.Error("anyLiveMatch for a different username = true, want false")
	}
}

// TestSumFileBytesDonePrefersLiveOverPersistedRegardlessOfState is issue
// #161's core fix: a file's live BytesDone wins even when that live entry is
// terminal (completed/errored/cancelled) — the old overlay excluded terminal
// matches and fell back to a stale persisted figure, causing the
// backwards-jump bug. A file with no live match at all falls back to its own
// persisted bytes.
func TestSumFileBytesDonePrefersLiveOverPersistedRegardlessOfState(t *testing.T) {
	files := []core.CandidateFile{
		{Filename: "01.flac", Size: 1000}, // in progress live
		{Filename: "02.flac", Size: 1000}, // just completed live, ahead of persisted
		{Filename: "03.flac", Size: 1000}, // no live match at all
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 400},
		{Username: "alice", Filename: "02.flac", State: core.TransferCompleted, BytesDone: 1000},
	})
	persisted := map[string]int64{
		"01.flac": 350, // stale, live wins
		"02.flac": 850, // stale (pre-completion), live wins
		"03.flac": 600, // no live match, persisted wins
	}

	got := sumFileBytesDone("alice", files, idx, persisted)
	if got != 400+1000+600 {
		t.Errorf("sumFileBytesDone = %d, want %d (400+1000+600)", got, 400+1000+600)
	}
}

// TestSumFileBytesDoneMissingPersistedEntryDefaultsToZero covers a file
// that's simply never had a transfer row yet (e.g. not-yet-enqueued, so it's
// absent from persistedByFilename): it must default to 0, not panic.
func TestSumFileBytesDoneMissingPersistedEntryDefaultsToZero(t *testing.T) {
	files := []core.CandidateFile{{Filename: "01.flac", Size: 1000}}
	got := sumFileBytesDone("alice", files, liveTransferIndex{}, nil)
	if got != 0 {
		t.Errorf("sumFileBytesDone = %d, want 0", got)
	}
}

// TestClampBytesDoneTableCases covers clampBytesDone's two behaviors: bound
// to bytesTotal when it's known and exceeded, and disable the clamp when
// bytesTotal is 0 ("unknown", not "empty").
func TestClampBytesDoneTableCases(t *testing.T) {
	cases := []struct {
		name             string
		done, bytesTotal int64
		want             int64
	}{
		{"exceeds total, clamped", 1300, 1000, 1000},
		{"under total, unchanged", 400, 1000, 400},
		{"zero total disables clamp", 700, 0, 700},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampBytesDone(tc.done, tc.bytesTotal); got != tc.want {
				t.Errorf("clampBytesDone(%d, %d) = %d, want %d", tc.done, tc.bytesTotal, got, tc.want)
			}
		})
	}
}

// TestJobBytesDoneFallsBackWithoutLiveMatch covers a candidate with no live
// data at all (e.g. LiveTransfers failed, or the peer backend restarted):
// the persisted AlbumBytesDone must be served unmodified.
func TestJobBytesDoneFallsBackWithoutLiveMatch(t *testing.T) {
	files := []core.CandidateFile{{Filename: "01.flac", Size: 1000}}
	got := jobBytesDone("alice", files, 1, 300, liveTransferIndex{}, nil)
	if got != 300 {
		t.Errorf("jobBytesDone = %d, want 300 (persisted fallback, no live match)", got)
	}
}

// TestJobBytesDoneFallsBackWhenPersistedMissingCandidate covers a live match
// existing, but the persisted map (from Store.TransferBytesByCandidate)
// having nothing for this candidate id — a nil TransferBytes dep, a failed
// query, or (for the SSE hub) a live match that appeared since the last
// correlation refresh. Falling back to the whole persisted AlbumBytesDone is
// safer than silently treating unmatched files as 0.
func TestJobBytesDoneFallsBackWhenPersistedMissingCandidate(t *testing.T) {
	files := []core.CandidateFile{{Filename: "01.flac", Size: 1000}}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 750},
	})
	got := jobBytesDone("alice", files, 1, 300, idx, nil)
	if got != 300 {
		t.Errorf("jobBytesDone = %d, want 300 (persisted fallback, candidate missing from persisted map)", got)
	}
}

// TestJobBytesDoneSumsPerFileWhenMatched is issue #161's fix, exercised at
// the jobBytesDone level: once persisted data is available for the
// candidate, the result is the per-file sum (live where matched, persisted
// otherwise) rather than the store's own AlbumBytesDone.
func TestJobBytesDoneSumsPerFileWhenMatched(t *testing.T) {
	files := []core.CandidateFile{
		{Filename: "01.flac", Size: 1000},
		{Filename: "02.flac", Size: 1000},
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 750},
	})
	persisted := map[int64]map[string]int64{
		1: {"01.flac": 300, "02.flac": 1000},
	}
	got := jobBytesDone("alice", files, 1, 1300, idx, persisted)
	if got != 750+1000 {
		t.Errorf("jobBytesDone = %d, want %d (750 live + 1000 persisted)", got, 750+1000)
	}
}

// TestJobBytesDoneReproducesIssue161BackwardsJump is a regression test
// reproducing the exact scenario from the #161 SSE live-run: a multi-file
// album where one file transitions from IN_PROGRESS to COMPLETED (with MORE
// live bytes than the not-yet-reconciled persisted snapshot reflects) while
// another file is still actively downloading. The old overlay
// (persistedDone - persistedNonTerminalDone + liveDone, filtered to
// non-terminal live matches) would drop the just-completed file's
// contribution entirely and report a total LOWER than the previous tick. The
// new per-file sum must not regress.
func TestJobBytesDoneReproducesIssue161BackwardsJump(t *testing.T) {
	files := []core.CandidateFile{
		{Filename: "01.flac", Size: 20_000_000}, // still downloading
		{Filename: "02.flac", Size: 13_345_695}, // just completed
	}

	// Tick N: file 02 still in progress, DB last reconciled with both files'
	// prior byte counts.
	idxBefore := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 12_124_160},
		{Username: "alice", Filename: "02.flac", State: core.TransferInProgress, BytesDone: 13_280_159},
	})
	persisted := map[int64]map[string]int64{
		1: {"01.flac": 12_124_160, "02.flac": 13_280_159},
	}
	before := jobBytesDone("alice", files, 1, 12_124_160+13_280_159, idxBefore, persisted)

	// Tick N+1: file 02 completes with its final (larger) byte count; the DB
	// has NOT reconciled yet, so persisted/AlbumBytesDone are unchanged from
	// tick N (this is the "unreconciled" window the continuity property
	// covers — see jobBytesDone's doc comment).
	idxAfter := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 12_124_160},
		{Username: "alice", Filename: "02.flac", State: core.TransferCompleted, BytesDone: 13_345_695},
	})
	after := jobBytesDone("alice", files, 1, 12_124_160+13_280_159, idxAfter, persisted)

	if after < before {
		t.Errorf("BytesDone regressed on file completion: before=%d after=%d", before, after)
	}
	wantAfter := int64(12_124_160 + 13_345_695)
	if after != wantAfter {
		t.Errorf("after = %d, want %d (file 02's final live bytes counted)", after, wantAfter)
	}
}
