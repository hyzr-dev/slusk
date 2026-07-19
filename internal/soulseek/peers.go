package soulseek

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
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

// newPeerConn wraps conn as an established peer connection, counting it in
// c.peerConns until it is closed.
func (c *Client) newPeerConn(conn net.Conn, username string, ct soul.ConnectionType) *PeerConn {
	return c.newPeerConnWithLease(conn, username, ct, nil)
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

// pendingResult is delivered to an in-flight indirect ConnectPeer attempt's
// done channel: either the peer connection completed via a matching
// PierceFirewall (conn), or a failure (err) - the server reporting
// CantConnectToPeer, or the server connection being lost entirely.
type pendingResult struct {
	conn *PeerConn
	err  error
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

	pc := c.newPeerConnWithLease(conn, attempt.username, attempt.ct, lease)
	attempt.done <- pendingResult{conn: pc}
	return true
}

// ConnectPeer establishes a connection to username for the given connection
// type (typically peer.ConnectionType, "P"). It first asks the server for
// the peer's current address and dials it directly; if that fails (e.g. the
// peer is behind a NAT/firewall with no port forwarding), it falls back to
// asking the server to relay a connection request to the peer, who then
// dials us back and completes the handshake with PierceFirewall. The whole
// attempt - address resolution, the direct dial, and the indirect fallback
// - is bounded by Config.establishTimeout.
func (c *Client) ConnectPeer(ctx context.Context, username string, ct soul.ConnectionType) (*PeerConn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.establishTimeout)
	defer cancel()

	addr, err := c.resolvePeerAddress(ctx, username)
	if err != nil {
		return nil, err
	}
	if addr.IP == nil || addr.IP.IsUnspecified() || addr.Port == 0 {
		return nil, fmt.Errorf("peer %s is offline (server reported no reachable address)", username)
	}

	directAddr := net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port))
	dialer := net.Dialer{Timeout: c.cfg.peerDialTimeout}
	directConn, directErr := dialer.DialContext(ctx, "tcp", directAddr)
	if directErr == nil {
		if _, err := peer.Write(directConn, &peer.PeerInit{Username: c.cfg.Username, ConnectionType: ct}, false); err != nil {
			_ = directConn.Close()
			return nil, fmt.Errorf("write peer init to %s at %s: %w", username, directAddr, err)
		}
		if c.logger != nil {
			c.logger.Info("peer connection established", "username", username, "type", ct, "path", "direct")
		}
		return c.newPeerConn(directConn, username, ct), nil
	}

	// Indirect (NAT-traversal) fallback: ask the server to relay a
	// ConnectToPeer request to the peer, who dials us back and completes
	// with PierceFirewall (see handlePeerConn in listener.go).
	token, attempt := c.registerPendingAttempt(username, ct)
	defer c.deregisterPendingAttempt(token, attempt)

	if err := sendToServer(c, &server.ConnectToPeer{Token: token, Username: username, Type: ct}); err != nil {
		return nil, fmt.Errorf("peer %s (token %d): direct dial to %s failed (%v) and requesting an indirect connection failed: %w",
			username, token, directAddr, directErr, err)
	}

	select {
	case res := <-attempt.done:
		if res.err != nil {
			return nil, fmt.Errorf("peer %s (token %d): direct dial to %s failed (%v) and %w",
				username, token, directAddr, directErr, res.err)
		}
		if c.logger != nil {
			c.logger.Info("peer connection established", "username", username, "type", ct, "path", "indirect", "direct_dial_err", directErr)
		}
		return res.conn, nil

	case <-ctx.Done():
		return nil, fmt.Errorf("peer %s (token %d): direct dial to %s failed (%v) and the indirect connection timed out: %w",
			username, token, directAddr, directErr, ctx.Err())
	}
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
