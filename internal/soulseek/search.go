package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

const (
	searchResultBuffer = 256
	maxSearchResults   = 2000
)

type searchSubscription struct {
	generation uint64
	results    chan core.SearchResult
	failures   chan error
	failOnce   sync.Once
	dropped    atomic.Uint64
}

func newSearchSubscription() *searchSubscription {
	return &searchSubscription{
		results:  make(chan core.SearchResult, searchResultBuffer),
		failures: make(chan error, 1),
	}
}

func (s *searchSubscription) fail(err error) {
	if err == nil {
		err = errNoServerConnection
	}
	s.failOnce.Do(func() { s.failures <- err })
}

func (s *searchSubscription) offer(result core.SearchResult) {
	select {
	case s.results <- result:
	default:
		s.dropped.Add(1)
	}
}

type searchRegistry struct {
	mu     sync.Mutex
	active map[soul.Token]*searchSubscription
}

func newSearchRegistry() *searchRegistry {
	return &searchRegistry{active: make(map[soul.Token]*searchSubscription)}
}

func (r *searchRegistry) add(token soul.Token, generation uint64, subscription *searchSubscription) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.active[token]; exists {
		return false
	}
	subscription.generation = generation
	r.active[token] = subscription
	return true
}

func (r *searchRegistry) removeIfSame(token soul.Token, subscription *searchSubscription) bool {
	r.mu.Lock()
	removed := r.active[token] == subscription
	if removed {
		delete(r.active, token)
	}
	r.mu.Unlock()
	return removed
}

func (r *searchRegistry) subscription(token soul.Token) *searchSubscription {
	r.mu.Lock()
	subscription := r.active[token]
	r.mu.Unlock()
	return subscription
}

func (r *searchRegistry) offer(token soul.Token, result core.SearchResult) bool {
	subscription := r.subscription(token)
	if subscription == nil {
		return false
	}
	subscription.offer(result)
	return true
}

func (r *searchRegistry) failGeneration(generation uint64, err error) {
	var failed []*searchSubscription
	r.mu.Lock()
	for token, subscription := range r.active {
		if subscription.generation == generation {
			delete(r.active, token)
			failed = append(failed, subscription)
		}
	}
	r.mu.Unlock()
	for _, subscription := range failed {
		subscription.fail(err)
	}
}

func (r *searchRegistry) failAll(err error) {
	var failed []*searchSubscription
	r.mu.Lock()
	for token, subscription := range r.active {
		delete(r.active, token)
		failed = append(failed, subscription)
	}
	r.mu.Unlock()
	for _, subscription := range failed {
		subscription.fail(err)
	}
}

// Search sends a direct Soulseek search through the active central-server
// generation and accumulates peer responses until timeout. The requested
// timeout is successful completion (including nonpositive durations); caller
// cancellation and connection/shutdown failures return partial results plus
// their error.
func (c *Client) Search(ctx context.Context, query string, timeout time.Duration) ([]core.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, nil
	}
	deadline := time.Now().Add(timeout)

	reservation := c.tokens.Reserve()
	subscription := newSearchSubscription()
	var results []core.SearchResult
	registered := false
	defer func() {
		if registered {
			c.searches.removeIfSame(reservation.token, subscription)
		}
		reservation.Release()
	}()

	// serverWriteMu binds registration and the code-26 write to one exact
	// central-server generation. Teardown takes the same lock before clearing
	// that generation, so reconnect cannot split these two operations.
	c.serverWriteMu.Lock()
	c.mu.Lock()
	if c.serverConn == nil {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return nil, errors.New("soulseek: not connected to server")
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return nil, err
	}
	if !time.Now().Before(deadline) {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return nil, nil
	}
	conn := c.serverConn
	cancelServer := c.serverCancel
	generation := c.serverGeneration
	if !c.searches.add(reservation.token, generation, subscription) {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return nil, errors.New("soulseek: reserved search token already active")
	}
	registered = true
	c.mu.Unlock()

	_, writeErr := server.Write(conn, &server.FileSearch{Token: reservation.token, SearchQuery: query})
	c.serverWriteMu.Unlock()
	finish := func(err error) ([]core.SearchResult, error) {
		c.searches.removeIfSame(reservation.token, subscription)
		for {
			select {
			case result := <-subscription.results:
				appendSearchResult(subscription, &results, result)
			default:
				if dropped := subscription.dropped.Load(); dropped > 0 && c.logger != nil {
					c.logger.Debug("soulseek search results dropped", "count", dropped)
				}
				return results, err
			}
		}
	}
	if writeErr != nil {
		if cancelServer != nil {
			cancelServer()
		}
		_ = conn.Close()
		return finish(fmt.Errorf("write file search: %w", writeErr))
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case result := <-subscription.results:
			appendSearchResult(subscription, &results, result)
		case err := <-subscription.failures:
			return finish(err)
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-timer.C:
			return finish(nil)
		}
	}
}

func appendSearchResult(subscription *searchSubscription, results *[]core.SearchResult, result core.SearchResult) {
	if len(*results) < maxSearchResults {
		*results = append(*results, result)
	} else {
		subscription.dropped.Add(1)
	}
}

// searchSessionHooks claims only code-9 (FileSearchResponse) P-frames; any
// other code is reported unhandled via errUnhandledPeerFrame so a sibling
// hook in the composed dispatch can claim it. Framing has already been
// validated by peerSession.readFrame; malformed code-9 payloads and
// claimed-identity mismatches close only the originating P session.
type searchSessionHooks struct {
	searches *searchRegistry
}

func (*searchSessionHooks) established(*peerSession) {}

func (h *searchSessionHooks) frame(session *peerSession, frame sessionFrame) error {
	if frame.connType != peer.ConnectionType {
		return errUnhandledPeerFrame
	}
	if frame.code != int(peer.CodeFileSearchResponse) {
		// Not a search response; this hook doesn't own the code (it may belong
		// to a sibling hook, e.g. the download hooks in a later group).
		return errUnhandledPeerFrame
	}

	response := &peer.FileSearchResponse{}
	if err := response.Deserialize(bytes.NewReader(frame.wire)); err != nil {
		return fmt.Errorf("deserialize file search response: %w", err)
	}
	// PeerInit is only a protocol claim, not cryptographic authentication.
	// Requiring the response to repeat the same identity prevents ambiguous
	// attribution while keeping a mismatch isolated to this P session.
	if response.Username != session.key.username {
		return fmt.Errorf("file search response username %q does not match peer init username %q", response.Username, session.key.username)
	}

	subscription := h.searches.subscription(response.Token)
	if subscription == nil {
		return nil
	}
	uploadSpeed, ok := checkedNonnegativeInt(response.AverageSpeed)
	if !ok {
		return nil
	}
	queueLength, ok := checkedNonnegativeInt(response.Queue)
	if !ok {
		return nil
	}
	for i, file := range response.Results {
		result, ok := mapSearchResult(session.key.username, response.FreeSlot, uploadSpeed, queueLength, file)
		if !ok {
			continue
		}
		subscription.offer(result)
		if (i+1)%searchResultBuffer == 0 {
			runtime.Gosched()
		}
	}
	return nil
}

func (*searchSessionHooks) closed(*peerSession, error) {}

func mapSearchResult(username string, freeSlot bool, uploadSpeed, queueLength int, file peer.File) (core.SearchResult, bool) {
	if file.Size > math.MaxInt64 {
		return core.SearchResult{}, false
	}

	bitrate := 0
	for _, attribute := range file.Attributes {
		if attribute.Code != peer.Bitrate {
			continue
		}
		value, ok := checkedUint32ToInt(attribute.Value)
		if !ok {
			return core.SearchResult{}, false
		}
		bitrate = value
		break
	}

	return core.SearchResult{
		Username:          username,
		Filename:          file.Name,
		Size:              int64(file.Size),
		BitRate:           bitrate,
		HasFreeUploadSlot: freeSlot,
		QueueLength:       queueLength,
		UploadSpeed:       uploadSpeed,
	}, true
}

func checkedUint32ToInt(value uint32) (int, bool) {
	converted := int(value)
	return converted, converted >= 0 && uint64(converted) == uint64(value)
}

func checkedNonnegativeInt(value int) (int, bool) {
	return value, value >= 0
}
