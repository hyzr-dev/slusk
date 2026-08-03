package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
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
// their error. A thin wrapper over runSearch, which holds the actual
// token-reservation / serverWriteMu / generation dance shared with
// SearchStream.
func (c *Client) Search(ctx context.Context, query string, timeout time.Duration) ([]core.SearchResult, error) {
	var results []core.SearchResult
	sink := func(result core.SearchResult) {
		results = append(results, result)
	}
	err := c.runSearch(ctx, query, timeout, maxSearchResults, sink, nil)
	return results, err
}

// searchStreamBatch is how many results SearchStream accumulates before
// flushing to emit — batches many small deliveries into fewer, larger frames
// for the session registry (internal/app) to fan out over SSE, rather than
// emitting a slice of one for every single peer response.
const searchStreamBatch = 64

// SearchStream runs a native Soulseek search like Search, but calls emit
// incrementally as results arrive — batched at searchStreamBatch, or flushed
// sooner whenever the subscription's result channel momentarily drains (see
// runSearch's drained hook), so a slow trickle of results well under
// searchStreamBatch still surfaces promptly instead of waiting for the batch
// to fill or the whole search to finish. emit is called only from this
// goroutine, never concurrently with itself and never after SearchStream
// returns; the slice it receives is not retained or mutated afterward by this
// method, so the caller may keep it. Unlike Search, no cap is applied here —
// the caller (internal/app's session registry) applies its own.
//
// Returns nil on normal timeout completion, and the failure (with everything
// already emitted retained by the caller) on context cancellation or
// connection/write failure — same contract as Search.
func (c *Client) SearchStream(ctx context.Context, query string, timeout time.Duration, emit func([]core.SearchResult)) error {
	var pending []core.SearchResult
	flush := func() {
		if len(pending) == 0 {
			return
		}
		emit(pending)
		pending = nil
	}
	sink := func(result core.SearchResult) {
		pending = append(pending, result)
		if len(pending) >= searchStreamBatch {
			flush()
		}
	}
	// maxResults=0: no cap here — the caller (internal/app's session
	// registry) applies its own, per SearchStream's own doc comment.
	err := c.runSearch(ctx, query, timeout, 0, sink, flush)
	// Final flush: runSearch's own post-return drain (see finish) delivers any
	// results still buffered in the subscription's channel straight to sink,
	// but never calls the drained hook itself, so a partial batch below
	// searchStreamBatch left over at the very end needs this explicit flush.
	flush()
	return err
}

// SearchStreaming reports that the native backend delivers search results
// genuinely incrementally as the network responds, not batched at
// completion — unlike slskd (see internal/slskd.Client.SearchStreaming),
// whose GET /responses returns nothing until its search finalizes.
func (c *Client) SearchStreaming() bool { return true }

// runSearch performs the whole lifecycle of one Soulseek network search:
// reserves a token, registers a subscription against the current server
// generation, writes the FileSearch frame, and pumps every result the peer
// hooks deliver into sink until timeout, cancellation, or subscription
// failure. This is the shared, subtlest part of Search and SearchStream and
// must never be duplicated — the token-reservation / serverWriteMu /
// generation dance binds registration to one exact central-server
// generation, and teardown takes the same lock before clearing that
// generation, so a reconnect cannot split the two operations.
//
// sink is called synchronously from this goroutine for every ACCEPTED result
// (see maxResults), both during the wait loop and while draining any results
// still buffered in the subscription's channel after it returns, so it never
// races itself. drained, if non-nil, is called after the wait loop has fully
// drained every result currently queued in the channel — i.e. at a natural
// pause in delivery — letting a streaming caller (SearchStream) flush a
// partial batch promptly rather than only at searchStreamBatch or at the
// very end; it is never called from the post-return drain in finish; a
// streaming caller does its own final flush after this method returns.
//
// maxResults caps how many results are ever passed to sink (0 = unlimited);
// anything beyond the cap increments subscription.dropped instead — the same
// counter offer's own channel-buffer overflow uses, so finish's single debug
// log line accounts for both causes, exactly as it did before Search's body
// was extracted into this shared method. Search passes maxSearchResults;
// SearchStream passes 0 and applies no cap of its own — its caller
// (internal/app's session registry) applies its own.
//
// Returns nil on normal timeout completion (including a nonpositive/expired
// timeout, which is a no-op success), and the failure (with everything
// already delivered to sink) on caller cancellation or a connection/write
// failure.
func (c *Client) runSearch(ctx context.Context, query string, timeout time.Duration, maxResults int, sink func(core.SearchResult), drained func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if timeout <= 0 {
		return nil
	}
	if phrase, excluded := matchExcludedPhrase(query, c.excludedPhrases.Load()); excluded {
		// Every well-behaved peer refuses to answer a search whose terms cover
		// an excluded phrase, so nothing is gained by putting this on the wire
		// (issue #319). Report it truthfully rather than silently returning an
		// empty result set indistinguishable from "nobody has this".
		//
		// The check sits in runSearch rather than in Search so that the manual
		// search path (SearchStream, issue #58) inherits it: a user typing an
		// excluded phrase gets the real reason instead of "no hits".
		return fmt.Errorf("%w: %q", core.ErrSearchExcluded, phrase)
	}
	deadline := time.Now().Add(timeout)

	reservation := c.tokens.Reserve()
	subscription := newSearchSubscription()
	registered := false
	defer func() {
		if registered {
			c.searches.removeIfSame(reservation.token, subscription)
		}
		reservation.Release()
	}()
	accepted := 0
	process := func(result core.SearchResult) {
		if maxResults > 0 && accepted >= maxResults {
			subscription.dropped.Add(1)
			return
		}
		accepted++
		sink(result)
	}

	// serverWriteMu binds registration and the code-26 write to one exact
	// central-server generation. Teardown takes the same lock before clearing
	// that generation, so reconnect cannot split these two operations.
	c.serverWriteMu.Lock()
	c.mu.Lock()
	if c.serverConn == nil {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return errors.New("soulseek: not connected to server")
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return err
	}
	if !time.Now().Before(deadline) {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return nil
	}
	conn := c.serverConn
	cancelServer := c.serverCancel
	generation := c.serverGeneration
	if !c.searches.add(reservation.token, generation, subscription) {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return errors.New("soulseek: reserved search token already active")
	}
	registered = true
	c.mu.Unlock()

	// writeServerLocked (not a bare server.Write) so this shares the standard
	// write deadline: the absolute deadline a prior sendToServerGeneration write
	// left on this shared conn would otherwise already be expired here and fail
	// the write immediately with i/o timeout.
	writeErr := writeServerLocked(c, conn, &server.FileSearch{Token: reservation.token, SearchQuery: query})
	c.serverWriteMu.Unlock()
	finish := func(err error) error {
		c.searches.removeIfSame(reservation.token, subscription)
		for {
			select {
			case result := <-subscription.results:
				process(result)
			default:
				if dropped := subscription.dropped.Load(); dropped > 0 && c.logger != nil {
					c.logger.Debug("soulseek search results dropped", "count", dropped)
				}
				return err
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
			process(result)
			if drained == nil {
				continue
			}
			// Non-blocking drain of anything else already queued, so drained
			// fires once per natural pause in delivery rather than once per
			// result.
		drainLoop:
			for {
				select {
				case r := <-subscription.results:
					process(r)
				default:
					break drainLoop
				}
			}
			drained()
		case err := <-subscription.failures:
			return finish(err)
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-timer.C:
			return finish(nil)
		}
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

	if subscription := h.searches.subscription(response.Token); subscription != nil {
		if uploadSpeed, ok := checkedNonnegativeInt(response.AverageSpeed); ok {
			if queueLength, ok := checkedNonnegativeInt(response.Queue); ok {
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
			}
		}
	}
	return releaseResponderSession(session)
}

// errResponderDelivered closes a P session that only ever delivered search
// responses to us. Returned as a frame result so readLoop performs the close;
// it is a routine lifecycle event, not a failure.
var errResponderDelivered = errors.New("soulseek: releasing responder connection after search response")

// releaseResponderSession drops a one-way responder session - one that pushed a
// FileSearchResponse to us and that we never wrote to - so its inbound lease
// frees promptly instead of lingering peerIdleTimeout while thousands more
// responses arrive; we re-establish on demand if we later download from this
// peer. A session we are using (wrote is set: download/upload/browse) is kept.
func releaseResponderSession(session *peerSession) error {
	if session.wrote.Load() {
		return nil
	}
	return errResponderDelivered
}

func (*searchSessionHooks) closed(*peerSession, error) {}

func mapSearchResult(username string, freeSlot bool, uploadSpeed, queueLength int, file peer.File) (core.SearchResult, bool) {
	if file.Size > math.MaxInt64 {
		return core.SearchResult{}, false
	}

	var duration, sampleRate, bitDepth int
	bitrate := 0
	bitrateSeen := false
	var variableBitRate bool
	for _, attribute := range file.Attributes {
		switch attribute.Code {
		case peer.Bitrate:
			// First Bitrate attribute wins; later ones are ignored. This is
			// the `break` the pre-switch loop had, expressed so the loop can
			// still reach the other codes below: a file carrying a valid
			// bitrate followed by an out-of-range one keeps the first value
			// and is accepted, exactly as before. Only a FIRST out-of-range
			// bitrate discards the whole file, because BitRate feeds
			// matcher.passesFloor and therefore automation — unlike the
			// branches below, which merely leave their field at zero on the
			// same conversion failure.
			if bitrateSeen {
				continue
			}
			bitrateSeen = true
			value, ok := checkedUint32ToInt(attribute.Value)
			if !ok {
				return core.SearchResult{}, false
			}
			bitrate = value
		case peer.Duration:
			if value, ok := checkedUint32ToInt(attribute.Value); ok {
				duration = value
			}
		case peer.VBR:
			variableBitRate = attribute.Value != 0
		case peer.SampleRate:
			if value, ok := checkedUint32ToInt(attribute.Value); ok {
				sampleRate = value
			}
		case peer.BitDepth:
			if value, ok := checkedUint32ToInt(attribute.Value); ok {
				bitDepth = value
			}
		default:
			// Unknown or unused (code 3) — ignored.
		}
	}

	return core.SearchResult{
		Username:          username,
		Filename:          file.Name,
		Size:              int64(file.Size),
		BitRate:           bitrate,
		HasFreeUploadSlot: freeSlot,
		QueueLength:       queueLength,
		UploadSpeed:       uploadSpeed,
		Duration:          duration,
		SampleRate:        sampleRate,
		BitDepth:          bitDepth,
		VariableBitRate:   variableBitRate,
	}, true
}

func checkedUint32ToInt(value uint32) (int, bool) {
	converted := int(value)
	return converted, converted >= 0 && uint64(converted) == uint64(value)
}

func checkedNonnegativeInt(value int) (int, bool) {
	return value, value >= 0
}

// matchExcludedPhrase reports whether query covers any phrase in phrases,
// using the server's actual matching semantics (empirically measured for
// issue #319): token-set containment, case-insensitive, order-independent and
// position-independent - not substring or adjacency matching. A phrase
// matches when every one of its tokens appears somewhere among the query's
// tokens, regardless of order or what else the query contains. phrases may be
// nil (nothing pushed yet, or an empty list), in which case nothing matches.
func matchExcludedPhrase(query string, phrases *[]string) (phrase string, matched bool) {
	if phrases == nil || len(*phrases) == 0 {
		return "", false
	}
	queryTokens := tokenSet(query)
	if len(queryTokens) == 0 {
		return "", false
	}
	for _, p := range *phrases {
		phraseTokens := strings.Fields(strings.ToLower(p))
		if len(phraseTokens) == 0 {
			continue
		}
		covered := true
		for _, t := range phraseTokens {
			if !queryTokens[t] {
				covered = false
				break
			}
		}
		if covered {
			return p, true
		}
	}
	return "", false
}

// tokenSet lowercases and whitespace-splits s into a set of unique tokens.
func tokenSet(s string) map[string]bool {
	fields := strings.Fields(strings.ToLower(s))
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return set
}
