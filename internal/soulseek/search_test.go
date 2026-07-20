package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

type searchServerConn struct {
	mu      sync.Mutex
	writes  [][]byte
	writeFn func([]byte) (int, error)
	closed  chan struct{}
	once    sync.Once
}

func newSearchServerConn() *searchServerConn {
	return &searchServerConn{closed: make(chan struct{})}
}

func (c *searchServerConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *searchServerConn) Write(p []byte) (int, error) {
	copyOfP := append([]byte(nil), p...)
	c.mu.Lock()
	c.writes = append(c.writes, copyOfP)
	fn := c.writeFn
	c.mu.Unlock()
	if fn != nil {
		return fn(copyOfP)
	}
	return len(p), nil
}

func (c *searchServerConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (*searchServerConn) LocalAddr() net.Addr              { return dummyAddr("search-local") }
func (*searchServerConn) RemoteAddr() net.Addr             { return dummyAddr("search-remote") }
func (*searchServerConn) SetDeadline(time.Time) error      { return nil }
func (*searchServerConn) SetReadDeadline(time.Time) error  { return nil }
func (*searchServerConn) SetWriteDeadline(time.Time) error { return nil }

func startSearchClient(t *testing.T) (*Client, *searchServerConn, uint64) {
	t.Helper()
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)
	conn := newSearchServerConn()
	const generation = 1
	c.mu.Lock()
	c.serverConn = conn
	c.serverGeneration = generation
	c.serverCancel = func() {}
	c.mu.Unlock()
	t.Cleanup(func() {
		c.searches.failAll(context.Canceled)
		_ = conn.Close()
	})
	return c, conn, generation
}

func parseSearchRequest(frame []byte) (soul.Token, string, error) {
	if len(frame) < 12 {
		return 0, "", io.ErrUnexpectedEOF
	}
	declared := binary.LittleEndian.Uint32(frame[:4])
	if int(declared)+4 != len(frame) {
		return 0, "", fmt.Errorf("declared size %d, frame length %d", declared, len(frame))
	}
	if code := binary.LittleEndian.Uint32(frame[4:8]); code != uint32(server.CodeFileSearch) {
		return 0, "", fmt.Errorf("code %d", code)
	}
	token := soul.Token(binary.LittleEndian.Uint32(frame[8:12]))
	reader := bytes.NewReader(frame[12:])
	var size uint32
	if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
		return 0, "", err
	}
	query := make([]byte, size)
	if _, err := io.ReadFull(reader, query); err != nil {
		return 0, "", err
	}
	if reader.Len() != 0 {
		return 0, "", errors.New("trailing search request bytes")
	}
	return token, string(query), nil
}

func waitSearchRequest(t *testing.T, requests <-chan []byte) (soul.Token, string) {
	t.Helper()
	select {
	case frame := <-requests:
		token, query, err := parseSearchRequest(frame)
		if err != nil {
			t.Fatalf("parse search request: %v", err)
		}
		return token, query
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for search request")
		return 0, ""
	}
}

func makeSearchResponse(token soul.Token, username string, files []peer.File) *peer.FileSearchResponse {
	return &peer.FileSearchResponse{
		Username: username, Token: token, Results: files,
		FreeSlot: true, AverageSpeed: 1234, Queue: 7,
	}
}

func writePeerResponse(t *testing.T, conn net.Conn, response *peer.FileSearchResponse) {
	t.Helper()
	if _, err := peer.Write(conn, response, false); err != nil {
		t.Fatalf("write peer response: %v", err)
	}
}

func registerTestPSession(t *testing.T, c *Client, username string) (*peerSession, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	session := c.newSession(local, sessionKey{username: username, connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	if got := c.registerSession(session); got != session {
		t.Fatal("P session did not win registration")
	}
	t.Cleanup(func() {
		session.Close(errors.New("test complete"))
		_ = remote.Close()
	})
	return session, remote
}

func TestSearchRegistersBeforeWireAndStreamsPeerResponses(t *testing.T) {
	c, conn, _ := startSearchClient(t)
	requests := make(chan []byte, 1)
	activeBeforeWrite := make(chan bool, 1)
	conn.writeFn = func(frame []byte) (int, error) {
		token, _, err := parseSearchRequest(frame)
		if err != nil {
			return 0, err
		}
		activeBeforeWrite <- c.searches.subscription(token) != nil
		requests <- frame
		return len(frame), nil
	}

	_, remote := registerTestPSession(t, c, "claimed-peer")
	resultCh := make(chan []core.SearchResult, 1)
	errCh := make(chan error, 1)
	go func() {
		results, err := c.Search(context.Background(), "artist album", 100*time.Millisecond)
		resultCh <- results
		errCh <- err
	}()

	token, query := waitSearchRequest(t, requests)
	if query != "artist album" {
		t.Fatalf("query = %q", query)
	}
	if active := <-activeBeforeWrite; !active {
		t.Fatal("search token was not active before the code-26 write")
	}

	writePeerResponse(t, remote, makeSearchResponse(token, "claimed-peer", []peer.File{{
		Name: "Album\\01.flac", Size: 111, Extension: "flac",
		Attributes: []peer.Attribute{{Code: peer.Bitrate, Value: 900}},
	}}))
	writePeerResponse(t, remote, &peer.FileSearchResponse{
		Username: "claimed-peer", Token: token,
		Results:        []peer.File{{Name: "Album\\02.flac", Size: 222, Extension: "flac"}},
		PrivateResults: []peer.File{{Name: "Private\\secret.flac", Size: 333, Extension: "flac"}},
		FreeSlot:       false, AverageSpeed: 4321, Queue: 9,
	})

	results := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want two public files: %+v", len(results), results)
	}
	if got := results[0]; got.Username != "claimed-peer" || got.Filename != "Album\\01.flac" || got.Size != 111 || got.BitRate != 900 || !got.HasFreeUploadSlot || got.UploadSpeed != 1234 || got.QueueLength != 7 {
		t.Fatalf("first result mapping = %+v", got)
	}
	if got := results[1]; got.Username != "claimed-peer" || got.BitRate != 0 || got.HasFreeUploadSlot || got.UploadSpeed != 4321 || got.QueueLength != 9 {
		t.Fatalf("second result mapping = %+v", got)
	}
	assertNoActiveSearches(t, c)
}

func TestSearchRejectsFileResponseIdentityMismatch(t *testing.T) {
	c, conn, _ := startSearchClient(t)
	requests := make(chan []byte, 1)
	conn.writeFn = func(frame []byte) (int, error) { requests <- frame; return len(frame), nil }
	mismatch, mismatchRemote := registerTestPSession(t, c, "claimed-peer")
	valid, validRemote := registerTestPSession(t, c, "valid-peer")

	out := make(chan []core.SearchResult, 1)
	go func() {
		results, _ := c.Search(context.Background(), "identity", 100*time.Millisecond)
		out <- results
	}()
	token, _ := waitSearchRequest(t, requests)
	writePeerResponse(t, mismatchRemote, makeSearchResponse(token, "different-peer", []peer.File{{Name: "spoof.flac", Size: 1, Extension: "flac"}}))
	select {
	case <-mismatch.done:
	case <-time.After(time.Second):
		t.Fatal("identity mismatch did not close its P session")
	}
	select {
	case <-valid.done:
		t.Fatal("identity mismatch closed an unrelated P session")
	default:
	}
	writePeerResponse(t, validRemote, makeSearchResponse(token, "valid-peer", []peer.File{{Name: "valid.flac", Size: 2, Extension: "flac"}}))

	results := <-out
	if len(results) != 1 || results[0].Username != "valid-peer" || results[0].Filename != "valid.flac" {
		t.Fatalf("identity-filtered results = %+v", results)
	}
}

func TestSearchTimeoutCancellationFailuresAndNonpositiveTimeout(t *testing.T) {
	t.Run("nonpositive timeout is successful without wire send", func(t *testing.T) {
		c, conn, _ := startSearchClient(t)
		for _, timeout := range []time.Duration{0, -time.Second} {
			got, err := c.Search(context.Background(), "q", timeout)
			if err != nil || len(got) != 0 {
				t.Fatalf("Search(%v) = %v, %v", timeout, got, err)
			}
		}
		conn.mu.Lock()
		writes := len(conn.writes)
		conn.mu.Unlock()
		if writes != 0 {
			t.Fatalf("nonpositive timeout wrote %d frames", writes)
		}
	})

	t.Run("caller cancellation returns partial and context error", func(t *testing.T) {
		c, conn, _ := startSearchClient(t)
		requests := make(chan []byte, 1)
		conn.writeFn = func(frame []byte) (int, error) { requests <- frame; return len(frame), nil }
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan struct {
			results []core.SearchResult
			err     error
		}, 1)
		go func() {
			results, err := c.Search(ctx, "q", time.Second)
			out <- struct {
				results []core.SearchResult
				err     error
			}{results, err}
		}()
		token, _ := waitSearchRequest(t, requests)
		c.searches.offer(token, core.SearchResult{Filename: "partial"})
		cancel()
		got := <-out
		if !errors.Is(got.err, context.Canceled) || len(got.results) != 1 {
			t.Fatalf("canceled Search = %+v, %v", got.results, got.err)
		}
		assertNoActiveSearches(t, c)
	})

	t.Run("server generation loss returns partial and error", func(t *testing.T) {
		c, conn, generation := startSearchClient(t)
		requests := make(chan []byte, 1)
		conn.writeFn = func(frame []byte) (int, error) { requests <- frame; return len(frame), nil }
		out := make(chan struct {
			results []core.SearchResult
			err     error
		}, 1)
		go func() {
			results, err := c.Search(context.Background(), "q", time.Second)
			out <- struct {
				results []core.SearchResult
				err     error
			}{results, err}
		}()
		token, _ := waitSearchRequest(t, requests)
		c.searches.offer(token, core.SearchResult{Filename: "partial"})
		c.searches.failGeneration(generation, errNoServerConnection)
		got := <-out
		if !errors.Is(got.err, errNoServerConnection) || len(got.results) != 1 {
			t.Fatalf("disconnected Search = %+v, %v", got.results, got.err)
		}
	})

	t.Run("write failure returns already offered partial", func(t *testing.T) {
		c, conn, _ := startSearchClient(t)
		writeErr := errors.New("forced write failure")
		conn.writeFn = func(frame []byte) (int, error) {
			token, _, err := parseSearchRequest(frame)
			if err != nil {
				return 0, err
			}
			c.searches.offer(token, core.SearchResult{Filename: "during-write"})
			return 0, writeErr
		}
		results, err := c.Search(context.Background(), "q", time.Second)
		if !errors.Is(err, writeErr) || len(results) != 1 || results[0].Filename != "during-write" {
			t.Fatalf("write-failed Search = %+v, %v", results, err)
		}
		assertNoActiveSearches(t, c)
	})
}

func TestConcurrentSearchesAreTokenIsolated(t *testing.T) {
	c, conn, _ := startSearchClient(t)
	requests := make(chan []byte, 2)
	conn.writeFn = func(frame []byte) (int, error) { requests <- frame; return len(frame), nil }

	type outcome struct {
		query   string
		results []core.SearchResult
		err     error
	}
	out := make(chan outcome, 2)
	for _, query := range []string{"first", "second"} {
		go func(query string) {
			results, err := c.Search(context.Background(), query, 80*time.Millisecond)
			out <- outcome{query: query, results: results, err: err}
		}(query)
	}

	tokens := make(map[string]soul.Token)
	for range 2 {
		token, query := waitSearchRequest(t, requests)
		tokens[query] = token
	}
	if tokens["first"] == tokens["second"] {
		t.Fatal("concurrent searches shared a token")
	}
	c.searches.offer(tokens["first"], core.SearchResult{Filename: "only-first"})
	c.searches.offer(tokens["second"], core.SearchResult{Filename: "only-second"})
	for range 2 {
		got := <-out
		if got.err != nil || len(got.results) != 1 || got.results[0].Filename != "only-"+got.query {
			t.Fatalf("isolated %q outcome = %+v, %v", got.query, got.results, got.err)
		}
	}
	assertNoActiveSearches(t, c)
}

func TestSearchRegistrySaturationMaximumLateAndIdentitySafety(t *testing.T) {
	r := newSearchRegistry()
	token := soul.Token(42)
	first := newSearchSubscription()
	if !r.add(token, 1, first) {
		t.Fatal("initial add failed")
	}
	if r.add(token, 1, newSearchSubscription()) {
		t.Fatal("duplicate token add succeeded")
	}
	for i := 0; i < searchResultBuffer+1; i++ {
		r.offer(token, core.SearchResult{Size: int64(i)})
	}
	if got := len(first.results); got != searchResultBuffer {
		t.Fatalf("buffer length = %d, want %d", got, searchResultBuffer)
	}
	if got := first.dropped.Load(); got != 1 {
		t.Fatalf("buffer drops = %d, want 1", got)
	}
	stale := newSearchSubscription()
	if r.removeIfSame(token, stale) {
		t.Fatal("stale removal removed active subscription")
	}
	if !r.removeIfSame(token, first) {
		t.Fatal("identity-matched removal failed")
	}
	if r.offer(token, core.SearchResult{}) {
		t.Fatal("late/unknown token was delivered")
	}
}

func TestSearchDrainsPastMaximumWithoutBlockingDelivery(t *testing.T) {
	c, conn, _ := startSearchClient(t)
	requests := make(chan []byte, 1)
	conn.writeFn = func(frame []byte) (int, error) { requests <- frame; return len(frame), nil }
	doneErr := errors.New("delivery complete")
	out := make(chan struct {
		results []core.SearchResult
		err     error
	}, 1)
	go func() {
		results, err := c.Search(context.Background(), "many", time.Second)
		out <- struct {
			results []core.SearchResult
			err     error
		}{results, err}
	}()
	token, _ := waitSearchRequest(t, requests)
	subscription := c.searches.subscription(token)
	if subscription == nil {
		t.Fatal("missing active subscription")
	}
	const offered = maxSearchResults + 100
	for i := 0; i < offered; i++ {
		for len(subscription.results) == cap(subscription.results) {
			runtime.Gosched()
		}
		subscription.offer(core.SearchResult{Size: int64(i)})
	}
	waitTree(t, func() bool { return len(subscription.results) == 0 }, "Search did not continue draining after maximum")
	subscription.fail(doneErr)
	got := <-out
	if !errors.Is(got.err, doneErr) {
		t.Fatalf("Search error = %v", got.err)
	}
	if len(got.results) != maxSearchResults {
		t.Fatalf("returned results = %d, want %d", len(got.results), maxSearchResults)
	}
	if dropped := subscription.dropped.Load(); dropped != offered-maxSearchResults {
		t.Fatalf("max-result drops = %d, want %d", dropped, offered-maxSearchResults)
	}
}

func TestSearchPeerHookLargeResponseMappingAndSessionIsolation(t *testing.T) {
	c, conn, _ := startSearchClient(t)
	requests := make(chan []byte, 1)
	conn.writeFn = func(frame []byte) (int, error) { requests <- frame; return len(frame), nil }
	bad, badRemote := registerTestPSession(t, c, "bad-peer")
	unsupportedSession, unsupportedRemote := registerTestPSession(t, c, "unsupported-peer")
	_, goodRemote := registerTestPSession(t, c, "good-peer")

	out := make(chan []core.SearchResult, 1)
	go func() {
		results, _ := c.Search(context.Background(), "q", 100*time.Millisecond)
		out <- results
	}()
	token, _ := waitSearchRequest(t, requests)

	// A complete frame with a code-9 header but malformed compressed payload
	// closes only this P session.
	malformedPayload := make([]byte, 5)
	binary.LittleEndian.PutUint32(malformedPayload[:4], uint32(peer.CodeFileSearchResponse))
	malformedPayload[4] = 0xff
	if _, err := badRemote.Write(packFrame(malformedPayload)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bad.done:
	case <-time.After(time.Second):
		t.Fatal("malformed code-9 response did not close its P session")
	}

	// An unsupported P code closes the active hostile session so it cannot
	// retain an inbound lease indefinitely by refreshing the idle deadline.
	unsupported := make([]byte, 4)
	binary.LittleEndian.PutUint32(unsupported, 0xffff)
	if _, err := unsupportedRemote.Write(packFrame(unsupported)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-unsupportedSession.done:
	case <-time.After(time.Second):
		t.Fatal("unsupported P code did not close its session")
	}
	files := []peer.File{
		{Name: "ok.flac", Size: 10, Extension: "flac"},
		{Name: "too-large.flac", Size: uint64(math.MaxInt64) + 1, Extension: "flac"},
	}
	writePeerResponse(t, goodRemote, makeSearchResponse(token, "good-peer", files))
	results := <-out
	if len(results) != 1 || results[0].Filename != "ok.flac" || results[0].Username != "good-peer" {
		t.Fatalf("good session results = %+v", results)
	}
}

func TestSearchHookOneLargeResponseNeverBlocksPeerReader(t *testing.T) {
	registry := newSearchRegistry()
	subscription := newSearchSubscription()
	const token soul.Token = 99
	if !registry.add(token, 1, subscription) {
		t.Fatal("add subscription")
	}
	files := make([]peer.File, searchResultBuffer+44)
	for i := range files {
		files[i] = peer.File{Name: fmt.Sprintf("%03d.flac", i), Size: uint64(i + 1), Extension: "flac"}
	}
	wire, err := (&peer.FileSearchResponse{}).Serialize(makeSearchResponse(token, "wire-name", files))
	if err != nil {
		t.Fatal(err)
	}
	hook := &searchSessionHooks{searches: registry}
	session := &peerSession{key: sessionKey{username: "wire-name", connType: peer.ConnectionType}}
	started := time.Now()
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeFileSearchResponse), wire: wire}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("nonblocking response routing took %v", elapsed)
	}
	if got := len(subscription.results); got != searchResultBuffer {
		t.Fatalf("large response buffered %d, want %d", got, searchResultBuffer)
	}
	if got := subscription.dropped.Load(); got != 44 {
		t.Fatalf("large response drops = %d, want 44", got)
	}
}

// TestSearchHookNonResponseCodeIsUnhandled locks in the claim-sentinel
// contract: searchSessionHooks only owns code 9 (FileSearchResponse). Any
// other P code — including download codes owned by a future hook — must be
// reported as unclaimed via errUnhandledPeerFrame rather than a hard error,
// so a sibling hook in the composed dispatch gets a chance to claim it.
func TestSearchHookNonResponseCodeIsUnhandled(t *testing.T) {
	hook := &searchSessionHooks{searches: newSearchRegistry()}
	session := &peerSession{key: sessionKey{username: "peer", connType: peer.ConnectionType}}
	err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeTransferRequest)})
	if !errors.Is(err, errUnhandledPeerFrame) {
		t.Fatalf("searchSessionHooks.frame(TransferRequest) = %v, want errUnhandledPeerFrame", err)
	}
}

// TestComposedSessionHooksUnknownCodeStillClosesSession complements
// TestSearchPeerHookLargeResponseMappingAndSessionIsolation: even once every
// registered hook can decline a frame it doesn't own via errUnhandledPeerFrame,
// a code no hook claims must still fail the frame (and thus close the
// session) rather than being silently accepted. This is the lease-abuse
// protection the claim-sentinel refactor must preserve.
func TestComposedSessionHooksUnknownCodeStillClosesSession(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	session := &peerSession{key: sessionKey{username: "peer", connType: peer.ConnectionType}}
	err := c.sessionHooks.frame(session, sessionFrame{connType: peer.ConnectionType, code: 0xffff})
	if err == nil {
		t.Fatal("composed session hooks did not error on an unclaimed peer code")
	}
}

func TestSearchNumericChecksAndShutdownFailure(t *testing.T) {
	if _, ok := mapSearchResult("u", false, 0, 0, peer.File{Name: "huge", Size: uint64(math.MaxInt64) + 1}); ok {
		t.Fatal("uint64 size above MaxInt64 was mapped")
	}
	if got, ok := mapSearchResult("u", false, 1, 2, peer.File{Name: "missing", Size: 1}); !ok || got.BitRate != 0 {
		t.Fatalf("missing bitrate mapping = %+v, %v", got, ok)
	}
	value, ok := checkedUint32ToInt(math.MaxUint32)
	if uint64(^uint(0))>>32 == 0 {
		if ok || value >= 0 {
			t.Fatalf("32-bit max uint32 conversion = %d, %v", value, ok)
		}
	} else if !ok || uint64(value) != math.MaxUint32 {
		t.Fatalf("64-bit max uint32 conversion = %d, %v", value, ok)
	}

	registry := newSearchRegistry()
	one, two := newSearchSubscription(), newSearchSubscription()
	registry.add(1, 1, one)
	registry.add(2, 2, two)
	registry.failAll(context.Canceled)
	for i, subscription := range []*searchSubscription{one, two} {
		select {
		case err := <-subscription.failures:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("shutdown subscription %d error = %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("shutdown did not fail subscription %d", i)
		}
	}
}

func assertNoActiveSearches(t *testing.T, c *Client) {
	t.Helper()
	c.searches.mu.Lock()
	active := len(c.searches.active)
	c.searches.mu.Unlock()
	c.tokens.mu.Lock()
	reserved := len(c.tokens.reservations)
	c.tokens.mu.Unlock()
	if active != 0 || reserved != 0 {
		t.Fatalf("active searches/tokens after return = %d/%d", active, reserved)
	}
}
