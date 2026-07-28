package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// detailWithTransfers builds a one-attempt JobDetailFunc whose single attempt
// (peer "peer_one") carries the given transfers, for the live-enrichment tests.
func detailWithTransfers(transfers []core.Transfer) JobDetailFunc {
	return func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{
			Job: core.AlbumJob{ID: jobID, Title: "Rounds", ArtistName: "Four Tet", State: core.StateDownloading},
			Attempts: []core.AttemptDetail{
				{
					Attempt:   core.Candidate{ID: 1, Username: "peer_one", State: core.CandidateActive},
					Transfers: transfers,
				},
			},
		}, true, nil
	}
}

// jobViewWithID builds a JobViewFunc that reports found=true for exactly the
// given job id, echoing it into a minimal core.JobView — enough for tests
// that only care about toJobDetailDTO's Attempts, not its embedded jobDTO
// header (issue #268 requires deps.JobView to resolve before the detail
// handler builds a body at all).
func jobViewWithID(jobID int64) JobViewFunc {
	return func(ctx context.Context, id int64) (core.JobView, bool, error) {
		if id != jobID {
			return core.JobView{}, false, nil
		}
		return core.JobView{Job: core.AlbumJob{ID: id}}, true, nil
	}
}

// decodeTransferMaps returns each transfer of the single attempt as a raw key
// map, so a test can assert whether an omitempty field is present at all rather
// than only its value.
func decodeTransferMaps(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var raw struct {
		Attempts []struct {
			Transfers []map[string]json.RawMessage `json:"transfers"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(raw.Attempts))
	}
	return raw.Attempts[0].Transfers
}

// A transfer's live queue position and speed are joined in from ListDownloads:
// by remote id where the store has one, else by username+filename. omitempty
// means an actively-downloading transfer (queue position 0) omits the queue
// key, a queued one (speed 0) omits the speed key, and a terminal one with no
// live entry omits both.
func TestJobDetailEnrichesTransfersWithLiveQueueAndSpeed(t *testing.T) {
	reg := prometheus.NewRegistry()

	jobDetail := detailWithTransfers([]core.Transfer{
		// Matched by remote id; actively downloading (speed set, queue 0).
		{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 50, BytesTotal: 100},
		// No remote id yet; matched by username+filename; queued (queue set, speed 0).
		{Username: "peer_one", Filename: "02.flac", State: core.TransferPending},
		// Terminal, no live entry: stays unenriched.
		{SlskdID: "g3", Username: "peer_one", Filename: "03.flac", State: core.TransferErrored},
	})
	live := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return []core.RemoteTransfer{
			{ID: "g1", Username: "peer_one", Filename: "01.flac", Speed: 524288},
			{ID: "g2", Username: "peer_one", Filename: "02.flac", QueuePosition: 5},
		}, nil
	}
	deps := testServerDeps(reg)
	deps.JobDetail = jobDetail
	deps.JobView = jobViewWithID(7)
	deps.LiveTransfers = live
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/7/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}

	transfers := decodeTransferMaps(t, rec.Body.Bytes())
	if len(transfers) != 3 {
		t.Fatalf("expected 3 transfers, got %d", len(transfers))
	}

	// 01.flac: speed present (524288), queuePosition omitted (0).
	if got := string(transfers[0]["speed"]); got != "524288" {
		t.Errorf("01.flac speed = %q, want 524288", got)
	}
	if _, ok := transfers[0]["queuePosition"]; ok {
		t.Errorf("01.flac should omit queuePosition (0), got %q", transfers[0]["queuePosition"])
	}

	// 02.flac: queuePosition present (5, via username+filename fallback), speed omitted.
	if got := string(transfers[1]["queuePosition"]); got != "5" {
		t.Errorf("02.flac queuePosition = %q, want 5", got)
	}
	if _, ok := transfers[1]["speed"]; ok {
		t.Errorf("02.flac should omit speed (0), got %q", transfers[1]["speed"])
	}

	// 03.flac: no live entry, both omitted.
	if _, ok := transfers[2]["speed"]; ok {
		t.Errorf("03.flac should omit speed, got %q", transfers[2]["speed"])
	}
	if _, ok := transfers[2]["queuePosition"]; ok {
		t.Errorf("03.flac should omit queuePosition, got %q", transfers[2]["queuePosition"])
	}
}

// Live enrichment is best-effort: a failing ListDownloads must not fail the
// whole detail request, only drop the queue/speed columns.
func TestJobDetailStillServedWhenLiveTransfersError(t *testing.T) {
	reg := prometheus.NewRegistry()

	jobDetail := detailWithTransfers([]core.Transfer{
		{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress},
	})
	live := func(ctx context.Context) ([]core.RemoteTransfer, error) {
		return nil, context.DeadlineExceeded
	}
	deps := testServerDeps(reg)
	deps.JobDetail = jobDetail
	deps.JobView = jobViewWithID(7)
	deps.LiveTransfers = live
	h := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/7/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	transfers := decodeTransferMaps(t, rec.Body.Bytes())
	if len(transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(transfers))
	}
	if _, ok := transfers[0]["speed"]; ok {
		t.Errorf("speed should be absent when live fetch failed, got %q", transfers[0]["speed"])
	}
}

// TestToJobDetailDTOOverlaysBytesDoneForLiveMatch is issue #161, part 1c: a
// matched live transfer's BytesDone must win over the persisted value.
func TestToJobDetailDTOOverlaysBytesDoneForLiveMatch(t *testing.T) {
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 1, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "peer_one"},
			Transfers: []core.Transfer{
				{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 100, BytesTotal: 1000},
			},
		}},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{ID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 750},
	})

	dto := toJobDetailDTO(core.JobView{Job: detail.Job}, detail, live, nil, testFailedRetryAfter, testMaxCandidates)
	tr := dto.Attempts[0].Transfers[0]
	if tr.BytesDone != 750 {
		t.Errorf("BytesDone = %d, want 750 (live overlay)", tr.BytesDone)
	}
	if tr.BytesTotal != 1000 {
		t.Errorf("BytesTotal = %d, want 1000 (never overlaid)", tr.BytesTotal)
	}
}

// TestToJobDetailDTOFallsBackToPersistedWithoutLiveMatch covers a transfer
// with no live counterpart at all (e.g. terminal and already reconciled away
// from ListDownloads): the persisted BytesDone must be served unmodified.
func TestToJobDetailDTOFallsBackToPersistedWithoutLiveMatch(t *testing.T) {
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 1, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "peer_one"},
			Transfers: []core.Transfer{
				{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000, BytesTotal: 1000},
			},
		}},
	}

	dto := toJobDetailDTO(core.JobView{Job: detail.Job}, detail, liveTransferIndex{}, nil, testFailedRetryAfter, testMaxCandidates)
	tr := dto.Attempts[0].Transfers[0]
	if tr.BytesDone != 1000 {
		t.Errorf("BytesDone = %d, want 1000 (persisted, no live match)", tr.BytesDone)
	}
}

// TestToJobDetailDTOTerminalLiveMatchOverwritesPersisted is issue #161's
// fix: a lingering terminal live entry (errored/cancelled/completed, not yet
// reconciled away from ListDownloads) still supplies bytes — see
// transferDetailDTO.BytesDone's comment on why a state-agnostic match is
// what's accurate here (the live entry's byte count is only ever purged in
// the same reconcile pass that also persists it, per
// internal/pipeline/downloading.go's reconcile).
func TestToJobDetailDTOTerminalLiveMatchOverwritesPersisted(t *testing.T) {
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 1, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "peer_one"},
			Transfers: []core.Transfer{
				{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 800, BytesTotal: 1000},
			},
		}},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		// Lingering terminal entry, not yet reconciled away — its BytesDone is
		// the file's final, accurate size.
		{ID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000},
	})

	dto := toJobDetailDTO(core.JobView{Job: detail.Job}, detail, live, nil, testFailedRetryAfter, testMaxCandidates)
	tr := dto.Attempts[0].Transfers[0]
	if tr.BytesDone != 1000 {
		t.Errorf("BytesDone = %d, want 1000 (terminal live match still supplies bytes)", tr.BytesDone)
	}
}

// TestToJobDetailDTOPersistedTerminalWinsOverLingeringLiveEntry is #259's
// original regression, restored: reconcile persists the terminal state
// before purging the transfer from the live backend, but that purge
// (removeFromSlskd) is best-effort and swallows non-404 errors, so a finished
// file can still match a lingering live entry that keeps reporting
// IN_PROGRESS at a stale speed. When both sides disagree on terminal-ness,
// the monotone rule (toJobDetailDTO) prefers the PERSISTED terminal state —
// reconcile is the authority on the actual outcome — and drops speed/queue
// regardless, since the transfer is terminal either way.
func TestToJobDetailDTOPersistedTerminalWinsOverLingeringLiveEntry(t *testing.T) {
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 1, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "peer_one"},
			Transfers: []core.Transfer{
				{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000, BytesTotal: 1000},
			},
		}},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{ID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 1000, Speed: 1300000, QueuePosition: 182},
	})

	tr := toJobDetailDTO(core.JobView{Job: detail.Job}, detail, live, nil, testFailedRetryAfter, testMaxCandidates).Attempts[0].Transfers[0]
	if tr.State != string(core.TransferCompleted) {
		t.Errorf("State = %q, want %q (persisted terminal wins over a lingering live entry)", tr.State, core.TransferCompleted)
	}
	if tr.Speed != 0 {
		t.Errorf("Speed = %d, want 0 (persisted state is terminal)", tr.Speed)
	}
	if tr.QueuePosition != 0 {
		t.Errorf("QueuePosition = %d, want 0 (persisted state is terminal)", tr.QueuePosition)
	}
	if tr.BytesDone != 1000 {
		t.Errorf("BytesDone = %d, want 1000 (bytes come from live in every state)", tr.BytesDone)
	}
}

// TestToJobDetailDTOTerminalLiveMatchDropsSpeedAndQueue is the reverse
// direction the monotone rule also covers (issue #258's 18:15 comment): the
// native client sets a transfer's terminal state in memory the instant it
// finishes, while Postgres only catches up on the next Downloading reconcile
// (default 15s) — so a lingering PERSISTED non-terminal row can equally be
// the stale one. Here live alone is terminal, so it wins and speed/queue are
// still dropped.
func TestToJobDetailDTOTerminalLiveMatchDropsSpeedAndQueue(t *testing.T) {
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 1, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "peer_one"},
			Transfers: []core.Transfer{
				{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 800, BytesTotal: 1000},
			},
		}},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{ID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferCompleted, BytesDone: 1000, Speed: 1300000, QueuePosition: 182},
	})

	tr := toJobDetailDTO(core.JobView{Job: detail.Job}, detail, live, nil, testFailedRetryAfter, testMaxCandidates).Attempts[0].Transfers[0]
	if tr.State != string(core.TransferCompleted) {
		t.Errorf("State = %q, want %q (live alone is terminal, so it wins)", tr.State, core.TransferCompleted)
	}
	if tr.Speed != 0 {
		t.Errorf("Speed = %d, want 0 (chosen/live state is terminal)", tr.Speed)
	}
	if tr.QueuePosition != 0 {
		t.Errorf("QueuePosition = %d, want 0 (chosen/live state is terminal)", tr.QueuePosition)
	}
}

// STALLED is a durable retry intent, not an end state: the transfer is still in
// flight and its live speed/queue position are real.
func TestToJobDetailDTOStalledKeepsLiveSpeedAndQueue(t *testing.T) {
	detail := core.JobDetail{
		Job: core.AlbumJob{ID: 1, Title: "Rounds", ArtistName: "Four Tet"},
		Attempts: []core.AttemptDetail{{
			Attempt: core.Candidate{ID: 1, Username: "peer_one"},
			Transfers: []core.Transfer{
				{SlskdID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferStalled, BytesDone: 400, BytesTotal: 1000},
			},
		}},
	}
	live := newLiveTransferIndex([]core.RemoteTransfer{
		{ID: "g1", Username: "peer_one", Filename: "01.flac", State: core.TransferInProgress, BytesDone: 600, Speed: 1500, QueuePosition: 3},
	})

	tr := toJobDetailDTO(core.JobView{Job: detail.Job}, detail, live, nil, testFailedRetryAfter, testMaxCandidates).Attempts[0].Transfers[0]
	if tr.Speed != 1500 || tr.QueuePosition != 3 {
		t.Errorf("Speed/QueuePosition = %d/%d, want 1500/3 (STALLED is not terminal)", tr.Speed, tr.QueuePosition)
	}
}

// match prefers the remote id and falls back to username+filename.
func TestLiveTransferIndexMatch(t *testing.T) {
	idx := newLiveTransferIndex([]core.RemoteTransfer{
		{ID: "g1", Username: "alice", Filename: "a.flac", Speed: 100},
		{ID: "g2", Username: "bob", Filename: "b.flac", QueuePosition: 3},
	})

	// By id.
	if lt, ok := idx.match(core.Transfer{SlskdID: "g1", Username: "alice", Filename: "a.flac"}); !ok || lt.Speed != 100 {
		t.Errorf("id match = %+v, ok=%v; want Speed 100", lt, ok)
	}
	// By username+filename when no remote id is stored yet.
	if lt, ok := idx.match(core.Transfer{Username: "bob", Filename: "b.flac"}); !ok || lt.QueuePosition != 3 {
		t.Errorf("fallback match = %+v, ok=%v; want QueuePosition 3", lt, ok)
	}
	// A stored id that is not live falls back to username+filename.
	if lt, ok := idx.match(core.Transfer{SlskdID: "stale", Username: "alice", Filename: "a.flac"}); !ok || lt.ID != "g1" {
		t.Errorf("stale-id fallback = %+v, ok=%v; want g1", lt, ok)
	}
	// No match at all.
	if _, ok := idx.match(core.Transfer{Username: "carol", Filename: "c.flac"}); ok {
		t.Error("expected no match for unknown transfer")
	}
}
