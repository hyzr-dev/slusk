package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// fakePeerSearcher is a PeerStreamSearcher test double: run implements the
// whole SearchStream contract (emit called synchronously, error returned on
// completion), and streaming backs SearchStreaming.
type fakePeerSearcher struct {
	streaming bool
	run       func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error
}

func (f *fakePeerSearcher) SearchStream(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
	return f.run(ctx, query, timeout, emit)
}

func (f *fakePeerSearcher) SearchStreaming() bool { return f.streaming }

// waitSearchDone polls Snapshot until the session is Done, failing the test
// if it doesn't finish within timeout.
func waitSearchDone(t *testing.T, s *Searches, id string, timeout time.Duration) core.SearchSession {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess, ok := s.Snapshot(id); ok && sess.Done {
			return sess
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("search did not finish in time")
	return core.SearchSession{}
}

// waitGroupCount polls Snapshot until it reports at least n groups.
func waitGroupCount(t *testing.T, s *Searches, id string, n int) core.SearchSession {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sess, ok := s.Snapshot(id); ok && len(sess.Groups) >= n {
			return sess
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not reach %d groups in time", id, n)
	return core.SearchSession{}
}

func TestSearchesStartValidatesQuery(t *testing.T) {
	s := NewSearches(SearchesParams{Peers: &fakePeerSearcher{}, Root: context.Background(), Timeout: time.Second})
	for _, q := range []string{"", "   ", string(make([]byte, searchQueryMaxLen+1))} {
		if _, err := s.Start(context.Background(), q); !errors.Is(err, ErrSearchQueryInvalid) {
			t.Fatalf("Start(%q) = %v, want ErrSearchQueryInvalid", q, err)
		}
	}
}

func TestSearchesStartReportsUnavailableWhenNoPeers(t *testing.T) {
	s := NewSearches(SearchesParams{Root: context.Background(), Timeout: time.Second})
	if _, err := s.Start(context.Background(), "q"); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("Start with nil Peers = %v, want ErrSearchUnavailable", err)
	}
}

func TestSearchesSnapshotAndDeltaReturnFalseForUnknownID(t *testing.T) {
	s := NewSearches(SearchesParams{Peers: &fakePeerSearcher{}, Root: context.Background(), Timeout: time.Second})
	if _, ok := s.Snapshot("deadbeefdeadbeefdeadbeefdeadbeef"); ok {
		t.Fatal("Snapshot reported true for unknown id")
	}
	if _, ok := s.Delta("deadbeefdeadbeefdeadbeefdeadbeef", 0); ok {
		t.Fatal("Delta reported true for unknown id")
	}
}

func TestSearchesStopReportsFalseForUnknownID(t *testing.T) {
	s := NewSearches(SearchesParams{Peers: &fakePeerSearcher{}, Root: context.Background(), Timeout: time.Second})
	if s.Stop("deadbeefdeadbeefdeadbeefdeadbeef") {
		t.Fatal("Stop reported true for unknown id")
	}
}

// TestSearchesStartGroupsByPeerAndReleaseDirWithScoreOrdering covers the
// core grouping contract: files land in one group per (username,
// matcher.ReleaseDir), never merged across peers or across release
// directories, and a lossless/reliable group scores higher than a lossy/
// unreliable one via the shared matcher primitives.
func TestSearchesStartGroupsByPeerAndReleaseDirWithScoreOrdering(t *testing.T) {
	peers := &fakePeerSearcher{streaming: true, run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		emit([]core.SearchResult{
			{Username: "alice", Filename: `Music\Radiohead\In Rainbows\01 - 15 Step.flac`, Size: 100, BitRate: 1000, HasFreeUploadSlot: true},
			{Username: "alice", Filename: `Music\Radiohead\In Rainbows\02 - Bodysnatchers.flac`, Size: 100, BitRate: 1000, HasFreeUploadSlot: true},
			{Username: "bob", Filename: `Shared\Other Album\01.mp3`, Size: 50, BitRate: 128},
		})
		return nil
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Second})
	sess, err := s.Start(context.Background(), "in rainbows")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitSearchDone(t, s, sess.ID, time.Second)
	if len(final.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per peer+releaseDir): %+v", len(final.Groups), final.Groups)
	}
	var aliceGroup, bobGroup *core.SearchGroup
	for i := range final.Groups {
		switch final.Groups[i].Peer {
		case "alice":
			aliceGroup = &final.Groups[i]
		case "bob":
			bobGroup = &final.Groups[i]
		}
	}
	if aliceGroup == nil || bobGroup == nil {
		t.Fatalf("expected one group per peer, got %+v", final.Groups)
	}
	if aliceGroup.TrackCount != 2 {
		t.Fatalf("alice track count = %d, want 2", aliceGroup.TrackCount)
	}
	if aliceGroup.Title != "In Rainbows" || aliceGroup.Parent != "Radiohead" {
		t.Fatalf("alice title/parent = %q/%q, want In Rainbows/Radiohead", aliceGroup.Title, aliceGroup.Parent)
	}
	if aliceGroup.Format != "flac" {
		t.Fatalf("alice format = %q, want flac", aliceGroup.Format)
	}
	if aliceGroup.Score <= bobGroup.Score {
		t.Fatalf("alice score %v should exceed bob score %v (lossless + free slot vs lossy)", aliceGroup.Score, bobGroup.Score)
	}
}

func TestSearchesResultCapSetsTruncated(t *testing.T) {
	peers := &fakePeerSearcher{streaming: true, run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		batch := make([]core.SearchResult, 0, searchMaxResults+5)
		for i := 0; i < searchMaxResults+5; i++ {
			batch = append(batch, core.SearchResult{Username: "u", Filename: fmt.Sprintf(`Dir\%d.flac`, i), Size: 1})
		}
		emit(batch)
		return nil
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Second})
	sess, err := s.Start(context.Background(), "many results")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitSearchDone(t, s, sess.ID, time.Second)
	if !final.Truncated {
		t.Fatal("expected Truncated once searchMaxResults is exceeded")
	}
	if final.Total != searchMaxResults {
		t.Fatalf("Total = %d, want %d (capped)", final.Total, searchMaxResults)
	}
}

// TestSearchesStartReturnsErrSearchBusyAtSessionCap verifies the cap counts
// only LIVE (not-yet-finished) sessions.
func TestSearchesStartReturnsErrSearchBusyAtSessionCap(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	peers := &fakePeerSearcher{run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return ctx.Err()
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Minute})
	for i := 0; i < searchMaxSessions; i++ {
		if _, err := s.Start(context.Background(), fmt.Sprintf("q%d", i)); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	if _, err := s.Start(context.Background(), "overflow"); !errors.Is(err, ErrSearchBusy) {
		t.Fatalf("Start at cap = %v, want ErrSearchBusy", err)
	}
}

func TestSearchesTTLEvictsFinishedSessionsFromSnapshot(t *testing.T) {
	peers := &fakePeerSearcher{run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		return nil
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Second})
	sess, err := s.Start(context.Background(), "q")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitSearchDone(t, s, sess.ID, time.Second)

	s.mu.Lock()
	internal := s.sessions[sess.ID]
	s.mu.Unlock()
	internal.mu.Lock()
	internal.finishedAt = time.Now().Add(-searchSessionTTL - time.Second)
	internal.mu.Unlock()

	if _, ok := s.Snapshot(sess.ID); ok {
		t.Fatal("expected a TTL-expired session to be evicted by Snapshot's lazy sweep")
	}
}

// TestSearchesDeltaCursorSemantics verifies nothing is ever returned twice
// and nothing is ever skipped as a session accumulates groups over multiple
// emit calls.
func TestSearchesDeltaCursorSemantics(t *testing.T) {
	started := make(chan func([]core.SearchResult), 1)
	release := make(chan struct{})
	peers := &fakePeerSearcher{streaming: true, run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		started <- emit
		<-release
		return nil
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Minute})
	sess, err := s.Start(context.Background(), "q")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	emit := <-started

	emit([]core.SearchResult{{Username: "u1", Filename: `A\01.flac`, Size: 1}})
	waitGroupCount(t, s, sess.ID, 1)
	delta1, ok := s.Delta(sess.ID, 0)
	if !ok || len(delta1.Groups) != 1 {
		t.Fatalf("delta1 = %+v, ok=%v, want exactly 1 group", delta1, ok)
	}

	emit([]core.SearchResult{{Username: "u2", Filename: `B\01.flac`, Size: 1}})
	waitGroupCount(t, s, sess.ID, 2)
	delta2, ok := s.Delta(sess.ID, delta1.Seq)
	if !ok || len(delta2.Groups) != 1 || delta2.Groups[0].Peer != "u2" {
		t.Fatalf("delta2 = %+v, ok=%v, want exactly the new group (u2), nothing repeated or skipped", delta2, ok)
	}

	delta3, ok := s.Delta(sess.ID, delta2.Seq)
	if !ok || len(delta3.Groups) != 0 {
		t.Fatalf("delta3 = %+v, ok=%v, want no groups (nothing changed since delta2.Seq)", delta3, ok)
	}

	close(release)
	waitSearchDone(t, s, sess.ID, time.Second)
}

func TestSearchesRetainsPartialResultsAndErrOnBackendFailure(t *testing.T) {
	wantErr := errors.New("connection lost")
	peers := &fakePeerSearcher{run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		emit([]core.SearchResult{{Username: "u", Filename: `A\01.flac`, Size: 1}})
		return wantErr
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Second})
	sess, err := s.Start(context.Background(), "q")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitSearchDone(t, s, sess.ID, time.Second)
	if final.Err != wantErr.Error() {
		t.Fatalf("Err = %q, want %q", final.Err, wantErr.Error())
	}
	if len(final.Groups) != 1 {
		t.Fatalf("expected partial results retained on failure, got %+v", final.Groups)
	}
}

func TestSearchesStopCancelsRunningSessionAndFreesSlot(t *testing.T) {
	started := make(chan struct{})
	peers := &fakePeerSearcher{run: func(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	s := NewSearches(SearchesParams{Peers: peers, Root: context.Background(), Timeout: time.Minute})
	sess, err := s.Start(context.Background(), "q")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started
	if !s.Stop(sess.ID) {
		t.Fatal("Stop reported not found for a live session")
	}
	final := waitSearchDone(t, s, sess.ID, time.Second)
	if final.Err == "" {
		t.Fatal("expected an error recorded once Stop cancelled the session")
	}

	s.mu.Lock()
	live := s.liveCountLocked()
	s.mu.Unlock()
	if live != 0 {
		t.Fatalf("live session count = %d, want 0 after Stop frees the slot", live)
	}
}
