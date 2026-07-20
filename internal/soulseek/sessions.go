package soulseek

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

const (
	defaultInboundPeerLimit                  = 128
	defaultSessionWriteQueue                 = 32
	defaultPeerIdleTimeout                   = 2 * time.Minute
	defaultInboundPeerSessionLifetime        = 10 * time.Minute
	maxPeerInitFrameSize              uint32 = 4 << 10
	maxPeerUsernameSize                      = int(peer.MaxPeerInitUsernameSize)
	maxDistributedFrameSize           uint32 = 16 << 10
	maxOrdinaryPeerFrameSize          uint32 = 16 << 20
)

type sessionInitiator uint8

const (
	sessionInitiatorLocal sessionInitiator = iota + 1
	sessionInitiatorRemote
)

type sessionRole uint8

const (
	sessionRoleOrdinary sessionRole = iota + 1
	sessionRoleParent
	sessionRoleChild
)

type sessionKey struct {
	username string
	connType soul.ConnectionType
}

// sessionTarget is the direct-address establishment input used by the
// distributed-tree worker. Unlike ConnectPeer, it neither asks the central
// server for an address nor falls back to the indirect connection dance.
type sessionTarget struct {
	username string
	connType soul.ConnectionType
	address  string
}

// sessionFrame is one complete, framing-validated P or D message. wire holds
// the size prefix, code, and payload so the next worker can deserialize the
// concrete message without another socket read.
type sessionFrame struct {
	connType soul.ConnectionType
	code     int
	wire     []byte
}

// sessionHooks is the narrow integration seam for the tree, search, and
// (future) download workers. Hooks run without component locks held.
// Returning an error from frame closes only that peer session.
type sessionHooks interface {
	established(*peerSession)
	frame(*peerSession, sessionFrame) error
	closed(*peerSession, error)
}

// errUnhandledPeerFrame is returned by an individual hook's frame method to
// signal that the frame's code is not owned by that hook, so the composed
// dispatch should offer it to the next hook instead of closing the session.
// It must never escape composedSessionHooks.frame: a code no hook claims is
// itself an error (see composedSessionHooks.frame in tree.go).
var errUnhandledPeerFrame = errors.New("soulseek: peer frame not handled by this hook")

type discardSessionHooks struct{}

func (discardSessionHooks) established(*peerSession)               {}
func (discardSessionHooks) frame(*peerSession, sessionFrame) error { return nil }
func (discardSessionHooks) closed(*peerSession, error)             {}

type inboundLease struct {
	once    sync.Once
	release func()
}

func (l *inboundLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(l.release)
}

// tokenAllocator is shared by indirect connection attempts and future
// searches. Each reservation has private pointer identity, so a stale release
// cannot free a token that has since been assigned to another reservation.
type tokenAllocator struct {
	mu           sync.Mutex
	reservations map[soul.Token]*tokenReservation
}

type tokenReservation struct {
	allocator *tokenAllocator
	token     soul.Token
	once      sync.Once
}

func newTokenAllocator() *tokenAllocator {
	return &tokenAllocator{reservations: make(map[soul.Token]*tokenReservation)}
}

func (a *tokenAllocator) Reserve() *tokenReservation {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		token := soul.NewToken()
		if _, exists := a.reservations[token]; exists {
			continue
		}
		reservation := &tokenReservation{allocator: a, token: token}
		a.reservations[token] = reservation
		return reservation
	}
}

func (r *tokenReservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.allocator.mu.Lock()
		if r.allocator.reservations[r.token] == r {
			delete(r.allocator.reservations, r.token)
		}
		r.allocator.mu.Unlock()
	})
}

// duplicateSessionPolicy returns the candidate to retain. The safe default
// retains the existing session; it deliberately assumes no lexical username
// ordering. A complementary policy can be injected after protocol evidence.
type duplicateSessionPolicy func(existing, candidate *peerSession) *peerSession

func keepExistingSession(existing, _ *peerSession) *peerSession { return existing }

type sessionRegistry struct {
	mu                sync.Mutex
	sessions          map[sessionKey]*peerSession
	closedGenerations map[soul.ConnectionType]uint64
	policy            duplicateSessionPolicy
}

func newSessionRegistry(policy duplicateSessionPolicy) *sessionRegistry {
	if policy == nil {
		policy = keepExistingSession
	}
	return &sessionRegistry{
		sessions:          make(map[sessionKey]*peerSession),
		closedGenerations: make(map[soul.ConnectionType]uint64),
		policy:            policy,
	}
}

func (r *sessionRegistry) Get(key sessionKey) *peerSession {
	r.mu.Lock()
	s := r.sessions[key]
	r.mu.Unlock()
	return s
}

// Register atomically rejects closed generations and applies duplicate
// arbitration under the registry lock, then closes the loser after unlocking.
func (r *sessionRegistry) Register(candidate *peerSession) (winner *peerSession, inserted bool) {
	var loser *peerSession
	var loserReason = errors.New("duplicate peer session")
	r.mu.Lock()
	if candidate.generation != 0 && candidate.generation <= r.closedGenerations[candidate.key.connType] {
		loser = candidate
		loserReason = errNoServerConnection
	} else {
		existing := r.sessions[candidate.key]
		if existing == nil {
			r.sessions[candidate.key] = candidate
			winner, inserted = candidate, true
		} else if r.policy(existing, candidate) == candidate {
			r.sessions[candidate.key] = candidate
			winner, inserted, loser = candidate, true, existing
		} else {
			winner, loser = existing, candidate
		}
	}
	r.mu.Unlock()
	if loser != nil {
		loser.Close(loserReason)
	}
	return winner, inserted
}

func (r *sessionRegistry) RemoveIfSame(key sessionKey, session *peerSession) bool {
	r.mu.Lock()
	removed := r.sessions[key] == session
	if removed {
		delete(r.sessions, key)
	}
	r.mu.Unlock()
	return removed
}

func (r *sessionRegistry) Snapshot() []*peerSession {
	r.mu.Lock()
	out := make([]*peerSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		out = append(out, session)
	}
	r.mu.Unlock()
	return out
}

func (r *sessionRegistry) CloseAll(reason error) {
	for _, session := range r.Snapshot() {
		session.Close(reason)
	}
}

// CloseGeneration invalidates a generation under the same lock used by
// Register, then closes its detached sessions without holding that lock.
func (r *sessionRegistry) CloseGeneration(connType soul.ConnectionType, generation uint64, reason error) {
	var closing []*peerSession
	r.mu.Lock()
	if generation > r.closedGenerations[connType] {
		r.closedGenerations[connType] = generation
	}
	for key, session := range r.sessions {
		if session.key.connType == connType && session.generation == generation {
			delete(r.sessions, key)
			closing = append(closing, session)
		}
	}
	r.mu.Unlock()
	for _, session := range closing {
		session.Close(reason)
	}
}

type sessionEstablishment struct {
	done    chan struct{}
	session *peerSession
	err     error
}

type peerSession struct {
	key        sessionKey
	initiator  sessionInitiator
	role       sessionRole
	generation uint64

	conn     net.Conn
	registry *sessionRegistry
	hooks    sessionHooks
	lease    *inboundLease
	client   *Client

	ctx              context.Context
	cancel           context.CancelFunc
	writes           chan []byte
	queuedWriteBytes atomic.Int64
	maxQueuedBytes   int64
	absoluteDeadline time.Time
	closeOnce        sync.Once
	done             chan struct{}
}

func (c *Client) newSession(conn net.Conn, key sessionKey, initiator sessionInitiator, role sessionRole, generation uint64, lease *inboundLease) *peerSession {
	ctx, cancel := context.WithCancel(c.lifecycleContext())
	maxFrameSize := int64(maxOrdinaryPeerFrameSize) + 4 // include size prefix retained in each queued frame
	if key.connType == distributed.ConnectionType {
		maxFrameSize = int64(maxDistributedFrameSize) + 4
	}
	var absoluteDeadline time.Time
	if key.connType == peer.ConnectionType && role == sessionRoleOrdinary && lease != nil {
		absoluteDeadline = time.Now().Add(c.cfg.inboundPeerSessionLifetime)
	}
	c.peerConns.Add(1)
	return &peerSession{
		key: key, initiator: initiator, role: role, generation: generation,
		conn: conn, registry: c.sessions, hooks: c.sessionHooks, lease: lease, client: c,
		ctx: ctx, cancel: cancel, writes: make(chan []byte, c.cfg.sessionWriteQueue),
		maxQueuedBytes: maxFrameSize * int64(c.cfg.sessionWriteQueue), absoluteDeadline: absoluteDeadline,
		done: make(chan struct{}),
	}
}

// registerSession starts the sole reader and bounded writer only after the
// handshake completed and the candidate won registry arbitration.
func (c *Client) registerSession(candidate *peerSession) *peerSession {
	winner, inserted := c.sessions.Register(candidate)
	if !inserted {
		return winner
	}
	candidate.hooks.established(candidate)
	if !c.startTracked(candidate.writeLoop) || !c.startTracked(candidate.readLoop) {
		candidate.Close(errors.New("client lifecycle is stopping"))
	}
	return candidate
}

// TrySend queues one complete serialized frame without blocking. The copied
// bytes cannot be mutated by the caller while the writer owns them.
func (s *peerSession) TrySend(frame []byte) bool {
	frameSize := int64(len(frame))
	if frameSize <= 0 || frameSize > s.maxQueuedBytes {
		return false
	}
	for {
		queued := s.queuedWriteBytes.Load()
		if queued > s.maxQueuedBytes-frameSize {
			return false
		}
		if s.queuedWriteBytes.CompareAndSwap(queued, queued+frameSize) {
			break
		}
	}
	copyOfFrame := append([]byte(nil), frame...)
	select {
	case <-s.ctx.Done():
		s.queuedWriteBytes.Add(-frameSize)
		return false
	default:
	}
	select {
	case s.writes <- copyOfFrame:
		return true
	case <-s.ctx.Done():
		s.queuedWriteBytes.Add(-frameSize)
		return false
	default:
		s.queuedWriteBytes.Add(-frameSize)
		return false
	}
}

func (s *peerSession) Close(reason error) {
	if reason == nil {
		reason = net.ErrClosed
	}
	closed := false
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.conn.Close()
		s.lease.Release()
		s.client.peerConns.Add(-1)
		s.registry.RemoveIfSame(s.key, s)
		close(s.done)
		closed = true
	})
	// Internal teardown and done notification precede the hook. Keeping the
	// hook outside closeOnce lets it safely observe done or re-enter Close.
	if closed {
		s.hooks.closed(s, reason)
	}
}

func (s *peerSession) writeLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.writes:
			s.queuedWriteBytes.Add(-int64(len(frame)))
			if err := writeFull(s.conn, frame); err != nil {
				s.Close(fmt.Errorf("write peer session: %w", err))
				return
			}
		}
	}
}

func writeFull(conn net.Conn, frame []byte) error {
	for len(frame) > 0 {
		n, err := conn.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("write made no progress")
		}
		frame = frame[n:]
	}
	return nil
}

func (s *peerSession) readLoop() {
	for {
		frame, err := s.readFrame()
		if err != nil {
			s.Close(fmt.Errorf("read peer session: %w", err))
			return
		}
		if err := s.hooks.frame(s, frame); err != nil {
			s.Close(err)
			return
		}
	}
}

type sessionDeadlineReader struct {
	conn             net.Conn
	idleTimeout      time.Duration
	absoluteDeadline time.Time
}

func (r sessionDeadlineReader) Read(p []byte) (int, error) {
	deadline := time.Now().Add(r.idleTimeout)
	if !r.absoluteDeadline.IsZero() && r.absoluteDeadline.Before(deadline) {
		deadline = r.absoluteDeadline
	}
	if err := r.conn.SetReadDeadline(deadline); err != nil {
		return 0, err
	}
	return r.conn.Read(p)
}

func (s *peerSession) readFrame() (sessionFrame, error) {
	switch s.key.connType {
	case peer.ConnectionType:
		var readerConn io.Reader = s.conn
		if s.role == sessionRoleOrdinary {
			readerConn = sessionDeadlineReader{conn: s.conn, idleTimeout: s.client.cfg.peerIdleTimeout, absoluteDeadline: s.absoluteDeadline}
		}
		reader, _, code, err := peer.ReadLimited(peer.Code(0), readerConn, false, maxOrdinaryPeerFrameSize)
		if err != nil {
			return sessionFrame{}, err
		}
		wire, err := readBufferedFrame(reader)
		return sessionFrame{connType: s.key.connType, code: int(code), wire: wire}, err
	case distributed.ConnectionType:
		reader, _, code, err := distributed.ReadLimited(s.conn, maxDistributedFrameSize)
		if err != nil {
			return sessionFrame{}, err
		}
		wire, err := readBufferedFrame(reader)
		return sessionFrame{connType: s.key.connType, code: int(code), wire: wire}, err
	default:
		return sessionFrame{}, fmt.Errorf("unsupported session type %q", s.key.connType)
	}
}

func readBufferedFrame(reader io.Reader) ([]byte, error) {
	// Protocol readers return an unread bytes.Buffer. Reuse that storage rather
	// than copying every hostile frame a second time before dispatch.
	if buffered, ok := reader.(interface{ Bytes() []byte }); ok {
		wire := buffered.Bytes()
		if len(wire) == 0 {
			return nil, errors.New("empty peer frame")
		}
		return wire, nil
	}
	wire, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(wire) == 0 {
		return nil, errors.New("empty peer frame")
	}
	return wire, nil
}

// getOrEstablishSession coalesces direct establishment by logical peer key.
// It is private and opaque; public ConnectPeer remains caller-owned.
func (c *Client) getOrEstablishSession(ctx context.Context, target sessionTarget, initiator sessionInitiator, role sessionRole, generation uint64) (*peerSession, error) {
	key := sessionKey{username: target.username, connType: target.connType}
	if existing := c.sessions.Get(key); existing != nil {
		return existing, nil
	}

	c.establishMu.Lock()
	if flight := c.establishes[key]; flight != nil {
		c.establishMu.Unlock()
		select {
		case <-flight.done:
			return flight.session, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &sessionEstablishment{done: make(chan struct{})}
	c.establishes[key] = flight
	c.establishMu.Unlock()

	flight.session, flight.err = c.establishSession(ctx, target, initiator, role, generation)
	c.establishMu.Lock()
	if c.establishes[key] == flight {
		delete(c.establishes, key)
	}
	close(flight.done)
	c.establishMu.Unlock()
	return flight.session, flight.err
}

func (c *Client) establishSession(ctx context.Context, target sessionTarget, initiator sessionInitiator, role sessionRole, generation uint64) (*peerSession, error) {
	if target.connType != peer.ConnectionType && target.connType != distributed.ConnectionType {
		return nil, fmt.Errorf("unsupported session type %q", target.connType)
	}
	key := sessionKey{username: target.username, connType: target.connType}
	if existing := c.sessions.Get(key); existing != nil {
		return existing, nil
	}

	dialer := net.Dialer{Timeout: c.cfg.peerDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", target.address)
	if err != nil {
		return nil, fmt.Errorf("dial peer session %s at %s: %w", target.username, target.address, err)
	}
	if _, err := peer.Write(conn, &peer.PeerInit{Username: c.cfg.Username, ConnectionType: target.connType}, false); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write peer init to %s at %s: %w", target.username, target.address, err)
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	candidate := c.newSession(conn, key, initiator, role, generation, nil)
	winner := c.registerSession(candidate)
	if winner == nil {
		return nil, errNoServerConnection
	}
	if generation != 0 && winner.generation == generation && !c.isServerGenerationActive(generation) {
		winner.Close(errNoServerConnection)
		return nil, errNoServerConnection
	}
	return winner, nil
}
