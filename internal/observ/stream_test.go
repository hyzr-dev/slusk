package observ

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// --- pure function tests ---------------------------------------------------

// buildJobsDelta must produce whole jobDTOs built by the same toJobDTO REST
// uses, only for jobs whose jobDTO differs from what this subscriber was
// last sent (issue #258/#268): a job's presence in the delta tracks whether
// its DTO changed, not whether it's still "relevant" — a scoped
// subscriber's set (sub.jobIDs) never changes for the life of the
// connection (issue #268), so there is no "leaving the set" case to
// correct for; see buildJobsDelta's doc comment.
func TestBuildJobsDeltaOnlyChangedJobsSent(t *testing.T) {
	view := core.JobView{
		Job:                 core.AlbumJob{ID: 7, Title: "Rounds"},
		Attempt:             &core.Candidate{ID: 1, Username: "alice", Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}},
		AlbumBytesTotal:     1000,
		AlbumBytesRemaining: 1000,
	}
	views := map[int64]core.JobView{7: view}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	withLive := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50, BytesDone: 300},
	})
	first := buildJobsDelta(sub, views, withLive, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 || first[0].Speed != 50 {
		t.Fatalf("first tick: expected job 7 at speed 50, got %+v", first)
	}

	// Unchanged: no live data change, nothing rebuilt differently -> empty delta.
	second := buildJobsDelta(sub, views, withLive, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(second) != 0 {
		t.Fatalf("unchanged tick must produce an empty delta, got %+v", second)
	}

	// The transfer's speed changes -> job 7 is still in sub.jobIDs (it never
	// leaves), but its dto differs from what was last sent, so it's included
	// again.
	changed := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 75, BytesDone: 300},
	})
	third := buildJobsDelta(sub, views, changed, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(third) != 1 || third[0].ID != 7 || third[0].Speed != 75 {
		t.Fatalf("changed tick: expected job 7 at speed 75, got %+v", third)
	}
}

// TestBuildJobsDeltaSameNowUnchangedJobStaysOmitted proves that FramedAt
// being part of jobDTO doesn't defeat buildJobsDelta's reflect.DeepEqual
// change-detection: passing the exact same now on two consecutive ticks for
// an otherwise-unchanged job must not resend it (issue #285).
func TestBuildJobsDeltaSameNowUnchangedJobStaysOmitted(t *testing.T) {
	views := map[int64]core.JobView{
		7: {Job: core.AlbumJob{ID: 7, Title: "Rounds"}},
	}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	first := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 {
		t.Fatalf("first tick: expected job 7, got %+v", first)
	}

	second := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(second) != 0 {
		t.Fatalf("same now, unchanged job: expected an empty delta, got %+v", second)
	}
}

// TestBuildJobsDeltaDifferentNowResendsUnchangedLiveMatchedJob is the
// converse of same-now: for a job that is LIVE-MATCHED (anyLiveMatch true on
// its candidate), two different now values on an otherwise-identical job
// must resend it — a FramedAt-only refresh is exactly what should
// re-freshen the client's freshness signal (issue #285), which matters for
// the stalled-but-live case: identical byte/speed values tick to tick must
// still get a fresh FramedAt or the client incorrectly falls back to REST
// despite the job still being live. FramedAt must only ever be bumped to
// `now` for a live-matched job — see the code-review regression this test
// guards against in TestBuildJobsDeltaNonLiveMatchedJobKeepsFramedAtStable.
func TestBuildJobsDeltaDifferentNowResendsUnchangedLiveMatchedJob(t *testing.T) {
	view := core.JobView{
		Job:     core.AlbumJob{ID: 7, Title: "Rounds"},
		Attempt: &core.Candidate{ID: 1, Username: "alice", Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}},
	}
	views := map[int64]core.JobView{7: view}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	// Stalled: same speed/bytes on both ticks, but the file still has a live
	// counterpart (State: TransferQueued), so the job stays live-matched.
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferQueued, Speed: 0},
	})

	first := buildJobsDelta(sub, views, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 || first[0].FramedAt != testNow.Format(timeFormat) {
		t.Fatalf("first tick: expected job 7 framed at testNow, got %+v", first)
	}

	later := testNow.Add(time.Second)
	second := buildJobsDelta(sub, views, idx, nil, testFailedRetryAfter, testMaxCandidates, later)
	if len(second) != 1 || second[0].ID != 7 || second[0].FramedAt != later.Format(timeFormat) {
		t.Fatalf("different now, still live-matched: expected job 7 resent with the new FramedAt, got %+v", second)
	}
}

// TestBuildJobsDeltaNonLiveMatchedJobKeepsFramedAtStable is the regression
// test for what code review found in #285: tick() used to compute ONE
// shared `now` per tick and thread it into every scoped job's FramedAt
// unconditionally, so a job that is NOT live-matched (no candidate, or a
// candidate with no current live counterpart) got a different FramedAt every
// tick even though nothing about it actually changed — resending every
// scoped job, including fully terminal ones, on every single tick forever,
// defeating delta encoding. A non-live-matched job must keep whatever
// FramedAt it was last assigned across ticks with different `now`, so once
// its other fields stop changing, it is correctly omitted from the delta.
func TestBuildJobsDeltaNonLiveMatchedJobKeepsFramedAtStable(t *testing.T) {
	views := map[int64]core.JobView{
		7: {Job: core.AlbumJob{ID: 7, Title: "Rounds"}},
	}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{7: {}}}

	first := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(first) != 1 || first[0].ID != 7 || first[0].FramedAt != testNow.Format(timeFormat) {
		t.Fatalf("first tick: expected job 7 framed at testNow, got %+v", first)
	}

	later := testNow.Add(time.Second)
	second := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, later)
	if len(second) != 0 {
		t.Fatalf("different now, not live-matched, nothing else changed: expected an empty delta (FramedAt must not have been bumped), got %+v", second)
	}
}

// A scoped (?jobs=) subscriber must receive an entry for every requested id
// present in viewByJob, live-matched or not — sub.jobIDs is the relevant set
// outright (issue #268), not filtered by live match.
func TestBuildJobsDeltaScopedIncludesNonLiveMatchedRequestedIDs(t *testing.T) {
	views := map[int64]core.JobView{
		1: {Job: core.AlbumJob{ID: 1, Title: "Queued job"}},
	}
	sub := &streamSubscriber{jobIDs: map[int64]struct{}{1: {}}}
	got := buildJobsDelta(sub, views, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("expected the requested job even with no candidate/live match, got %+v", got)
	}
}

// TestBuildJobsDeltaEmptyForUnscopedSubscriber is issue #268's explicit
// decision: a subscriber that never sent ?jobs= gets no job frames at all,
// even when jobs are live-matched — every job-list surface now knows its own
// page and must publish it via ?jobs=.
func TestBuildJobsDeltaEmptyForUnscopedSubscriber(t *testing.T) {
	views := map[int64]core.JobView{
		7: {
			Job:     core.AlbumJob{ID: 7},
			Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}},
		},
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, Speed: 50},
	})
	sub := &streamSubscriber{} // no jobIDs
	got := buildJobsDelta(sub, views, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if len(got) != 0 {
		t.Fatalf("expected no job frames for an unscoped subscriber, got %+v", got)
	}
}

func TestMergeJobsDeltaUnionsByIDNewerWins(t *testing.T) {
	old := []jobDTO{{ID: 1, BytesDone: 100}, {ID: 2, BytesDone: 200}}
	next := []jobDTO{{ID: 2, BytesDone: 250}, {ID: 3, BytesDone: 300}}
	got := mergeJobsDelta(old, next)
	want := map[int64]int64{1: 100, 2: 250, 3: 300}
	if len(got) != len(want) {
		t.Fatalf("mergeJobsDelta(...) = %+v, want 3 entries", got)
	}
	for _, dto := range got {
		if bytesDone, ok := want[dto.ID]; !ok || bytesDone != dto.BytesDone {
			t.Errorf("job %d BytesDone = %d, want %d", dto.ID, dto.BytesDone, want[dto.ID])
		}
	}
}

func TestMergeJobsDeltaEmptyOldReturnsNextUnchanged(t *testing.T) {
	next := []jobDTO{{ID: 1}}
	got := mergeJobsDelta(nil, next)
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("mergeJobsDelta(nil, next) = %+v, want next unchanged", got)
	}
}

// buildStreamDetail must produce exactly what GET /api/jobs/{id}/detail
// produces for the same inputs — that identity is the whole point of #258's
// alternative A, since it is what lets the frontend replace rather than
// merge. Needs both a view (for the embedded jobDTO header, issue #268) and
// a detail to be cached.
func TestBuildStreamDetailMatchesRESTHandlerOutput(t *testing.T) {
	view := core.JobView{Job: core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet"}}
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "alice"},
			Transfers: []core.Transfer{
				{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 100, BytesTotal: 1000},
			},
		}},
	}
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 400, Speed: 100},
	})

	got := buildStreamDetail(view, true, detail, true, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if got == nil {
		t.Fatal("expected a detail for a cached job")
	}
	want := toJobDetailDTO(view, detail, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("stream detail diverged from the REST handler's:\n got %+v\nwant %+v", *got, want)
	}
	// And the live overlay actually applied, so the comparison above is not
	// two identically-wrong values.
	if got.Attempts[0].Transfers[0].BytesDone != 400 {
		t.Errorf("BytesDone = %d, want 400 from live", got.Attempts[0].Transfers[0].BytesDone)
	}
}

// A job missing EITHER the cached detail or the cached view omits the field
// entirely rather than sending an incomplete object the frontend would
// mistake for "this job has no attempts" (issue #268: both are now needed).
func TestBuildStreamDetailUncachedIsNil(t *testing.T) {
	view := core.JobView{Job: core.AlbumJob{ID: 7}}
	detail := core.JobDetail{Job: core.AlbumJob{ID: 7}}
	if got := buildStreamDetail(view, false, detail, true, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow); got != nil {
		t.Errorf("expected nil when the view isn't cached, got %+v", got)
	}
	if got := buildStreamDetail(view, true, detail, false, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow); got != nil {
		t.Errorf("expected nil when the detail isn't cached, got %+v", got)
	}
	if got := buildStreamDetail(core.JobView{}, false, core.JobDetail{}, false, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates, testNow); got != nil {
		t.Errorf("expected nil when neither is cached, got %+v", got)
	}
}

// TestComputeJobsFingerprintOrderIndependent pins the fingerprint's central
// requirement (issue #275): ListJobsWithTransfer's ORDER BY j.updated_at DESC
// has no tiebreaker, so at production row counts Postgres may hand back two
// logically identical calls in different row order. A sequential hash over
// the slice would pass every other test in this file yet still bump
// jobsGeneration spuriously in production; only this test, which shuffles
// the input, catches that regression.
func TestComputeJobsFingerprintOrderIndependent(t *testing.T) {
	views := []core.JobView{
		{Job: core.AlbumJob{ID: 1, State: core.StateDownloading, Title: "A"}, Status: "active", Peer: "alice"},
		{Job: core.AlbumJob{ID: 2, State: core.StateWanted, Title: "B"}, Status: "queued", Peer: ""},
		{Job: core.AlbumJob{ID: 3, State: core.StateFailed, Title: "C"}, Status: "failed", Peer: "bob"},
	}
	shuffled := []core.JobView{views[2], views[0], views[1]}

	got := computeJobsFingerprint(views)
	gotShuffled := computeJobsFingerprint(shuffled)
	if got != gotShuffled {
		t.Fatalf("fingerprint must be order-independent: sequential = %+v, shuffled = %+v", got, gotShuffled)
	}
}

// TestComputeJobsFingerprintChangesTable proves each page-membership-relevant
// field independently moves the fingerprint when mutated alone.
func TestComputeJobsFingerprintChangesTable(t *testing.T) {
	base := func() core.JobView {
		return core.JobView{
			Job: core.AlbumJob{
				ID:         1,
				State:      core.StateDownloading,
				Retries:    2,
				UpdatedAt:  testNow,
				Source:     core.SourceLidarr,
				Title:      "Rounds",
				ArtistName: "Doves",
			},
			Status: "active",
			Peer:   "alice",
		}
	}
	baseline := computeJobsFingerprint([]core.JobView{base()})

	cases := []struct {
		name   string
		mutate func(v *core.JobView)
	}{
		{"Status", func(v *core.JobView) { v.Status = "stalled" }},
		{"Job.State", func(v *core.JobView) { v.Job.State = core.StateFailed }},
		{"Peer", func(v *core.JobView) { v.Peer = "bob" }},
		{"Job.Retries", func(v *core.JobView) { v.Job.Retries = 3 }},
		{"Job.UpdatedAt", func(v *core.JobView) { v.Job.UpdatedAt = testNow.Add(time.Second) }},
		{"Job.Source", func(v *core.JobView) { v.Job.Source = core.SourceManual }},
		{"Job.Title", func(v *core.JobView) { v.Job.Title = "Different" }},
		{"Job.ArtistName", func(v *core.JobView) { v.Job.ArtistName = "Different" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.mutate(&v)
			got := computeJobsFingerprint([]core.JobView{v})
			if got == baseline {
				t.Errorf("mutating %s did not move the fingerprint", tc.name)
			}
		})
	}
}

// TestComputeJobsFingerprintIgnoresLiveOnlyFields keeps the busy-system win
// (issue #275): fields that move on essentially every refresh during an
// active download must NOT move the fingerprint, or the generation would
// bump every ~5s forever and pin every subscriber to the invalidate throttle
// floor permanently.
func TestComputeJobsFingerprintIgnoresLiveOnlyFields(t *testing.T) {
	base := core.JobView{
		Job:                 core.AlbumJob{ID: 1, Title: "Rounds"},
		Attempt:             &core.Candidate{ID: 9, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}}},
		AlbumBytesDone:      100,
		AlbumBytesTotal:     1000,
		AlbumBytesRemaining: 900,
	}
	baseline := computeJobsFingerprint([]core.JobView{base})

	mutated := base
	mutated.AlbumBytesDone = 500
	mutated.AlbumBytesTotal = 2000
	mutated.AlbumBytesRemaining = 1500
	mutated.Attempt = &core.Candidate{ID: 9, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1000}, {Filename: "b.flac", Size: 2000}}}

	got := computeJobsFingerprint([]core.JobView{mutated})
	if got != baseline {
		t.Fatalf("live-only bytes/Attempt.Files fields must not move the fingerprint: baseline = %+v, got = %+v", baseline, got)
	}
}

// TestComputeJobsFingerprintShrinkingSetChanges proves a job leaving the set
// (CANCELLED, deleted) moves the fingerprint even though its own hash is
// simply subtracted from — not corrupted, but absent from — the wrapping sum.
func TestComputeJobsFingerprintShrinkingSetChanges(t *testing.T) {
	views := []core.JobView{
		{Job: core.AlbumJob{ID: 1, Title: "A"}},
		{Job: core.AlbumJob{ID: 2, Title: "B"}},
	}
	full := computeJobsFingerprint(views)
	shrunk := computeJobsFingerprint(views[:1])
	if full == shrunk {
		t.Fatalf("removing a job must move the fingerprint: full = %+v, shrunk = %+v", full, shrunk)
	}
}

// TestComputeJobsFingerprintCountField proves the count field is actually
// populated from len(views), independent of sum. Without this,
// TestComputeJobsFingerprintShrinkingSetChanges alone is satisfied by sum's
// own change and leaves a `count: 0` mutation undetected (issue #275 review):
// the doc comment sells count as the guard against pathological wrapping-sum
// cancellation, but nothing proved the field was ever assigned at all.
func TestComputeJobsFingerprintCountField(t *testing.T) {
	views := []core.JobView{
		{Job: core.AlbumJob{ID: 1, Title: "A"}},
		{Job: core.AlbumJob{ID: 2, Title: "B"}},
		{Job: core.AlbumJob{ID: 3, Title: "C"}},
	}
	fp := computeJobsFingerprint(views)
	if fp.count != len(views) {
		t.Fatalf("count = %d, want %d", fp.count, len(views))
	}
}

// TestComputeJobsFingerprintDuplicateHashesDoNotCancel is the direct test of
// "wrapping addition, not XOR" (issue #275 review): two jobs whose per-job
// hashes are identical (here, literally the same view twice) sum to a
// nonzero value under wrapping addition (2h mod 2^64) but XOR to exactly
// zero. TestComputeJobsFingerprintOrderIndependent cannot catch a XOR
// regression — XOR is commutative too — so it needs this separate case.
func TestComputeJobsFingerprintDuplicateHashesDoNotCancel(t *testing.T) {
	v := core.JobView{Job: core.AlbumJob{ID: 1, Title: "A"}}
	fp := computeJobsFingerprint([]core.JobView{v, v})
	if fp.sum == 0 {
		t.Fatalf("duplicate per-job hashes must not cancel to zero under wrapping addition (sum = 0 implies XOR combination): %+v", fp)
	}
}

func TestChangedSinceLastTableCases(t *testing.T) {
	detailA := &jobDetailDTO{Job: jobDTO{ID: 1}, Attempts: []attemptDetailDTO{{ID: 1, Transfers: []transferDetailDTO{{Filename: "a.flac", BytesDone: 10}}}}}
	detailB := &jobDetailDTO{Job: jobDTO{ID: 1}, Attempts: []attemptDetailDTO{{ID: 1, Transfers: []transferDetailDTO{{Filename: "a.flac", BytesDone: 20}}}}}

	cases := []struct {
		name        string
		prev, next  livePayload
		newJobCount int
		want        bool
	}{
		{"nothing changed", livePayload{Detail: detailA, Down: 5, Up: 6}, livePayload{Detail: detailA, Down: 5, Up: 6}, 0, false},
		{"jobs changed", livePayload{}, livePayload{}, 1, true},
		{"down changed", livePayload{Down: 5}, livePayload{Down: 6}, 0, true},
		{"up changed", livePayload{Up: 5}, livePayload{Up: 6}, 0, true},
		{"detail nested transfer bytes changed", livePayload{Detail: detailA}, livePayload{Detail: detailB}, 0, true},
		{"detail appeared", livePayload{}, livePayload{Detail: detailA}, 0, true},
		// A fresh throughput sample no longer forces a live frame on its own
		// (issue #265): throughput now travels on its own independent
		// `event: throughput` channel, gated purely on whether a fresh
		// sample exists, never on changedSinceLast. Down/Up unchanged here
		// stands in for "a new sample arrived at the same rate as before" —
		// changedSinceLast no longer even receives throughput data to
		// distinguish that case from "nothing changed" at all.
		{"same rate, unaffected by any throughput sample arriving on the side", livePayload{Detail: detailA, Down: 5, Up: 6}, livePayload{Detail: detailA, Down: 5, Up: 6}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := changedSinceLast(tc.prev, tc.next, tc.newJobCount)
			if got != tc.want {
				t.Errorf("changedSinceLast(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildLiveSnapshotDetailScopingByJobID(t *testing.T) {
	live := []core.RemoteTransfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 500}}
	idx := newLiveTransferIndex(live)
	view := core.JobView{Job: core.AlbumJob{ID: 7}}
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 7},
		Attempts: []core.AttemptDetail{{
			Attempt:   core.Candidate{ID: 1, Username: "alice"},
			Transfers: []core.Transfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesTotal: 1000}},
		}},
	}
	series := core.ThroughputSeries{
		Download: []core.ThroughputSample{{BytesPerSecond: 100}, {BytesPerSecond: 200}},
		Upload:   []core.ThroughputSample{{BytesPerSecond: 300}, {BytesPerSecond: 400}},
	}

	unscoped := buildLiveSnapshot(live, 0, series, view, true, detail, true, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if unscoped.Detail != nil {
		t.Errorf("expected nil Detail without ?job=, got %+v", unscoped.Detail)
	}

	scoped := buildLiveSnapshot(live, 7, series, view, true, detail, true, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if scoped.Detail == nil || len(scoped.Detail.Attempts) != 1 {
		t.Fatalf("expected a detail for ?job=7, got %+v", scoped.Detail)
	}
	if got := scoped.Detail.Attempts[0].Transfers[0].BytesDone; got != 500 {
		t.Errorf("BytesDone = %d, want 500 merged in from live", got)
	}
	if scoped.Down != 200 || scoped.Up != 400 || scoped.Down != unscoped.Down || scoped.Up != unscoped.Up {
		t.Errorf("global rates changed under ?job=: scoped down/up=%d/%d, unscoped=%d/%d", scoped.Down, scoped.Up, unscoped.Down, unscoped.Up)
	}

	// Scoped, but the hub has no cached detail (or view) for that id yet —
	// the field is omitted rather than sent empty, and the frontend keeps
	// its REST copy.
	missing := buildLiveSnapshot(live, 999, series, core.JobView{}, false, core.JobDetail{}, false, idx, nil, testFailedRetryAfter, testMaxCandidates, testNow)
	if missing.Detail != nil {
		t.Errorf("expected nil Detail for an uncached job id, got %+v", missing.Detail)
	}
	if missing.Down != 200 || missing.Up != 400 {
		t.Errorf("missing scoped detail affected global rates: down/up=%d/%d", missing.Down, missing.Up)
	}
}

func TestLivePayloadExactBidirectionalJSON(t *testing.T) {
	// Jobs is deliberately left unset: it's delta-encoded (see buildJobsDelta)
	// and omitempty, so a frame with nothing job-related to report omits the
	// key entirely rather than emitting an empty array. Throughput/
	// UploadThroughput no longer exist on livePayload at all (issue #265) —
	// see TestThroughputPayloadExactBidirectionalJSON for their new home.
	payload := livePayload{
		Down: 200,
		Up:   400,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"down":200,"up":400}`
	if string(body) != want {
		t.Errorf("live JSON = %s\nwant      = %s", body, want)
	}
}

// TestThroughputPayloadExactBidirectionalJSON pins the wire shape of
// `event: throughput` (issue #265): field names are `download`/`upload`,
// deliberately different from livePayload's `down`/`up` scalars, and both
// are omitempty since a frame with a fresh sample in only one direction
// must not emit an empty array for the other.
func TestThroughputPayloadExactBidirectionalJSON(t *testing.T) {
	payload := throughputPayload{
		Download: []throughputSampleDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 200, ActiveTransfers: 2}},
		Upload:   []throughputSampleDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 400, ActiveTransfers: 4}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"download":[{"at":"2026-01-01T00:00:00Z","bytesPerSecond":200,"activeTransfers":2}],"upload":[{"at":"2026-01-01T00:00:00Z","bytesPerSecond":400,"activeTransfers":4}]}`
	if string(body) != want {
		t.Errorf("throughput JSON = %s\nwant      = %s", body, want)
	}

	empty := throughputPayload{}
	emptyBody, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if string(emptyBody) != `{}` {
		t.Errorf("empty throughput JSON = %s, want {}", emptyBody)
	}
}

// TestInvalidatePayloadExactBidirectionalJSON pins the wire shape of
// `event: invalidate` (issue #275), matching the convention of
// TestLivePayloadExactBidirectionalJSON/TestThroughputPayloadExactBidirectionalJSON:
// the field is `generation`, and it is NOT omitempty — a generation of 0 is
// meaningful (see TestStreamHubNoInvalidateOnFirstRefresh), so the key must
// always be present rather than vanishing on the zero value.
func TestInvalidatePayloadExactBidirectionalJSON(t *testing.T) {
	payload := invalidatePayload{Generation: 3}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"generation":3}`
	if string(body) != want {
		t.Errorf("invalidate JSON = %s\nwant           = %s", body, want)
	}

	var decoded invalidatePayload
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded != payload {
		t.Errorf("round-tripped = %+v, want %+v", decoded, payload)
	}
}

// TestStreamHubScopedSubscriptionKeepsGlobalRatesAndSeries covers the "down/
// up stay global" criterion (issue #265): two subscribers scoped to
// DIFFERENT ?jobs= sets, both opted into ?throughput=1, still see identical
// Down/Up scalars and byte-identical initial throughput frames — the
// directional series is a global quantity, entirely independent of a
// subscriber's job scope.
func TestStreamHubScopedSubscriptionKeepsGlobalRatesAndSeries(t *testing.T) {
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: at, BytesPerSecond: 200}},
			Upload:   []core.ThroughputSample{{At: at, BytesPerSecond: 400}},
		}, nil
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, noopJobDetail, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	unscopedID, _, _, _, _, unscoped, unscopedThroughput, _ := hub.subscribe(context.Background(), 0, map[int64]struct{}{1: {}}, true, "")
	defer hub.unsubscribe(unscopedID)
	scopedID, _, _, _, _, scoped, scopedThroughput, _ := hub.subscribe(context.Background(), 7, map[int64]struct{}{2: {}}, true, "")
	defer hub.unsubscribe(scopedID)

	if scoped.Down != unscoped.Down || scoped.Up != unscoped.Up {
		t.Errorf("scoped rates = %d/%d, global = %d/%d", scoped.Down, scoped.Up, unscoped.Down, unscoped.Up)
	}
	if !reflect.DeepEqual(scopedThroughput, unscopedThroughput) {
		t.Errorf("scoped throughput frame differs from global: scoped=%+v global=%+v", scopedThroughput, unscopedThroughput)
	}
}

// TestSendLatestReplacesRatesUnionsJobs asserts mailbox replacement: Down/Up
// (ordinary snapshot fields) are superseded outright, while Jobs
// (delta-encoded, unioned by id) preserves what the superseded frame
// carried. Throughput's own accumulation contract moved to
// sendLatestThroughput (issue #265) — see
// TestSendLatestThroughputAccumulatesAcrossDisplacedFrame below.
func TestSendLatestReplacesRatesUnionsJobs(t *testing.T) {
	ch := make(chan livePayload, 1)

	first := livePayload{
		Jobs: []jobDTO{{ID: 1, BytesDone: 100}, {ID: 2, BytesDone: 200}},
		Down: 10,
		Up:   11,
	}
	if got := sendLatest(ch, first); got.Down != 10 || got.Up != 11 {
		t.Fatalf("first send: got %+v", got)
	}

	second := livePayload{
		Jobs: []jobDTO{{ID: 2, BytesDone: 250}, {ID: 3, BytesDone: 300}},
		Down: 20,
		Up:   21,
	}
	merged := sendLatest(ch, second)
	if merged.Down != 20 || merged.Up != 21 {
		t.Errorf("merged Down/Up = %d/%d, want second's 20/21", merged.Down, merged.Up)
	}
	wantJobs := map[int64]int64{1: 100, 2: 250, 3: 300}
	if len(merged.Jobs) != len(wantJobs) {
		t.Fatalf("merged.Jobs = %+v, want 3 entries (union of both ticks, job 1 not dropped)", merged.Jobs)
	}
	for _, dto := range merged.Jobs {
		if want, ok := wantJobs[dto.ID]; !ok || want != dto.BytesDone {
			t.Errorf("job %d BytesDone = %d, want %d", dto.ID, dto.BytesDone, wantJobs[dto.ID])
		}
	}

	queued := <-ch
	if len(queued.Jobs) != 3 {
		t.Errorf("queued = %+v, want 3 jobs", queued)
	}
}

// TestSendLatestThroughputAccumulatesAcrossDisplacedFrame is the test issue
// #265 demands (written first, TDD): sendLatestThroughput must ADD an
// undelivered frame's samples to the next one rather than discard them —
// checked both on the return value and on what ends up queued in the
// channel, since either one alone could hide a bug in the other (a merge
// that mutated its argument in place, say, could pass a return-value-only
// check while leaving the channel with something different). Throughput is
// the one field where a displaced sample is unrecoverable rather than
// self-healing (see this file's package comment), so accumulation, not mere
// non-blocking delivery, is the property that matters here (see
// TestStreamHubTickThroughputSurvivesUndrainedMailbox for the
// interaction-level version of the same requirement).
func TestSendLatestThroughputAccumulatesAcrossDisplacedFrame(t *testing.T) {
	tch := make(chan throughputPayload, 1)

	d1 := throughputSampleDTO{At: "d1"}
	u1 := throughputSampleDTO{At: "u1"}
	first := throughputPayload{Download: []throughputSampleDTO{d1}, Upload: []throughputSampleDTO{u1}}
	if got := sendLatestThroughput(tch, first); len(got.Download) != 1 || len(got.Upload) != 1 {
		t.Fatalf("first send: got %+v", got)
	}

	// Sent WITHOUT reading the channel first — first is still queued.
	d2 := throughputSampleDTO{At: "d2"}
	u2 := throughputSampleDTO{At: "u2"}
	second := throughputPayload{Download: []throughputSampleDTO{d2}, Upload: []throughputSampleDTO{u2}}
	merged := sendLatestThroughput(tch, second)

	if len(merged.Download) != 2 || merged.Download[0] != d1 || merged.Download[1] != d2 {
		t.Errorf("returned Download = %+v, want [d1, d2] (the caller advances its watermark from this)", merged.Download)
	}
	if len(merged.Upload) != 2 || merged.Upload[0] != u1 || merged.Upload[1] != u2 {
		t.Errorf("returned Upload = %+v, want [u1, u2]", merged.Upload)
	}

	queued := <-tch
	if len(queued.Download) != 2 || queued.Download[0] != d1 || queued.Download[1] != d2 {
		t.Errorf("queued Download = %+v, want [d1, d2]", queued.Download)
	}
	if len(queued.Upload) != 2 || queued.Upload[0] != u1 || queued.Upload[1] != u2 {
		t.Errorf("queued Upload = %+v, want [u1, u2]", queued.Upload)
	}
}

// TestSendLatestThroughputCapsAtFortyEightAcrossDisplacedFrames covers the
// cap half of the same contract: accumulating across repeated displaced
// frames must never let either direction's slice grow past
// streamThroughputCap, and the entries kept must be the newest ones.
func TestSendLatestThroughputCapsAtFortyEightAcrossDisplacedFrames(t *testing.T) {
	tch := make(chan throughputPayload, 1)

	first := throughputPayload{Download: make([]throughputSampleDTO, streamThroughputCap)}
	for i := range first.Download {
		first.Download[i] = throughputSampleDTO{At: fmt.Sprintf("d%d", i)}
	}
	if got := sendLatestThroughput(tch, first); len(got.Download) != streamThroughputCap {
		t.Fatalf("first send: got %d samples, want %d", len(got.Download), streamThroughputCap)
	}

	// Sent without draining: displaces the full 48-sample frame above with
	// 5 more, which must push the total to 53 before capping back to 48,
	// dropping the OLDEST 5 (d0..d4) and keeping the newest.
	second := throughputPayload{Download: make([]throughputSampleDTO, 5)}
	for i := range second.Download {
		second.Download[i] = throughputSampleDTO{At: fmt.Sprintf("e%d", i)}
	}
	merged := sendLatestThroughput(tch, second)
	if len(merged.Download) != streamThroughputCap {
		t.Fatalf("merged Download = %d samples, want capped at %d", len(merged.Download), streamThroughputCap)
	}
	if merged.Download[0].At != "d5" {
		t.Errorf("merged.Download[0] = %+v, want d5 (the oldest 5 samples dropped by the cap)", merged.Download[0])
	}
	if last := merged.Download[len(merged.Download)-1]; last.At != "e4" {
		t.Errorf("merged.Download[last] = %+v, want e4 (the newest sample kept)", last)
	}

	queued := <-tch
	if len(queued.Download) != streamThroughputCap {
		t.Errorf("queued Download = %d samples, want capped at %d", len(queued.Download), streamThroughputCap)
	}
}

func TestSendLatestDeliversDirectlyToEmptyChannel(t *testing.T) {
	ch := make(chan livePayload, 1)
	payload := livePayload{Down: 5}
	got := sendLatest(ch, payload)
	if got.Down != 5 {
		t.Errorf("got = %+v, want the payload unchanged", got)
	}
	select {
	case queued := <-ch:
		if queued.Down != 5 {
			t.Errorf("queued = %+v, want Down=5", queued)
		}
	default:
		t.Fatal("expected a payload queued in ch")
	}
}

// --- ?jobs= parsing ----------------------------------------------------

func TestParseStreamJobIDs(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    map[int64]struct{}
		wantErr bool
	}{
		{"absent", "", nil, false},
		{"single", "5", map[int64]struct{}{5: {}}, false},
		{"multiple", "1,2,3", map[int64]struct{}{1: {}, 2: {}, 3: {}}, false},
		{"deduplicated", "1,1,2", map[int64]struct{}{1: {}, 2: {}}, false},
		{"non-numeric", "1,abc", nil, true},
		{"zero", "0", nil, true},
		{"negative", "-5", nil, true},
		{"empty entry", "1,,2", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStreamJobIDs(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got %v", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseStreamJobIDs(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseStreamJobIDsRejectsOverMax(t *testing.T) {
	ids := make([]string, streamMaxJobScope+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	_, err := parseStreamJobIDs(strings.Join(ids, ","))
	if err == nil {
		t.Fatalf("expected an error for %d distinct ids (max %d)", len(ids), streamMaxJobScope)
	}
}

// --- hub tests ---------------------------------------------------------

// TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe covers the shared-
// broadcaster lifecycle directly: started on the first subscriber, still
// running with two, and stopped only once the last one leaves.
func TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe(t *testing.T) {
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)

	id1, _, _, _, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	if !hubRunning(hub) {
		t.Fatal("expected ticker running after first subscribe")
	}

	id2, _, _, _, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	if n := hubSubCount(hub); n != 2 {
		t.Fatalf("expected 2 subs, got %d", n)
	}
	if !hubRunning(hub) {
		t.Fatal("expected ticker still running with two subscribers")
	}

	hub.unsubscribe(id1)
	if !hubRunning(hub) {
		t.Fatal("expected ticker still running with one subscriber left")
	}
	if n := hubSubCount(hub); n != 1 {
		t.Fatalf("expected 1 sub left, got %d", n)
	}

	hub.unsubscribe(id2)
	if hubRunning(hub) {
		t.Fatal("expected ticker stopped after last unsubscribe")
	}
	if n := hubSubCount(hub); n != 0 {
		t.Fatalf("expected 0 subs left, got %d", n)
	}
}

// hubRunning/hubSubCount read streamHub's mutex-guarded fields under its own
// lock — a bare field read here would itself be a data race in a
// concurrency test.
func hubRunning(h *streamHub) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancel != nil
}

func hubSubCount(h *streamHub) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// TestStreamHubTickNoOpAfterContextCancelled is issue #161 review finding #4:
// a tick running against an already-cancelled context (the run goroutine was
// parked on h.mu when unsubscribe cancelled it, and a new subscriber
// registered before this stale tick got the lock) must not send anything or
// touch subscriber state.
func TestStreamHubTickNoOpAfterContextCancelled(t *testing.T) {
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	_, ch, _, _, _, initial, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hub.tick(ctx)

	select {
	case got := <-ch:
		t.Fatalf("tick with a cancelled context must not send a frame, got %+v", got)
	default:
	}

	hub.mu.Lock()
	sub := hub.subs[0]
	hub.mu.Unlock()
	if sub.last.Down != initial.Down {
		t.Errorf("subscriber state changed after a no-op tick: last=%+v", sub.last)
	}
}

// TestStreamHubCorrelationTickBoundedByFetchTimeout is issue #266: run's
// corrTicker branch used to pass its own untimed ctx straight into
// h.fetchLive/h.refreshCorrelation, so a hung deps call wedged the whole
// single-goroutine select loop forever — including tick's 1Hz broadcast —
// while per-connection heartbeats kept flowing and masked the outage. It
// passes context.Background() directly (never cancelled by any caller,
// unlike TestStreamHubTickNoOpAfterContextCancelled above) with a
// liveTransfers dependency that blocks until its context is done, and
// asserts correlationTick still returns — bounded by its own
// streamFetchTimeout budget — instead of hanging.
func TestStreamHubCorrelationTickBoundedByFetchTimeout(t *testing.T) {
	blockingLive := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	hub := newStreamHub(noopJobs, blockingLive, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)

	done := make(chan struct{})
	go func() {
		hub.correlationTick(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * streamFetchTimeout):
		t.Fatal("correlationTick did not return within 2x streamFetchTimeout; hung deps call is not bounded (#266)")
	}
}

// TestStreamHubTickSendsChangedDataAndSuppressesUnchanged drives the
// broadcaster's tick() directly across multiple calls with changing and
// unchanged live data, and asserts on what a subscriber actually receives —
// covering the gap the #161 review's finding #10 called out: every prior
// HTTP-level test used tick = time.Hour, so tick() itself was never
// exercised on the non-empty path.
func TestStreamHubTickSendsChangedDataAndSuppressesUnchanged(t *testing.T) {
	var down int64 = 100
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: atomic.LoadInt64(&down)},
		}, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job:                 core.AlbumJob{ID: 7},
			Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
			AlbumBytesTotal:     1000,
			AlbumBytesRemaining: 1000,
		}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	// subscribe() itself does a synchronous correlation refresh, so the
	// fixture above is already loaded before the first tick. Scoped to job 7
	// (issue #268: unscoped subscribers get no job frames at all).
	id, ch, _, _, _, initial, _, _ := hub.subscribe(context.Background(), 0, map[int64]struct{}{7: {}}, false, "")
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].Speed != 100 {
		t.Fatalf("initial payload = %+v, want job 7 at speed 100", initial)
	}

	// Unchanged data: tick must not enqueue a second frame.
	hub.tick(context.Background())
	select {
	case got := <-ch:
		t.Fatalf("unchanged tick must not send a frame, got %+v", got)
	default:
	}

	// Changed data: tick must enqueue a frame reflecting it.
	atomic.StoreInt64(&down, 250)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 || got.Jobs[0].Speed != 250 {
			t.Errorf("changed tick payload = %+v, want job 7 at speed 250", got)
		}
	default:
		t.Fatal("changed tick must send a frame")
	}
}

// newInvalidateFixture builds a hub jobsFn that returns a single job, mutable
// via changing title (a fingerprint field) so tests can force a generation
// bump between correlationTicks.
func newInvalidateFixture() (jobsFn JobsFunc, setTitle func(string), setErr func(error)) {
	var mu sync.Mutex
	title := "A"
	var err error
	jobsFn = func(ctx context.Context) ([]core.JobView, error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			return nil, err
		}
		return []core.JobView{{Job: core.AlbumJob{ID: 1, Title: title}}}, nil
	}
	setTitle = func(t string) {
		mu.Lock()
		defer mu.Unlock()
		title = t
	}
	setErr = func(e error) {
		mu.Lock()
		defer mu.Unlock()
		err = e
	}
	return jobsFn, setTitle, setErr
}

// TestStreamHubNoInvalidateOnFirstRefresh proves the first-ever refresh never
// bumps jobsGeneration (issue #275): there is nothing to compare the very
// first fingerprint against, so hasFingerprint alone must suppress a bump.
func TestStreamHubNoInvalidateOnFirstRefresh(t *testing.T) {
	jobsFn, _, _ := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, _, _, ich, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id)

	hub.tick(context.Background())
	select {
	case got := <-ich:
		t.Fatalf("first refresh must not invalidate, got %+v", got)
	default:
	}
	if hub.generationSnapshot() != 0 {
		t.Fatalf("jobsGeneration = %d, want 0 after only the first refresh", hub.generationSnapshot())
	}
}

// TestStreamHubFreshSubscriberGetsNoImmediateInvalidate proves subscribe's
// two initial values together (issue #275, decision 3): lastInvalidateAt =
// now, so the client's own onopen refetch isn't immediately followed by a
// redundant server-sent one, AND lastSeenGeneration = h.generationSnapshot()
// (read AFTER subscribe's own synchronous refresh), so a subscriber that
// connects when jobsGeneration is already > 0 doesn't treat that pre-existing
// generation as unseen.
//
// An interval of time.Hour (the original version of this test) makes the
// throttle condition (`now.Sub(lastInvalidateAt) >= invalidateInterval`)
// false regardless of either initial value, which is why that version could
// not actually fail under either mutation it claimed to guard: this uses a
// short interval and ticks past it with the generation already primed to a
// nonzero value BEFORE subscribing, so only a correctly-initialized
// lastSeenGeneration keeps the subscriber silent once the window elapses.
func TestStreamHubFreshSubscriberGetsNoImmediateInvalidate(t *testing.T) {
	jobsFn, setTitle, _ := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, 30*time.Millisecond)

	// Prime the fingerprint, then bump jobsGeneration to a nonzero value
	// BEFORE subscribing, so a fresh subscriber sees a generation already in
	// play rather than the zero value a first-ever refresh would leave.
	hub.correlationTick(context.Background())
	setTitle("B")
	hub.correlationTick(context.Background())
	if hub.generationSnapshot() != 1 {
		t.Fatalf("jobsGeneration = %d, want 1 before subscribing", hub.generationSnapshot())
	}

	id, _, _, ich, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id)

	time.Sleep(40 * time.Millisecond) // past invalidateInterval
	hub.tick(context.Background())
	select {
	case got := <-ich:
		t.Fatalf("subscribe must have already caught up to jobsGeneration=1, so no invalidate should fire with no further change, got %+v", got)
	default:
	}
}

// TestStreamHubInvalidatesAfterThrottleElapses is the direct test of the
// throttle window (issue #275, decision 1): a generation bump produces
// nothing until invalidateInterval has elapsed since this subscriber's last
// invalidation (connect counts as one), then produces exactly one frame.
// The "nothing" half is asserted with an immediately-following tick so a
// slow machine can't flake it into a false pass.
func TestStreamHubInvalidatesAfterThrottleElapses(t *testing.T) {
	jobsFn, setTitle, _ := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, 50*time.Millisecond)
	id, _, _, ich, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id)

	setTitle("B")
	hub.correlationTick(context.Background())
	hub.tick(context.Background())
	select {
	case got := <-ich:
		t.Fatalf("must not invalidate before the throttle window elapses, got %+v", got)
	default:
	}

	time.Sleep(60 * time.Millisecond)
	hub.tick(context.Background())
	select {
	case got := <-ich:
		if got.Generation != 1 {
			t.Errorf("Generation = %d, want 1", got.Generation)
		}
	default:
		t.Fatal("must invalidate once the throttle window elapses")
	}
}

// TestStreamHubGenerationBumpInsideThrottleWindowIsNotLost is the direct
// test of decision 3 (a counter, not a dirty flag): two generation bumps
// occurring back-to-back inside one subscriber's throttle window must not be
// collapsed into "no signal" — the subscriber must still see the LATEST
// generation once the window elapses, not miss it because some earlier tick
// already "consumed" a bump.
func TestStreamHubGenerationBumpInsideThrottleWindowIsNotLost(t *testing.T) {
	jobsFn, setTitle, _ := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, 50*time.Millisecond)
	id, _, _, ich, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id)

	setTitle("B")
	hub.correlationTick(context.Background())
	setTitle("C")
	hub.correlationTick(context.Background())
	if hub.generationSnapshot() != 2 {
		t.Fatalf("jobsGeneration = %d, want 2 after two distinct changes", hub.generationSnapshot())
	}

	hub.tick(context.Background())
	select {
	case got := <-ich:
		t.Fatalf("must not invalidate before the throttle window elapses, got %+v", got)
	default:
	}

	time.Sleep(60 * time.Millisecond)
	hub.tick(context.Background())
	select {
	case got := <-ich:
		if got.Generation != 2 {
			t.Errorf("Generation = %d, want 2 (the latest, not lost)", got.Generation)
		}
	default:
		t.Fatal("must invalidate exactly once carrying the latest generation")
	}
}

// TestStreamHubFailedJobsFetchLeavesGenerationUntouched proves
// refreshCorrelation's existing early return on a failed h.jobs call also
// protects the fingerprint/generation state (issue #275): a transient
// Postgres hiccup must not be indistinguishable from "the data changed".
func TestStreamHubFailedJobsFetchLeavesGenerationUntouched(t *testing.T) {
	jobsFn, _, setErr := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, _, _, ich, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id)

	setErr(fmt.Errorf("boom"))
	hub.correlationTick(context.Background())
	if hub.generationSnapshot() != 0 {
		t.Fatalf("jobsGeneration = %d, want 0 after a failed fetch", hub.generationSnapshot())
	}

	hub.tick(context.Background())
	select {
	case got := <-ich:
		t.Fatalf("a failed fetch must not invalidate, got %+v", got)
	default:
	}
}

// TestStreamHubInvalidateSentToSubscriberWithoutThroughput is the regression
// guard for the invalidate block's placement in tick (issue #275): it must
// run BEFORE the `if !sub.wantThroughput { continue }` guard, or every
// non-Overview subscriber would silently never receive an invalidation.
func TestStreamHubInvalidateSentToSubscriberWithoutThroughput(t *testing.T) {
	jobsFn, setTitle, _ := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Millisecond)
	id, _, _, ich, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "") // wantThroughput = false
	defer hub.unsubscribe(id)

	setTitle("B")
	hub.correlationTick(context.Background())
	time.Sleep(2 * time.Millisecond)
	hub.tick(context.Background())
	select {
	case got := <-ich:
		if got.Generation != 1 {
			t.Errorf("Generation = %d, want 1", got.Generation)
		}
	default:
		t.Fatal("a wantThroughput=false subscriber must still receive an invalidate frame")
	}
}

// TestStreamHubInvalidateThrottleIsPerSubscriber proves each subscriber's
// throttle window is tracked independently: one connecting 80ms after
// another must not inherit the first's window and must fire on its own
// schedule.
//
// Interval/offsets scaled 4x from an earlier version (50ms/20ms/35ms/20ms)
// that left only 15ms of margin on its negative assertion (sub2 must not
// have fired at ~35ms elapsed against a 50ms window) — routine scheduling
// jitter on a loaded CI box could overshoot that and trip t.Fatalf. 200ms/
// 80ms/140ms/80ms keeps the identical proportions (and thus the identical
// property under test) with 60ms of margin, at a negligible wall-clock cost.
func TestStreamHubInvalidateThrottleIsPerSubscriber(t *testing.T) {
	jobsFn, setTitle, _ := newInvalidateFixture()
	hub := newStreamHub(jobsFn, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, 200*time.Millisecond)
	id1, _, _, ich1, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id1)

	time.Sleep(80 * time.Millisecond)
	id2, _, _, ich2, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id2)

	setTitle("B")
	hub.correlationTick(context.Background())

	time.Sleep(140 * time.Millisecond) // sub1: ~220ms since connect, past window; sub2: ~140ms, still inside
	hub.tick(context.Background())
	select {
	case got := <-ich1:
		if got.Generation != 1 {
			t.Errorf("sub1 Generation = %d, want 1", got.Generation)
		}
	default:
		t.Fatal("sub1's window has elapsed and must invalidate")
	}
	select {
	case got := <-ich2:
		t.Fatalf("sub2's window has not elapsed yet, must not invalidate, got %+v", got)
	default:
	}

	time.Sleep(80 * time.Millisecond) // sub2: now past its own window
	hub.tick(context.Background())
	select {
	case got := <-ich2:
		if got.Generation != 1 {
			t.Errorf("sub2 Generation = %d, want 1", got.Generation)
		}
	default:
		t.Fatal("sub2's window has now elapsed and must invalidate")
	}
}

// TestStreamHubTickSendsUploadOnlyDeltaWithIndependentWatermark ports the
// pre-#265 test of the same name to the now-independent throughput channel:
// the two directions' watermarks stay independent of one another, an
// upload-only sample must produce an `event: throughput` frame carrying only
// that direction, and a subscriber that connected after the sample already
// exists must not receive it again on its own next tick.
func TestStreamHubTickSendsUploadOnlyDeltaWithIndependentWatermark(t *testing.T) {
	t1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	var uploadAdvanced atomic.Bool
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		series := core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: t1, BytesPerSecond: 100}},
			Upload:   []core.ThroughputSample{{At: t1, BytesPerSecond: 300}},
		}
		if uploadAdvanced.Load() {
			series.Upload = append(series.Upload, core.ThroughputSample{At: t2, BytesPerSecond: 300})
		}
		return series, nil
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, _, tch, _, _, initial, initialThroughput, _ := hub.subscribe(context.Background(), 0, nil, true, "")
	defer hub.unsubscribe(id)
	if initial.Down != 100 || initial.Up != 300 {
		t.Fatalf("initial down/up = %d/%d, want 100/300", initial.Down, initial.Up)
	}
	if len(initialThroughput.Upload) != 1 {
		t.Fatalf("first subscriber initial upload series = %+v, want just t1", initialThroughput.Upload)
	}

	uploadAdvanced.Store(true)
	id2, _, tch2, _, _, _, secondInitialThroughput, _ := hub.subscribe(context.Background(), 0, nil, true, "")
	defer hub.unsubscribe(id2)
	if len(secondInitialThroughput.Upload) != 2 {
		t.Fatalf("second subscriber initial upload series = %+v, want t1 and t2", secondInitialThroughput.Upload)
	}

	hub.tick(context.Background())
	select {
	case got := <-tch:
		if len(got.Download) != 0 {
			t.Errorf("download delta = %+v, want none", got.Download)
		}
		if len(got.Upload) != 1 || got.Upload[0].At != t2.Format(timeFormat) {
			t.Errorf("upload delta = %+v, want only t2", got.Upload)
		}
	default:
		t.Fatal("upload-only sample must trigger a throughput frame")
	}
	select {
	case got := <-tch2:
		t.Fatalf("newer subscriber must not receive already-seen upload sample, got %+v", got)
	default:
	}

	hub.mu.Lock()
	sub1, sub2 := hub.subs[id], hub.subs[id2]
	downloadAt1, uploadAt1 := sub1.lastDownloadThroughputAt, sub1.lastUploadThroughputAt
	downloadAt2, uploadAt2 := sub2.lastDownloadThroughputAt, sub2.lastUploadThroughputAt
	hub.mu.Unlock()
	if !downloadAt1.Equal(t1) || !uploadAt1.Equal(t2) || !downloadAt2.Equal(t1) || !uploadAt2.Equal(t2) {
		t.Errorf("subscriber watermarks = first %s/%s second %s/%s, want %s/%s for both", downloadAt1, uploadAt1, downloadAt2, uploadAt2, t1, t2)
	}
}

// TestStreamHubTickThroughputSurvivesUndrainedMailbox is the interaction-
// level test issue #265 demands: sendLatestThroughput's accumulate-rather-
// than-discard contract must actually hold across real tick() calls, not
// just in its own unit test in isolation. A subscriber's tch is deliberately
// never drained across two ticks with distinct samples; the third tick
// drains everything at once, and every sample from all three ticks must be
// present exactly once, none skipped.
func TestStreamHubTickThroughputSurvivesUndrainedMailbox(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	samples := []core.ThroughputSample{{At: t0, BytesPerSecond: 100}}
	var mu sync.Mutex
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]core.ThroughputSample, len(samples))
		copy(out, samples)
		return core.ThroughputSeries{Download: out}, nil
	}
	addSample := func(at time.Time, bps int64) {
		mu.Lock()
		defer mu.Unlock()
		samples = append(samples, core.ThroughputSample{At: at, BytesPerSecond: bps})
	}

	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, _, tch, _, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, true, "")
	defer hub.unsubscribe(id)

	addSample(t0.Add(1*time.Second), 200)
	hub.tick(context.Background()) // queues t0+1s, undrained
	addSample(t0.Add(2*time.Second), 300)
	hub.tick(context.Background()) // must accumulate onto the undrained frame, not discard it
	addSample(t0.Add(3*time.Second), 400)
	hub.tick(context.Background())

	got := <-tch
	wantAts := []string{
		t0.Add(1 * time.Second).Format(timeFormat),
		t0.Add(2 * time.Second).Format(timeFormat),
		t0.Add(3 * time.Second).Format(timeFormat),
	}
	if len(got.Download) != len(wantAts) {
		t.Fatalf("Download = %+v, want %d samples across all three ticks", got.Download, len(wantAts))
	}
	for i, want := range wantAts {
		if got.Download[i].At != want {
			t.Errorf("Download[%d].At = %q, want %q", i, got.Download[i].At, want)
		}
	}
}

// TestStreamHubTickBuildsNoThroughputFrameWithoutSubscriber covers the two
// halves of issue #265's most subtle invariant: a subscriber that never
// asked for ?throughput=1 must never receive an `event: throughput` frame at
// all, but must still see a fresh `down`/`up` on its `live` frame from the
// very newest sample — proving fetchThroughput itself was never gated,
// only the per-subscriber send.
func TestStreamHubTickBuildsNoThroughputFrameWithoutSubscriber(t *testing.T) {
	var bps atomic.Int64
	bps.Store(100)
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: time.Now(), BytesPerSecond: bps.Load()}},
		}, nil
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, ch, tch, _, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, false, "")
	defer hub.unsubscribe(id)

	bps.Store(250)
	hub.tick(context.Background())

	select {
	case got := <-tch:
		t.Fatalf("subscriber without ?throughput=1 must never receive a throughput frame, got %+v", got)
	default:
	}
	select {
	case got := <-ch:
		if got.Down != 250 {
			t.Errorf("live frame Down = %d, want 250 (fetchThroughput must not be gated by wantThroughput)", got.Down)
		}
	default:
		t.Fatal("expected a live frame reflecting the new Down rate")
	}
}

// TestStreamHubTickWatermarkSurvivesSubSecondPrecision is issue #265's
// review finding: tick used to advance the throughput watermark by
// round-tripping the sample's time.Time through timeFormat (no fractional
// seconds) via time.Parse, which truncates a real sample's sub-second
// component and moves the watermark BACKWARDS by up to 999ms relative to the
// sample just sent. newThroughputSince would then re-select that same
// already-sent sample on every subsequent tick forever, so a subscriber got
// a duplicate `event: throughput` frame every second even when the meter
// produced nothing new. Every other test in this file builds its timestamps
// with whole-second time.Date(...), which cannot see this: a whole-second
// value survives the truncating round trip unchanged. This one uses a
// sub-second offset (237ms) so the bug is observable: a tick with a fresh
// sample must send exactly one frame, and a second tick with no further
// sample must send none.
func TestStreamHubTickWatermarkSurvivesSubSecondPrecision(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(237 * time.Millisecond)
	var mu sync.Mutex
	samples := []core.ThroughputSample{{At: t0, BytesPerSecond: 100}}
	throughputFn := func(ctx context.Context) (core.ThroughputSeries, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]core.ThroughputSample, len(samples))
		copy(out, samples)
		return core.ThroughputSeries{Download: out}, nil
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, throughputFn, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, _, tch, _, _, _, _, _ := hub.subscribe(context.Background(), 0, nil, true, "")
	defer hub.unsubscribe(id)

	mu.Lock()
	samples = append(samples, core.ThroughputSample{At: t1, BytesPerSecond: 150})
	mu.Unlock()

	hub.tick(context.Background())
	select {
	case got := <-tch:
		if len(got.Download) != 1 || got.Download[0].At != t1.Format(timeFormat) {
			t.Fatalf("first tick with a fresh sub-second sample: got %+v, want just t1", got)
		}
	default:
		t.Fatal("expected a throughput frame carrying the fresh sub-second sample")
	}

	// No new sample added: a second tick must NOT resend t1. Before the fix,
	// the watermark truncated t1's milliseconds on the way in, so
	// newThroughputSince re-selected the same sample here.
	hub.tick(context.Background())
	select {
	case got := <-tch:
		t.Fatalf("second tick with no new sample must send nothing, got %+v", got)
	default:
	}
}

// TestStreamHubScopedJobPicksUpLiveMatchWithinOneTick covers a scoped
// subscriber's requested job gaining a live match: its jobDTO reflects the
// live Speed the very next tick. Unlike the pre-#268 unscoped design, this
// no longer depends on the event-driven correlation refresh at all — a
// scoped job is already in viewByJob (it's in "wanted" the moment it's
// requested, live-matched or not — see refreshCorrelation), and Speed is
// always computed from that tick's own fresh live fetch (toJobDTO's idx
// parameter), never from a cached value. correlationInterval is still
// time.Hour here specifically to prove that.
func TestStreamHubScopedJobPicksUpLiveMatchWithinOneTick(t *testing.T) {
	var liveOn atomic.Bool
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		if !liveOn.Load() {
			return nil, nil
		}
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, Speed: 42},
		}, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job:                 core.AlbumJob{ID: 9},
			Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
			AlbumBytesTotal:     1000,
			AlbumBytesRemaining: 1000,
		}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, ch, _, _, _, initial, _, _ := hub.subscribe(context.Background(), 0, map[int64]struct{}{9: {}}, false, "")
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].Speed != 0 {
		t.Fatalf("initial payload = %+v, want job 9 present with no live match yet", initial)
	}

	liveOn.Store(true)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 || got.Jobs[0].ID != 9 || got.Jobs[0].Speed != 42 {
			t.Errorf("expected job 9's speed picked up within one tick, got %+v", got)
		}
	default:
		t.Fatal("expected a frame once the candidate became live-matched")
	}
}

// TestStreamHubFileLevelRefreshKeepsMultiFileAlbumTotalMonotonic is issue
// #258 review finding B1: the event-driven refresh trigger must compare
// live-matched FILES, not live-matched CANDIDATES. A multi-file candidate
// stays "matched" at candidate granularity as long as ANY of its files is
// still live, so a candidate-granular trigger would never fire when one file
// among several completes and leaves ListDownloads — serving that one
// file's stale persisted bytes (from the last correlationInterval refresh)
// and producing a visible backwards step in the album total, exactly the bug
// commit 99fc7aa's now-removed byte floor existed to paper over.
func TestStreamHubFileLevelRefreshKeepsMultiFileAlbumTotalMonotonic(t *testing.T) {
	var aLive atomic.Bool
	aLive.Store(true)
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		// Speed is set on both files so that a.flac leaving the live list
		// entirely always changes the album's aggregated Speed (900 -> 400)
		// and so always produces a frame to assert on, regardless of
		// whether BytesDone itself happens to move — Speed changing is not
		// what this test is about, but it's what makes the frame arrive
		// deterministically so the streamed BytesDone can be checked
		// directly instead of only inferred from the cache.
		live := []core.RemoteTransfer{
			{Username: "alice", Filename: "b.flac", State: core.TransferInProgress, BytesDone: 3_000_000, Speed: 400},
		}
		if aLive.Load() {
			live = append(live, core.RemoteTransfer{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 9_000_000, Speed: 500})
		}
		return live, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job: core.AlbumJob{ID: 11},
			Attempt: &core.Candidate{ID: 101, Username: "alice", Files: []core.CandidateFile{
				{Filename: "a.flac", Size: 9_000_000},
				{Filename: "b.flac", Size: 3_000_000},
			}},
			AlbumBytesTotal:     12_000_000,
			AlbumBytesRemaining: 12_000_000,
		}}, nil
	}
	// Simulates reconcile: while a.flac is still live it hasn't been
	// persisted at its final size yet (5MB, an arbitrary partial figure);
	// the instant it completes and leaves the live list, its persisted bytes
	// are already final (9MB) — reconcile always persists before purging
	// (jobBytesDone's doc comment). A correctly-firing per-file refresh picks
	// that up in the very same tick; a candidate-granular one (still
	// "matched" via b.flac) would keep serving the stale 5MB.
	transferBytesFn := func(ctx context.Context, ids []int64) (map[int64]map[string]int64, error) {
		if aLive.Load() {
			return map[int64]map[string]int64{101: {"a.flac": 5_000_000, "b.flac": 3_000_000}}, nil
		}
		return map[int64]map[string]int64{101: {"a.flac": 9_000_000, "b.flac": 3_000_000}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, transferBytesFn, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, ch, _, _, _, initial, _, _ := hub.subscribe(context.Background(), 0, map[int64]struct{}{11: {}}, false, "")
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 {
		t.Fatalf("initial payload = %+v, want job 11 present", initial)
	}
	if before := initial.Jobs[0].BytesDone; before != 12_000_000 {
		t.Fatalf("initial BytesDone = %d, want 12_000_000 (both files live)", before)
	}

	// a.flac completes and leaves ListDownloads; b.flac stays live, so the
	// candidate as a whole is still "matched" at candidate granularity — only
	// a file-granular trigger notices this transition at all. Speed dropping
	// from 900 (both files) to 400 (b.flac alone) guarantees a frame is sent
	// regardless of what BytesDone does, so the streamed BytesDone can be
	// asserted directly rather than only inferred from the cache.
	aLive.Store(false)
	hub.tick(context.Background())

	select {
	case got := <-ch:
		if len(got.Jobs) != 1 {
			t.Fatalf("expected job 11 in the frame after a.flac completes, got %+v", got)
		}
		if got.Jobs[0].BytesDone != 12_000_000 {
			t.Errorf("streamed BytesDone = %d, want 12_000_000 (a.flac's now-final persisted bytes + b.flac's live bytes) — a candidate-granular trigger would regress this to 8_000_000 by keeping a.flac's stale 5MB persisted figure", got.Jobs[0].BytesDone)
		}
	default:
		t.Fatal("expected a frame once a.flac left the live list (Speed alone must have changed)")
	}

	// Corroborating evidence at the cache level: the refresh must actually
	// have run, not just happened to leave BytesDone unchanged.
	if got := hub.bytesSnapshot()[101]["a.flac"]; got != 9_000_000 {
		t.Errorf("bytesByCandidate[101][\"a.flac\"] = %d, want 9_000_000 — the event-driven refresh must fire on a.flac's own live-match transition even though candidate 101 (via b.flac) stays live-matched throughout", got)
	}
}

// TestStreamHubPreservesBytesCacheWhenTransferBytesFailsDuringEventDrivenRefresh
// is issue #258 review finding C1: refreshCorrelation now runs under tick's
// own tighter streamFetchTimeout budget (already partly consumed by
// fetchLive/fetchThroughput) whenever the event-driven trigger fires, not
// only from subscribe/run's untimed contexts as on origin/main — a real risk
// of a mid-refresh deadline exceeded that didn't exist before. A failed
// transferBytes call must leave h.bytesByCandidate as it was, not nil: nil
// would make jobBytesDone fall back to the store's own (possibly much lower
// or stale) AlbumBytesDone, producing exactly the kind of backwards step
// this whole PR exists to remove.
func TestStreamHubPreservesBytesCacheWhenTransferBytesFailsDuringEventDrivenRefresh(t *testing.T) {
	var aLive atomic.Bool
	aLive.Store(true)
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		live := []core.RemoteTransfer{
			{Username: "dave", Filename: "b.flac", State: core.TransferInProgress, BytesDone: 3_000_000, Speed: 400},
		}
		if aLive.Load() {
			live = append(live, core.RemoteTransfer{Username: "dave", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 9_000_000, Speed: 500})
		}
		return live, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{{
			Job: core.AlbumJob{ID: 41},
			Attempt: &core.Candidate{ID: 301, Username: "dave", Files: []core.CandidateFile{
				{Filename: "a.flac", Size: 9_000_000},
				{Filename: "b.flac", Size: 3_000_000},
			}},
			// Deliberately far below the correct 12M, so a fallback to this
			// value on a failed transferBytes fetch is unmistakable.
			AlbumBytesDone:      4_000_000,
			AlbumBytesTotal:     12_000_000,
			AlbumBytesRemaining: 12_000_000,
		}}, nil
	}
	transferBytesFn := func(ctx context.Context, ids []int64) (map[int64]map[string]int64, error) {
		if aLive.Load() {
			return map[int64]map[string]int64{301: {"a.flac": 9_000_000, "b.flac": 3_000_000}}, nil
		}
		// The event-driven refresh triggered by a.flac's own transition runs
		// out of budget (see this test's doc comment).
		return nil, context.DeadlineExceeded
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, transferBytesFn, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, ch, _, _, _, initial, _, _ := hub.subscribe(context.Background(), 0, map[int64]struct{}{41: {}}, false, "")
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].BytesDone != 12_000_000 {
		t.Fatalf("initial payload = %+v, want job 41 at 12_000_000 bytes", initial)
	}

	// a.flac completes and leaves ListDownloads at the same moment
	// transferBytes starts failing; b.flac stays live.
	aLive.Store(false)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 {
			t.Fatalf("expected job 41 in the frame, got %+v", got)
		}
		if got.Jobs[0].BytesDone != 12_000_000 {
			t.Errorf("streamed BytesDone = %d, want 12_000_000 (preserved cache) — a failed transferBytes fetch must not fall back to the stale AlbumBytesDone (4_000_000)", got.Jobs[0].BytesDone)
		}
	default:
		t.Fatal("expected a frame once a.flac left the live list (Speed alone must have changed)")
	}

	if got := hub.bytesSnapshot()[301]; got == nil {
		t.Error("bytesByCandidate[301] = nil, want the preserved pre-failure map")
	} else if got["a.flac"] != 9_000_000 || got["b.flac"] != 3_000_000 {
		t.Errorf("bytesByCandidate[301] = %+v, want the preserved {a.flac: 9_000_000, b.flac: 3_000_000}", got)
	}
}

// TestStreamHubScopedJobViewRefreshesPromptlyOnLiveMatchChange is issue
// #268's simplification of the old B3/C2 mechanism: a SCOPED job never
// leaves viewByJob's "wanted" set (it's requested, live-matched or not — see
// refreshCorrelation), so there is no "eviction race" to guard against
// anymore, and scopedJobIDs' trackedIDs (which existed only for that race)
// is gone. What remains is simpler and still worth covering: the
// event-driven refresh (triggered by the live-matched file set changing)
// still refreshes this job's view promptly, so a DB state transition
// (DOWNLOADING -> IMPORTING) that happens in the same window a live transfer
// disappears is reflected without waiting a full correlationInterval.
func TestStreamHubScopedJobViewRefreshesPromptlyOnLiveMatchChange(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	liveFn := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		if !live.Load() {
			return nil, nil
		}
		return []core.RemoteTransfer{
			{Username: "carol", Filename: "01.flac", State: core.TransferInProgress, Speed: 999, BytesDone: 44_000_000},
		}, nil
	}
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		job := core.AlbumJob{ID: 30, State: core.StateDownloading, UpdatedAt: t0}
		albumBytesDone := int64(44_000_000)
		if !live.Load() {
			// Simulates reconcile finishing the download in the SAME window
			// the live transfer disappears: the persisted row moves on to
			// IMPORTING with its final byte count, not just a reverted
			// DOWNLOADING snapshot.
			job.State = core.StateImporting
			job.UpdatedAt = t1
			albumBytesDone = 49_000_000
		}
		return []core.JobView{{
			Job:                 job,
			Attempt:             &core.Candidate{Username: "carol", Files: []core.CandidateFile{{Filename: "01.flac", Size: 49_000_000}}},
			AlbumBytesDone:      albumBytesDone,
			AlbumBytesTotal:     49_000_000,
			AlbumBytesRemaining: 49_000_000 - albumBytesDone,
		}}, nil
	}
	hub := newStreamHub(jobsFn, liveFn, noopThroughput, noopTransferBytes, nil, nil, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, ch, _, _, _, initial, _, _ := hub.subscribe(context.Background(), 0, map[int64]struct{}{30: {}}, false, "")
	defer hub.unsubscribe(id)
	if len(initial.Jobs) != 1 || initial.Jobs[0].Speed != 999 {
		t.Fatalf("initial payload = %+v, want job 30 at speed 999", initial)
	}

	// The transfer disappears entirely (finished) at the same moment
	// reconcile commits the job's transition to IMPORTING.
	live.Store(false)
	hub.tick(context.Background())
	select {
	case got := <-ch:
		if len(got.Jobs) != 1 || got.Jobs[0].ID != 30 {
			t.Fatalf("expected an updated frame for job 30, got %+v", got)
		}
		dto := got.Jobs[0]
		if dto.Speed != 0 {
			t.Errorf("Speed = %d, want 0 (reverted)", dto.Speed)
		}
		if dto.State != string(core.StateImporting) {
			t.Errorf("State = %q, want %q (fresh view picked up promptly)", dto.State, core.StateImporting)
		}
		if dto.BytesDone != 49_000_000 {
			t.Errorf("BytesDone = %d, want 49_000_000 (fresh view's final AlbumBytesDone)", dto.BytesDone)
		}
		if dto.UpdatedAt != t1.Format(timeFormat) {
			t.Errorf("UpdatedAt = %q, want %q (fresh view's timestamp)", dto.UpdatedAt, t1.Format(timeFormat))
		}
	default:
		t.Fatal("expected an updated frame reflecting the state transition")
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
// tick/correlation/heartbeat/invalidate intervals instead of the real
// constants.
func newStreamTestServer(deps ServerDeps, tick, correlation, heartbeat, invalidate time.Duration) http.Handler {
	mux := http.NewServeMux()
	registerStream(mux, deps, tick, correlation, heartbeat, invalidate)
	return mux
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
	// Long tick/correlation/heartbeat: this test only cares about the
	// immediate first frame sent at subscribe time, not the periodic timers.
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

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
	mux := newStreamTestServer(deps, time.Hour, time.Hour, 15*time.Millisecond, time.Hour)

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
	mux := newStreamTestServer(deps, 5*time.Millisecond, time.Hour, time.Hour, time.Hour)

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
	// jobs is omitted (omitempty, no live-matched jobs) but down/up are
	// explicit zeroes, never omitted — see downSpeed/upSpeed's fallbacks.
	if body := rec.String(); !strings.Contains(body, `{"down":0,"up":0}`) {
		t.Errorf("nil live sources must emit explicit zero rates with jobs omitted, got %q", body)
	}
}

// TestStreamEndpointTwoClientsBothGetInitialFrame is the HTTP-level
// counterpart of TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe: two
// concurrent connections against the same mux both get served (and both get
// their own first payload) without either blocking the other. It does not
// itself prove loop sharing — that's TestStreamHubSharesOneLoopAndStopsOnLastUnsubscribe's
// job — only that the HTTP layer doesn't serialize or drop either client.
func TestStreamEndpointTwoClientsBothGetInitialFrame(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

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
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

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
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	for _, raw := range []string{"not-a-number", "0", "-5"} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/stream?job="+raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestStreamEndpointInvalidJobsParamReturns400 covers ?jobs= validation
// (issue #258): non-numeric, non-positive, or empty entries, and an array
// over streamMaxJobScope, must all 400 rather than silently degrading.
func TestStreamEndpointInvalidJobsParamReturns400(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	over := make([]string, streamMaxJobScope+1)
	for i := range over {
		over[i] = strconv.Itoa(i + 1)
	}

	for _, raw := range []string{"abc", "0", "-1", "1,,2", strings.Join(over, ",")} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/stream?jobs="+raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestStreamEndpointInvalidThroughputParamReturns400 covers ?throughput=
// parsing (issue #265): strict like ?jobs=, so anything other than exactly
// "1" is a 400 rather than silently defaulting to off.
func TestStreamEndpointInvalidThroughputParamReturns400(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	for _, raw := range []string{"0", "true", "yes", "2", "-1"} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/stream?throughput="+raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestStreamEndpointThroughputOptInSendsEvent covers ?throughput=1 end to
// end at the wire level: an `event: throughput` line must appear in the raw
// SSE body.
func TestStreamEndpointThroughputOptInSendsEvent(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.Throughput = func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: time.Now(), BytesPerSecond: 123}},
		}, nil
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream?throughput=1", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: throughput")
	cancel()
	<-done

	if !strings.Contains(rec.String(), `"download":[`) {
		t.Errorf("expected the download series in the throughput frame, got %q", rec.String())
	}
}

// TestStreamEndpointNoThroughputParamNeverSendsEvent is the negative half:
// without ?throughput=1, no `event: throughput` line ever appears, even
// though samples exist and would otherwise be sent.
func TestStreamEndpointNoThroughputParamNeverSendsEvent(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.Throughput = func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{
			Download: []core.ThroughputSample{{At: time.Now(), BytesPerSecond: 123}},
		}, nil
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: live")
	time.Sleep(20 * time.Millisecond) // let the handler finish writing whatever it's going to write
	cancel()
	<-done

	if strings.Contains(rec.String(), "event: throughput") {
		t.Errorf("no ?throughput=1: expected no throughput event, got %q", rec.String())
	}
}

// TestStreamEndpointJobsScopeRestrictsToRequestedIDs covers the ?jobs=
// scoping contract itself end to end: a subscriber that names an id set only
// ever gets jobs from that set, even when other jobs exist and are
// live-matched.
func TestStreamEndpointJobsScopeRestrictsToRequestedIDs(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 10},
			{Username: "bob", Filename: "02.flac", State: core.TransferInProgress, BytesDone: 20},
		}, nil
	}
	deps.Jobs = func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{Job: core.AlbumJob{ID: 1}, Attempt: &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 100}}}},
			{Job: core.AlbumJob{ID: 2}, Attempt: &core.Candidate{Username: "bob", Files: []core.CandidateFile{{Filename: "02.flac", Size: 100}}}},
		}, nil
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/stream?jobs=1", nil)
	rec := newTestStreamRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { mux.ServeHTTP(rec, req); close(done) }()

	waitForBody(t, rec, "event: live")
	cancel()
	<-done

	body := rec.String()
	if !strings.Contains(body, `"id":1`) {
		t.Errorf("expected requested job 1 in body, got %q", body)
	}
	if strings.Contains(body, `"id":2`) {
		t.Errorf("job 2 must not appear for a subscriber scoped to ?jobs=1, got %q", body)
	}
}

// TestStreamEndpointFedByOwnCorrelationRefresh covers the job<->candidate
// correlation path end to end without any GET /api/jobs call: the hub's
// deps.Jobs and deps.JobDetail are queried directly by subscribe()'s
// synchronous refresh, so ?job=<id> reports live per-file data on the very
// first frame rather than after a whole correlationInterval.
func TestStreamEndpointFedByOwnCorrelationRefresh(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.LiveTransfers = func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 250},
		}, nil
	}
	deps.Jobs = func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:                 core.AlbumJob{ID: 42},
				Attempt:             &core.Candidate{Username: "alice", Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
				AlbumBytesTotal:     1000,
				AlbumBytesRemaining: 1000,
			},
		}, nil
	}
	deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
		t.Fatal("GET /api/stream must never call PagedJobs")
		return PagedJobsResult{}, nil
	}
	deps.JobDetail = func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{
			Job: core.AlbumJob{ID: jobID},
			Attempts: []core.AttemptDetail{{
				Attempt:   core.Candidate{ID: 1, Username: "alice"},
				Transfers: []core.Transfer{{Username: "alice", Filename: "01.flac", State: core.TransferInProgress, BytesTotal: 1000}},
			}},
		}, true, nil
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

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

// TestStreamEndpointWritesInvalidateEvent is the only test in this file
// pinning the literal wire bytes for `event: invalidate` (issue #275): the
// event name and the `generation` JSON key are exactly what
// web/src/api/stream.tsx depends on, so a rename either side would otherwise
// not be caught until the frontend silently stopped refetching.
func TestStreamEndpointWritesInvalidateEvent(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	var mu sync.Mutex
	title := "A"
	deps.Jobs = func(ctx context.Context) ([]core.JobView, error) {
		mu.Lock()
		defer mu.Unlock()
		return []core.JobView{{Job: core.AlbumJob{ID: 1, Title: title}}}, nil
	}
	// tick/correlation/invalidate all short so the change below is picked up
	// and delivered promptly; heartbeat stays out of the way at time.Hour.
	mux := newStreamTestServer(deps, 5*time.Millisecond, 5*time.Millisecond, time.Hour, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: live")

	mu.Lock()
	title = "B"
	mu.Unlock()

	waitForBody(t, rec, "event: invalidate\ndata: {\"generation\":1}\n\n")
	cancel()
	<-done
}

// TestStreamEndpointRejectsOverCapacity is issue #161 review finding #7: past
// streamMaxSubscribers open connections, a new one must get a proper 503
// rather than an unbounded broadcaster fan-out.
func TestStreamEndpointRejectsOverCapacity(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	var cancels []context.CancelFunc
	var dones []chan struct{}
	for i := 0; i < streamMaxSubscribers; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
		rec := newTestStreamRecorder()
		done := make(chan struct{})
		dones = append(dones, done)
		go func() { mux.ServeHTTP(rec, req); close(done) }()
		waitForBody(t, rec, "event: live")
	}
	defer func() {
		for i, cancel := range cancels {
			cancel()
			<-dones[i]
		}
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 once at streamMaxSubscribers", rec.Code)
	}
}

// TestLivePayloadHasNoDBOnlyFieldsAtTopLevel is the design's explicit
// requirement: the stream must never carry DB-only fields like
// status/state/events/peers at the TOP level (down/up/detail/jobs itself).
// Since #258 a job entry legitimately carries the whole jobDTO, DB-owned
// fields included — that's the point of streaming whole objects — so this
// only asserts about the payload's own top-level keys, not jobDTO's shape.
// Covers throughputPayload too (issue #265's new independent event) — the
// same requirement applies regardless of which event a field rides on.
func TestLivePayloadHasNoDBOnlyFieldsAtTopLevel(t *testing.T) {
	live := []core.RemoteTransfer{{Username: "alice", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 10, Speed: 5, QueuePosition: 2}}
	payload := buildLiveSnapshot(live, 1, core.ThroughputSeries{}, core.JobView{}, false, core.JobDetail{}, false, newLiveTransferIndex(live), nil, testFailedRetryAfter, testMaxCandidates, testNow)
	payload.Jobs = []jobDTO{}

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
			t.Errorf("live payload top level must not contain %q (DB-only field)", forbidden)
		}
	}

	tp := throughputPayload{Download: []throughputSampleDTO{{At: "2026-01-01T00:00:00Z", BytesPerSecond: 100, ActiveTransfers: 1}}}
	tpBody, err := json.Marshal(tp)
	if err != nil {
		t.Fatalf("marshal throughput: %v", err)
	}
	var tpRaw map[string]json.RawMessage
	if err := json.Unmarshal(tpBody, &tpRaw); err != nil {
		t.Fatalf("unmarshal throughput: %v", err)
	}
	for _, forbidden := range []string{"status", "state", "events", "peers"} {
		if _, ok := tpRaw[forbidden]; ok {
			t.Errorf("throughput payload top level must not contain %q (DB-only field)", forbidden)
		}
	}
}

// --- ?search= scope (issue #58) -------------------------------------------

// TestStreamEndpointInvalidSearchParamReturns400 covers ?search= strict
// parsing: anything that isn't 32 lowercase hex characters is a 400, matching
// ?job=/?jobs=/?throughput='s own intolerance of malformed input.
func TestStreamEndpointInvalidSearchParamReturns400(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	for _, raw := range []string{"not-hex", "ABCDEF00ABCDEF00ABCDEF00ABCDEF00", "a1b2c3", "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4x"} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/stream?search="+raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

const testSearchID = "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"

// TestStreamEndpointSearchScopeSendsInitialDeltaThenTickUpdates covers the
// happy path end to end: the initial frame carries the full delta from
// since=0, and a later tick with a fresh delta advances it.
func TestStreamEndpointSearchScopeSendsInitialDeltaThenTickUpdates(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	var mu sync.Mutex
	seq := 1
	deps.SearchDelta = func(id string, since int) (core.SearchDelta, bool) {
		if id != testSearchID {
			return core.SearchDelta{}, false
		}
		mu.Lock()
		defer mu.Unlock()
		if since >= seq {
			return core.SearchDelta{ID: id, Seq: seq, Done: false, Streaming: true}, true
		}
		return core.SearchDelta{
			ID: id, Seq: seq, Streaming: true,
			Groups: []core.SearchGroup{{ID: "g1", Peer: "peer1", Version: seq}},
		}, true
	}
	mux := newStreamTestServer(deps, 10*time.Millisecond, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/stream?search="+testSearchID, nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, `"peer":"peer1"`)

	mu.Lock()
	seq = 2
	mu.Unlock()

	waitForBody(t, rec, `"seq":2`)
	cancel()
	<-done
}

// TestStreamEndpointSearchUnknownIDSendsExpiredFrame covers a well-formed but
// unresolved id (evicted between the POST and this connection, or a
// reconnect after eviction): exactly one `expired: true` frame, never a 400.
func TestStreamEndpointSearchUnknownIDSendsExpiredFrame(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.SearchDelta = func(id string, since int) (core.SearchDelta, bool) {
		return core.SearchDelta{}, false
	}
	mux := newStreamTestServer(deps, 10*time.Millisecond, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream?search="+testSearchID, nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, `"expired":true`)
	time.Sleep(30 * time.Millisecond) // let a few more ticks pass
	cancel()
	<-done

	if n := strings.Count(rec.String(), `"expired":true`); n != 1 {
		t.Errorf("expired frame sent %d times, want exactly 1", n)
	}
}

// TestStreamEndpointNoSearchParamNeverSendsSearchEvent is the negative half:
// without ?search=, no `event: search` line ever appears.
func TestStreamEndpointNoSearchParamNeverSendsSearchEvent(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.SearchDelta = func(id string, since int) (core.SearchDelta, bool) {
		return core.SearchDelta{ID: id, Seq: 1, Groups: []core.SearchGroup{{ID: "g1"}}}, true
	}
	mux := newStreamTestServer(deps, time.Hour, time.Hour, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil).WithContext(ctx)
	rec := newTestStreamRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	waitForBody(t, rec, "event: live")
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if strings.Contains(rec.String(), "event: search") {
		t.Errorf("no ?search=: expected no search event, got %q", rec.String())
	}
}

// TestStreamHubSearchScopedSubscriberStillGetsInvalidateAndThroughput guards
// tick's block ORDER. The `event: search` block ends every iteration in a
// `continue`, so it must stay LAST in tick's per-subscriber loop.
//
// The uncovered case this pins is specifically a search-scoped subscriber on a
// tick where its SEARCH HAS NOTHING NEW — the `len(delta.Groups) == 0 && ...`
// early `continue`. That is the ordinary steady state of a live search (the
// hub ticks at 1Hz, results do not arrive every tick), and if the search block
// were hoisted above the invalidate/throughput blocks those subscribers would
// silently stop receiving either event for the rest of the connection. #275's
// own tests only cover subscribers with NO ?search= scope, so they do not
// reach this path. This branch was rebased over #275 by hand, so this is the
// guard against the next such merge.
//
// Table-driven over wantThroughput because the two events sit on opposite
// sides of a guard the search block must not be hoisted past either.
func TestStreamHubSearchScopedSubscriberStillGetsInvalidateAndThroughput(t *testing.T) {
	tests := []struct {
		name           string
		wantThroughput bool
	}{
		{name: "search scope with throughput", wantThroughput: true},
		{name: "search scope without throughput", wantThroughput: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobsFn, setTitle, _ := newInvalidateFixture()
			var tmu sync.Mutex
			sample := core.ThroughputSample{At: time.Now(), BytesPerSecond: 1024}
			throughputFn := func(context.Context) (core.ThroughputSeries, error) {
				tmu.Lock()
				defer tmu.Unlock()
				return core.ThroughputSeries{Download: []core.ThroughputSample{sample}}, nil
			}
			// Deliberately unchanging: after subscribe's initial frame the
			// search block always takes its "nothing to send" continue.
			searchDeltaFn := func(id string, since int) (core.SearchDelta, bool) {
				delta := core.SearchDelta{ID: id, Seq: 1, Streaming: true}
				if since < 1 {
					delta.Groups = []core.SearchGroup{{ID: "g1", Peer: "p1", Version: 1}}
				}
				return delta, true
			}
			hub := newStreamHub(jobsFn, noopLiveTransfers, throughputFn, noopTransferBytes, nil, searchDeltaFn, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Millisecond)
			id, _, tch, ich, sch, _, _, _ := hub.subscribe(context.Background(), 0, nil, tt.wantThroughput, testSearchID)
			defer hub.unsubscribe(id)

			// Drain whatever subscribe already delivered, so every assertion
			// below is about what tick produced.
			select {
			case <-tch:
			default:
			}
			select {
			case <-sch:
			default:
			}

			setTitle("B")
			hub.correlationTick(context.Background())
			time.Sleep(2 * time.Millisecond)
			// A fresh sample, newer than the one subscribe already watermarked.
			tmu.Lock()
			sample = core.ThroughputSample{At: time.Now(), BytesPerSecond: 2048}
			tmu.Unlock()
			hub.tick(context.Background())

			select {
			case got := <-ich:
				if got.Generation != 1 {
					t.Errorf("Generation = %d, want 1", got.Generation)
				}
			default:
				t.Fatal("a ?search=-scoped subscriber must still receive an invalidate frame; tick's search block must stay after the invalidate block")
			}

			select {
			case got := <-tch:
				if !tt.wantThroughput {
					t.Fatalf("wantThroughput=false subscriber received a throughput frame: %+v", got)
				}
				if len(got.Download) == 0 {
					t.Errorf("throughput frame carried no download samples: %+v", got)
				}
			default:
				if tt.wantThroughput {
					t.Fatal("a ?search=-scoped subscriber must still receive a throughput frame; tick's search block must stay after the throughput block")
				}
			}

			// Sanity check on the fixture itself: the assertions above are only
			// meaningful if the search block really did take its early
			// `continue` this tick, i.e. produced no frame of its own.
			select {
			case got := <-sch:
				t.Fatalf("fixture produced a search frame; this test must exercise the no-change continue path, got %+v", got)
			default:
			}
		})
	}
}

// TestStreamHubSearchTruncatedFlipReachesSubscriberWithoutDone pins the
// failure the "is there anything to send" predicate used to have: once
// app.searchMaxResults is reached every later result is dropped, so no group
// version changes and the session's Seq freezes. A delta whose ONLY change is
// Truncated flipping to true therefore produced no frame at all until Done
// flipped — on a broad query that saturates in the first seconds of a 60s
// search, the UI showed a frozen count and no "showing the first N" notice for
// the rest of the run.
func TestStreamHubSearchTruncatedFlipReachesSubscriberWithoutDone(t *testing.T) {
	var mu sync.Mutex
	truncated := false
	searchDeltaFn := func(id string, since int) (core.SearchDelta, bool) {
		mu.Lock()
		defer mu.Unlock()
		delta := core.SearchDelta{ID: id, Seq: 1, Total: 2000, Streaming: true, Truncated: truncated}
		if since < 1 {
			delta.Groups = []core.SearchGroup{{ID: "g1", Peer: "p1", Version: 1}}
		}
		return delta, true
	}
	hub := newStreamHub(noopJobs, noopLiveTransfers, noopThroughput, noopTransferBytes, nil, searchDeltaFn, testFailedRetryAfter, testMaxCandidates, time.Hour, time.Hour, time.Hour)
	id, _, _, _, sch, _, _, initialSearch := hub.subscribe(context.Background(), 0, nil, false, testSearchID)
	defer hub.unsubscribe(id)

	if initialSearch.Truncated {
		t.Fatalf("initial frame already truncated, fixture is wrong: %+v", initialSearch)
	}

	// Nothing changed: no frame.
	hub.tick(context.Background())
	select {
	case got := <-sch:
		t.Fatalf("unchanged delta produced a frame: %+v", got)
	default:
	}

	// Seq and Groups are unchanged — only Truncated flips. Done stays false.
	mu.Lock()
	truncated = true
	mu.Unlock()
	hub.tick(context.Background())
	select {
	case got := <-sch:
		if !got.Truncated {
			t.Fatalf("frame does not carry Truncated: %+v", got)
		}
		if got.Done {
			t.Fatalf("Done flipped too; this test must exercise Truncated alone: %+v", got)
		}
	default:
		t.Fatal("the truncated flip produced no frame; a saturated search would show a frozen count with no notice until Done")
	}

	// And it is not resent every tick once the subscriber knows.
	hub.tick(context.Background())
	select {
	case got := <-sch:
		t.Fatalf("truncated frame resent after the subscriber already had it: %+v", got)
	default:
	}
}

// TestMergeSearchGroupsNeverDropsGroups pins the decision to leave the union
// UNCAPPED. It used to truncate at 500 entries of a slice sorted by group id
// (a sha256 prefix), so an arbitrary subset vanished — and because the
// subscriber's searchSeq cursor had already advanced past those versions and
// the frontend does not poll REST during a live search, they were never
// resent. The union is keyed by group id and app.searchMaxResults bounds a
// session at 2000 files (hence at most 2000 groups), so it is already bounded
// without a cap.
func TestMergeSearchGroupsNeverDropsGroups(t *testing.T) {
	const total = 1200
	var old, next []searchGroupDTO
	for i := range total {
		g := searchGroupDTO{ID: fmt.Sprintf("%04x", i), Peer: "p1"}
		if i%2 == 0 {
			old = append(old, g)
		} else {
			next = append(next, g)
		}
	}

	merged := mergeSearchGroups(old, next)
	if len(merged) != total {
		t.Fatalf("merged %d groups, want all %d — a cap here drops groups permanently", len(merged), total)
	}
	seen := make(map[string]struct{}, len(merged))
	for _, g := range merged {
		seen[g.ID] = struct{}{}
	}
	for i := range total {
		if _, ok := seen[fmt.Sprintf("%04x", i)]; !ok {
			t.Fatalf("group %04x was dropped by the merge", i)
		}
	}
	if !sort.SliceIsSorted(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID }) {
		t.Error("merged groups are not sorted by id")
	}
}

// TestSendLatestSearchAccumulatesAcrossDisplacedFrame is the direct analogue
// of TestSendLatestThroughputAccumulatesAcrossDisplacedFrame: a search delta
// is append-only, so a displaced frame's groups must be unioned into the
// next send, never discarded — the frontend does not poll REST during a live
// search, so a dropped frame here would leave a permanent hole.
func TestSendLatestSearchAccumulatesAcrossDisplacedFrame(t *testing.T) {
	sch := make(chan searchPayload, 1)

	g1 := searchGroupDTO{ID: "g1", Peer: "p1"}
	first := searchPayload{ID: testSearchID, Seq: 1, Groups: []searchGroupDTO{g1}, Total: 1}
	if got := sendLatestSearch(sch, first); len(got.Groups) != 1 {
		t.Fatalf("first send: got %+v", got)
	}

	// Sent WITHOUT reading the channel first — first is still queued.
	g2 := searchGroupDTO{ID: "g2", Peer: "p2"}
	second := searchPayload{ID: testSearchID, Seq: 2, Groups: []searchGroupDTO{g2}, Total: 2, Done: true, Truncated: true}
	merged := sendLatestSearch(sch, second)

	if len(merged.Groups) != 2 {
		t.Fatalf("merged Groups = %+v, want both g1 and g2 unioned", merged.Groups)
	}
	byID := map[string]searchGroupDTO{}
	for _, g := range merged.Groups {
		byID[g.ID] = g
	}
	if _, ok := byID["g1"]; !ok {
		t.Error("g1 lost across the displaced frame")
	}
	if _, ok := byID["g2"]; !ok {
		t.Error("g2 missing from the newer frame")
	}
	if merged.Seq != 2 {
		t.Errorf("Seq = %d, want the newer (higher) value 2", merged.Seq)
	}
	if !merged.Done || !merged.Truncated {
		t.Errorf("Done/Truncated = %v/%v, want both true (OR'd forward)", merged.Done, merged.Truncated)
	}

	queued := <-sch
	if len(queued.Groups) != 2 {
		t.Errorf("queued Groups = %+v, want both unioned", queued.Groups)
	}
}

// TestSendLatestSearchUpdatingSameGroupKeepsNewerVersion verifies the "newer
// wins per group id" half of the union: when the same group changes again
// before the displaced frame is read, the fresher copy survives, not both.
func TestSendLatestSearchUpdatingSameGroupKeepsNewerVersion(t *testing.T) {
	sch := make(chan searchPayload, 1)

	stale := searchGroupDTO{ID: "g1", Peer: "p1", TrackCount: 1}
	sendLatestSearch(sch, searchPayload{ID: testSearchID, Seq: 1, Groups: []searchGroupDTO{stale}})

	fresh := searchGroupDTO{ID: "g1", Peer: "p1", TrackCount: 2}
	merged := sendLatestSearch(sch, searchPayload{ID: testSearchID, Seq: 2, Groups: []searchGroupDTO{fresh}})

	if len(merged.Groups) != 1 {
		t.Fatalf("merged Groups = %+v, want exactly one entry for g1", merged.Groups)
	}
	if merged.Groups[0].TrackCount != 2 {
		t.Errorf("TrackCount = %d, want 2 (the newer version wins)", merged.Groups[0].TrackCount)
	}
}
