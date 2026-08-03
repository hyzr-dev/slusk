package soulseek

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/file"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
)

// errPeerCantConnectBack is the failure delivered to a pending indirect
// ConnectPeer attempt when the server relays a CantConnectToPeer for its
// token: the peer received our ConnectToPeer relay but could not dial us
// back either.
var errPeerCantConnectBack = errors.New("peer reported it cannot connect back")

// PeerConn is an established connection to another Soulseek peer, either
// dialed directly by ConnectPeer or completed via the indirect
// (NAT-traversal) fallback. Closing it is safe to call more than once and
// decrements Client's peer-connection count exactly once.
type PeerConn struct {
	net.Conn
	Username string
	Type     soul.ConnectionType

	closeOnce sync.Once
	onClose   func()
	lease     *inboundLease
}

// Close closes the underlying connection. It is safe to call multiple
// times; only the first call runs the accounting callback.
func (p *PeerConn) Close() error {
	p.closeOnce.Do(func() {
		if p.onClose != nil {
			p.onClose()
		}
		p.lease.Release()
	})
	return p.Conn.Close()
}

func (c *Client) newPeerConnWithLease(conn net.Conn, username string, ct soul.ConnectionType, lease *inboundLease) *PeerConn {
	c.peerConns.Add(1)
	return &PeerConn{
		Conn: conn, Username: username, Type: ct, lease: lease,
		onClose: func() { c.peerConns.Add(-1) },
	}
}

// addrResult is delivered to a GetPeerAddress waiter registered in
// Client.pendingAddrs: either the server's answer (msg) or, if the server
// connection went away while waiting, err.
type addrResult struct {
	msg server.GetPeerAddress
	err error
}

// registerAddrWaiter registers a buffered channel to receive the next
// GetPeerAddress response for username. Multiple waiters for the same
// username may be registered concurrently; deliverAddr fans the response out
// to all of them.
func (c *Client) registerAddrWaiter(username string) chan addrResult {
	ch := make(chan addrResult, 1)

	c.addrMu.Lock()
	c.pendingAddrs[username] = append(c.pendingAddrs[username], ch)
	c.addrMu.Unlock()

	return ch
}

// deliverAddr fans msg out to every waiter currently registered for
// username, then clears them. A response with no registered waiter (the
// server answered a request we are no longer waiting on, or answered
// unprompted) is dropped with a debug log.
func (c *Client) deliverAddr(username string, res addrResult) {
	c.addrMu.Lock()
	waiters := c.pendingAddrs[username]
	delete(c.pendingAddrs, username)
	c.addrMu.Unlock()

	if len(waiters) == 0 {
		if c.logger != nil {
			c.logger.Debug("dropping GetPeerAddress response with no waiter", "username", username)
		}
		return
	}

	for _, ch := range waiters {
		select {
		case ch <- res:
		default:
			// Waiter channel is buffered (size 1) and already has its one
			// slot filled or was abandoned; do not block delivery to the
			// remaining waiters over one that is no longer listening.
		}
	}
}

// deregisterAddrWaiter removes ch from username's registered waiters, e.g.
// when resolvePeerAddress gives up on it (the request never reaching the
// server, or ctx expiring) without deliverAddr ever firing for it. Without
// this, an abandoned waiter would sit in pendingAddrs indefinitely - other
// waiters for the same username may still be pending, so the whole slice
// can't simply be cleared - until either an unrelated later GetPeerAddress
// response for the same username incidentally flushed it via deliverAddr,
// or the server connection is lost entirely.
func (c *Client) deregisterAddrWaiter(username string, ch chan addrResult) {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()

	waiters := c.pendingAddrs[username]
	for i, w := range waiters {
		if w == ch {
			c.pendingAddrs[username] = append(waiters[:i], waiters[i+1:]...)
			break
		}
	}
	if len(c.pendingAddrs[username]) == 0 {
		delete(c.pendingAddrs, username)
	}
}

// failAllAddrWaiters fails every currently registered GetPeerAddress waiter
// with err, e.g. when the server connection is lost while they are waiting.
func (c *Client) failAllAddrWaiters(err error) {
	c.addrMu.Lock()
	all := c.pendingAddrs
	c.pendingAddrs = make(map[string][]chan addrResult)
	c.addrMu.Unlock()

	for _, waiters := range all {
		for _, ch := range waiters {
			select {
			case ch <- addrResult{err: err}:
			default:
			}
		}
	}
}

// resolvePeerAddress asks the server for username's current address and
// waits for either the answer or ctx to expire.
func (c *Client) resolvePeerAddress(ctx context.Context, username string) (server.GetPeerAddress, error) {
	waiter := c.registerAddrWaiter(username)

	if err := sendToServer(c, &server.GetPeerAddress{Username: username}); err != nil {
		c.deregisterAddrWaiter(username, waiter)
		return server.GetPeerAddress{}, fmt.Errorf("request address of %s: %w", username, err)
	}

	select {
	case res := <-waiter:
		if res.err != nil {
			return server.GetPeerAddress{}, fmt.Errorf("resolve address of %s: %w", username, res.err)
		}
		return res.msg, nil

	case <-ctx.Done():
		c.deregisterAddrWaiter(username, waiter)
		return server.GetPeerAddress{}, fmt.Errorf("resolve address of %s: %w", username, ctx.Err())
	}
}

// pendingResult is delivered to an in-flight indirect dial attempt's done
// channel: either the peer connection completed via a matching PierceFirewall
// (the raw conn plus the inbound lease it holds - the peer dialed us back
// through our listener), or a failure (err) - the server reporting
// CantConnectToPeer, or the server connection being lost entirely. The raw
// socket (rather than a wrapped PeerConn) lets the receiver decide whether to
// wrap it as a caller-owned PeerConn (ConnectPeer) or a registered peerSession
// (getOrConnectPeerSession).
type pendingResult struct {
	conn  net.Conn
	lease *inboundLease
	err   error
}

// pendingAttempt is one in-flight indirect (NAT-traversal) connection
// attempt, registered in Client.pending under the token sent to the server
// in a ConnectToPeer request.
type pendingAttempt struct {
	username         string
	ct               soul.ConnectionType
	done             chan pendingResult // buffered 1
	tokenReservation *tokenReservation
}

// registerPendingAttempt allocates a fresh token (regenerating on the
// astronomically unlikely collision) and registers a pending indirect
// connection attempt for it.
func (c *Client) registerPendingAttempt(username string, ct soul.ConnectionType) (soul.Token, *pendingAttempt) {
	attempt := &pendingAttempt{username: username, ct: ct, done: make(chan pendingResult, 1)}
	reservation := c.tokens.Reserve()
	attempt.tokenReservation = reservation
	c.pendingMu.Lock()
	c.pending[reservation.token] = attempt
	c.pendingMu.Unlock()
	return reservation.token, attempt
}

// deregisterPendingAttempt removes token's entry (only if it still points at
// attempt; a completion may have already removed it - see below) and, either
// way, drains and closes any connection already sitting in attempt.done.
//
// completePendingDial and handleCantConnectToPeer both delete token from
// c.pending and deliver to attempt.done atomically, under pendingMu (see
// their comments). That makes the delete here and their delete-and-deliver
// mutually exclusive: either this call wins the race and removes the entry
// first, in which case neither of them will ever find the token and nothing
// is ever sent to done, or one of them already ran to completion first, in
// which case the value is already sitting in done (buffered, size 1) by the
// time this call's lock acquisition returns. Either way, by the time the
// select below runs there is nothing left to race: it either finds the
// channel empty (first case) or finds the value already delivered (second
// case) and closes it. Without this drain, the second case would otherwise
// leak the connection into a channel nobody reads from again.
func (c *Client) deregisterPendingAttempt(token soul.Token, attempt *pendingAttempt) {
	defer attempt.tokenReservation.Release()
	c.pendingMu.Lock()
	if cur, ok := c.pending[token]; ok && cur == attempt {
		delete(c.pending, token)
	}
	c.pendingMu.Unlock()

	select {
	case res := <-attempt.done:
		if res.conn != nil {
			_ = res.conn.Close()
			res.lease.Release()
		}
	default:
	}
}

// completePendingDial matches an incoming PierceFirewall's token against a
// pending indirect ConnectPeer attempt and, if found, delivers conn to it.
// It reports whether the token matched a still-registered attempt; the
// caller (handlePeerConn) closes conn itself when it did not.
//
// The map deletion and the channel send happen atomically under pendingMu so
// this can never race deregisterPendingAttempt (see its comment): if
// ConnectPeer's ctx expires concurrently, either deregisterPendingAttempt
// removes the token first (and this call then finds it already gone), or
// this call removes it first and deregisterPendingAttempt's own drain picks
// up the delivered result once it acquires the lock. There is no window
// where the token has been removed from the map but the delivery has not
// happened yet, which is what previously let a delivery land in the channel
// after deregisterPendingAttempt's drain had already given up, leaking the
// connection forever. This also guarantees the send below never blocks: at
// most one of completePendingDial / handleCantConnectToPeer ever observes
// the token present and gets to deliver into the buffered (size 1) channel.
func (c *Client) completePendingDial(token soul.Token, conn net.Conn, leases ...*inboundLease) bool {
	var lease *inboundLease
	if len(leases) != 0 {
		lease = leases[0]
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	attempt, ok := c.pending[token]
	if !ok {
		return false
	}
	delete(c.pending, token)

	attempt.done <- pendingResult{conn: conn, lease: lease}
	return true
}

// ConnectPeer establishes a connection to username for the given connection
// type (typically peer.ConnectionType, "P") and returns it as a caller-owned
// PeerConn. The connection strategy - direct dial then indirect NAT-traversal
// fallback, bounded by Config.establishTimeout - lives in dialPeer, shared with
// getOrConnectPeerSession.
func (c *Client) ConnectPeer(ctx context.Context, username string, ct soul.ConnectionType) (*PeerConn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.establishTimeout)
	defer cancel()

	conn, lease, err := c.dialPeer(ctx, username, ct)
	if err != nil {
		return nil, err
	}
	return c.newPeerConnWithLease(conn, username, ct, lease), nil
}

// dialPeer establishes a raw TCP connection to username for connection type ct.
// It first asks the server for the peer's current address and dials it
// directly; if that fails (e.g. the peer is behind a NAT/firewall with no port
// forwarding), it falls back to asking the server to relay a connection request
// to the peer, who then dials us back and completes the handshake with
// PierceFirewall. If the server-supplied address is present but blocked by
// validateDialAddr (threat T12: loopback/link-local/private), the direct dial
// is skipped without ever touching the address, and the same indirect
// fallback is used - only a nil/unspecified address or non-positive port,
// which leaves no fallback target either, is a hard failure. It returns the
// connected socket and, for the indirect path where the peer dialed us back
// through our listener, the inbound lease that socket holds (nil for the
// direct path); the caller owns both and must wrap them (as a PeerConn or a
// peerSession) or close and release them. The whole attempt is bounded by the
// ctx the caller supplies (ConnectPeer and getOrConnectPeerSession each apply
// establishTimeout).
func (c *Client) dialPeer(ctx context.Context, username string, ct soul.ConnectionType) (net.Conn, *inboundLease, error) {
	addr, err := c.resolvePeerAddress(ctx, username)
	if err != nil {
		return nil, nil, err
	}

	validateErr := c.validateDialAddr(addr.IP, addr.Port)
	var blocked *blockedAddrError
	if validateErr != nil && !errors.As(validateErr, &blocked) {
		// No reachable address at all (nil/unspecified IP or non-positive
		// port) - there is nothing to fall back to either, so this is a hard
		// failure.
		return nil, nil, fmt.Errorf("peer %s: %w", username, validateErr)
	}

	directAddr := net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port))
	var directErr error
	if validateErr != nil {
		// The address is present but policy-blocked (threat T12). Skip the
		// direct dial entirely - never touch the suspect address - and fall
		// through to the indirect NAT-traversal path exactly as if a direct
		// dial attempt had failed: the indirect path never dials the server-
		// supplied address itself, since the peer connects back to us.
		if c.logger != nil {
			c.logger.Debug("skipping direct dial to blocked peer address", "username", username, "addr", directAddr, "err", validateErr)
		}
		directErr = validateErr
	} else {
		dialer := net.Dialer{Timeout: c.cfg.peerDialTimeout}
		directConn, dErr := dialer.DialContext(ctx, "tcp", directAddr)
		if dErr == nil {
			if _, err := peer.Write(directConn, &peer.PeerInit{Username: c.cfg.Username, ConnectionType: ct}, false); err != nil {
				_ = directConn.Close()
				return nil, nil, fmt.Errorf("write peer init to %s at %s: %w", username, directAddr, err)
			}
			if c.logger != nil {
				c.logger.Debug("peer connection established", "username", username, "type", ct, "path", "direct")
			}
			return directConn, nil, nil
		}
		directErr = dErr
	}

	// Indirect (NAT-traversal) fallback: ask the server to relay a
	// ConnectToPeer request to the peer, who dials us back and completes
	// with PierceFirewall (see handlePeerConn in listener.go).
	token, attempt := c.registerPendingAttempt(username, ct)
	defer c.deregisterPendingAttempt(token, attempt)

	if err := sendToServer(c, &server.ConnectToPeer{Token: token, Username: username, Type: ct}); err != nil {
		return nil, nil, fmt.Errorf("peer %s (token %d): direct dial to %s failed (%v) and requesting an indirect connection failed: %w",
			username, token, directAddr, directErr, err)
	}

	select {
	case res := <-attempt.done:
		if res.err != nil {
			return nil, nil, fmt.Errorf("peer %s (token %d): direct dial to %s failed (%v) and %w",
				username, token, directAddr, directErr, res.err)
		}
		if c.logger != nil {
			c.logger.Debug("peer connection established", "username", username, "type", ct, "path", "indirect", "direct_dial_err", directErr)
		}
		return res.conn, res.lease, nil

	case <-ctx.Done():
		return nil, nil, fmt.Errorf("peer %s (token %d): direct dial to %s failed (%v) and the indirect connection timed out: %w",
			username, token, directAddr, directErr, ctx.Err())
	}
}

// getOrConnectPeerSession returns the registered ordinary "P" session for
// username, establishing one if none exists yet. Unlike getOrEstablishSession
// (which dials a known address for the distributed tree), it resolves the
// peer's address via the server and falls back to the indirect NAT-traversal
// dance, exactly like ConnectPeer - but the result is a shared, registered
// peerSession rather than a raw PeerConn, so search (#54) and downloads (#55)
// reuse the single "P" connection the protocol allows per peer. Concurrent
// callers for the same peer coalesce onto one establishment.
func (c *Client) getOrConnectPeerSession(ctx context.Context, username string) (*peerSession, error) {
	key := sessionKey{username: username, connType: peer.ConnectionType}
	return c.coalesceEstablish(ctx, key, func() (*peerSession, error) {
		return c.connectPeerSession(ctx, username)
	})
}

// connectPeerSession dials username for an ordinary "P" connection (direct or
// via the indirect fallback) and registers the resulting socket as a peer
// session. It mirrors establishSession but resolves the address itself; the
// establishment is bounded by establishTimeout. If a session for the peer was
// registered concurrently (e.g. an inbound PeerInit raced us), registerSession
// closes our losing candidate and returns the winner.
func (c *Client) connectPeerSession(ctx context.Context, username string) (*peerSession, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.establishTimeout)
	defer cancel()

	conn, lease, err := c.dialPeer(ctx, username, peer.ConnectionType)
	if err != nil {
		return nil, err
	}
	key := sessionKey{username: username, connType: peer.ConnectionType}
	candidate := c.newSession(conn, key, sessionInitiatorLocal, sessionRoleOrdinary, 0, lease)
	if winner := c.registerSession(candidate); winner != nil {
		return winner, nil
	}
	// registerSession only returns nil for a closed generation, which a
	// generation-0 ordinary "P" candidate can never hit; the guard mirrors
	// establishSession (where non-zero generations make it reachable) and is
	// kept defensively.
	return nil, errNoServerConnection
}

// handleConnectToPeer handles an incoming server.ConnectToPeer notification:
// another peer (unable to reach us directly, or attempting the reverse of
// our own indirect fallback) has asked the server to relay a connection
// request to us. We dial the peer back - on the plain (non-obfuscated) port
// only; ObfuscatedPort is deliberately unsupported here - and reply with
// PierceFirewall carrying the relayed token. Runs in its own goroutine so a
// slow or unreachable peer cannot stall message handling. ctx and generation
// belong to the exact central-server session that supplied the request.
func (c *Client) handleConnectToPeer(ctx context.Context, generation uint64, msg server.ConnectToPeer) {
	c.startTracked(func() {
		if err := c.validateDialAddr(msg.IP, msg.Port); err != nil {
			if c.logger != nil {
				c.logger.Warn("refusing connect-to-peer dial to blocked address", "username", msg.Username, "ip", msg.IP, "port", msg.Port, "err", err)
			}
			if sendErr := sendToServerGeneration(c, generation, &server.CantConnectToPeer{Token: msg.Token, Username: msg.Username}); sendErr != nil && c.logger != nil {
				c.logger.Debug("write cant connect to peer", "username", msg.Username, "token", msg.Token, "err", sendErr)
			}
			return
		}

		addr := net.JoinHostPort(msg.IP.String(), strconv.Itoa(msg.Port))
		dialer := net.Dialer{Timeout: c.cfg.peerDialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			if c.logger != nil {
				c.logger.Debug("mirror connect-to-peer dial failed", "username", msg.Username, "token", msg.Token, "addr", addr, "err", err)
			}
			if sendErr := sendToServerGeneration(c, generation, &server.CantConnectToPeer{Token: msg.Token, Username: msg.Username}); sendErr != nil && c.logger != nil {
				c.logger.Debug("write cant connect to peer", "username", msg.Username, "token", msg.Token, "err", sendErr)
			}
			return
		}

		if _, err := peer.Write(conn, &peer.PierceFirewall{Token: msg.Token}, false); err != nil {
			if c.logger != nil {
				c.logger.Debug("write pierce firewall", "username", msg.Username, "token", msg.Token, "err", err)
			}
			_ = conn.Close()
			return
		}

		if ctx.Err() != nil {
			_ = conn.Close()
			return
		}
		if msg.Type == file.ConnectionType {
			// A mirror-dialed F connection is never a peerSession, and the
			// outbound dial above consumed no inbound lease, so
			// handleInboundFileConn gets a nil one here (contrast the
			// accepted-socket path in listener.go, which passes its lease
			// through).
			if !c.startTracked(func() { c.handleInboundFileConn(ctx, conn, nil) }) {
				_ = conn.Close()
			}
			return
		}
		if msg.Type != peer.ConnectionType && msg.Type != distributed.ConnectionType {
			_ = conn.Close()
			return
		}
		role := sessionRoleOrdinary
		if msg.Type == distributed.ConnectionType {
			role = sessionRoleChild
		}
		candidate := c.newSession(conn, sessionKey{username: msg.Username, connType: msg.Type}, sessionInitiatorRemote, role, generation, nil)
		if c.registerSession(candidate) == nil {
			return
		}
		if c.logger != nil {
			c.logger.Debug("mirror peer session retained", "username", msg.Username, "type", msg.Type)
		}
	})
}

// handleCantConnectToPeer handles the server telling us a peer we asked it
// to relay a connection request to (via ConnectPeer's indirect fallback)
// reported it could not connect back. It fails the matching pending
// indirect-connection attempt, if one is still registered for the token.
//
// Like completePendingDial, the map deletion and channel send happen
// atomically under pendingMu so this can never race
// deregisterPendingAttempt (see its comment).
func (c *Client) handleCantConnectToPeer(msg server.CantConnectToPeer) {
	c.pendingMu.Lock()
	attempt, ok := c.pending[msg.Token]
	if ok {
		delete(c.pending, msg.Token)
		attempt.done <- pendingResult{err: errPeerCantConnectBack}
	}
	c.pendingMu.Unlock()

	if !ok && c.logger != nil {
		c.logger.Debug("received cant-connect-to-peer for unknown token", "username", msg.Username, "token", msg.Token)
	}
}

// failAllPendingAttempts fails every currently registered indirect
// (NAT-traversal) ConnectPeer attempt with err, e.g. when the server
// connection is lost while they are waiting on either a PierceFirewall
// completion or a CantConnectToPeer relay - neither of which can now ever
// arrive without the server. Clearing c.pending under pendingMu here rules
// out a concurrent completePendingDial or handleCantConnectToPeer also
// trying to deliver to the same attempt (see their comments): by the time
// this call's lock acquisition returns, every token has already been
// removed, so they will find nothing and neither will send.
func (c *Client) failAllPendingAttempts(err error) {
	c.pendingMu.Lock()
	all := c.pending
	c.pending = make(map[soul.Token]*pendingAttempt)
	c.pendingMu.Unlock()

	for _, attempt := range all {
		select {
		case attempt.done <- pendingResult{err: err}:
		default:
		}
	}
}
