package soulseek

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
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
}

// Close closes the underlying connection. It is safe to call multiple
// times; only the first call runs the accounting callback.
func (p *PeerConn) Close() error {
	p.closeOnce.Do(func() {
		if p.onClose != nil {
			p.onClose()
		}
	})
	return p.Conn.Close()
}

// newPeerConn wraps conn as an established peer connection, counting it in
// c.peerConns until it is closed.
func (c *Client) newPeerConn(conn net.Conn, username string, ct soul.ConnectionType) *PeerConn {
	c.peerConns.Add(1)
	return &PeerConn{
		Conn:     conn,
		Username: username,
		Type:     ct,
		onClose:  func() { c.peerConns.Add(-1) },
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
		return server.GetPeerAddress{}, fmt.Errorf("request address of %s: %w", username, err)
	}

	select {
	case res := <-waiter:
		if res.err != nil {
			return server.GetPeerAddress{}, fmt.Errorf("resolve address of %s: %w", username, res.err)
		}
		return res.msg, nil

	case <-ctx.Done():
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
	username string
	ct       soul.ConnectionType
	done     chan pendingResult // buffered 1
}

// registerPendingAttempt allocates a fresh token (regenerating on the
// astronomically unlikely collision) and registers a pending indirect
// connection attempt for it.
func (c *Client) registerPendingAttempt(username string, ct soul.ConnectionType) (soul.Token, *pendingAttempt) {
	attempt := &pendingAttempt{username: username, ct: ct, done: make(chan pendingResult, 1)}

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for {
		token := soul.NewToken()
		if _, exists := c.pending[token]; !exists {
			c.pending[token] = attempt
			return token, attempt
		}
	}
}

// deregisterPendingAttempt removes token's entry (only if it still points at
// attempt; a completion may have already replaced/removed it) and, either
// way, drains and closes any connection that was delivered into attempt.done
// in the race window between ConnectPeer's select giving up (ctx expiring)
// and a concurrent completePendingDial or handleCantConnectToPeer call.
// Without this, a PierceFirewall that wins that race would leak its
// connection open forever.
func (c *Client) deregisterPendingAttempt(token soul.Token, attempt *pendingAttempt) {
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
func (c *Client) completePendingDial(token soul.Token, conn net.Conn) bool {
	c.pendingMu.Lock()
	attempt, ok := c.pending[token]
	if ok {
		delete(c.pending, token)
	}
	c.pendingMu.Unlock()

	if !ok {
		return false
	}

	pc := c.newPeerConn(conn, attempt.username, attempt.ct)
	select {
	case attempt.done <- pendingResult{conn: pc}:
	default:
		// ConnectPeer's own deregisterPendingAttempt drain (see above) is
		// the intended consumer of this extremely unlikely case (the
		// buffered channel already holding a value should be impossible
		// since only one PierceFirewall can ever match a given token before
		// it is removed from the map); close defensively rather than leak.
		_ = pc.Close()
	}
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
			c.logger.Info("peer connection established", "user", username, "type", ct, "path", "direct")
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
			c.logger.Info("peer connection established", "user", username, "type", ct, "path", "indirect", "direct_dial_err", directErr)
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
// slow or unreachable peer cannot stall message handling.
func (c *Client) handleConnectToPeer(msg server.ConnectToPeer) {
	go func() {
		addr := net.JoinHostPort(msg.IP.String(), strconv.Itoa(msg.Port))
		dialer := net.Dialer{Timeout: c.cfg.peerDialTimeout}
		conn, err := dialer.Dial("tcp", addr)
		if err != nil {
			if c.logger != nil {
				c.logger.Debug("mirror connect-to-peer dial failed", "username", msg.Username, "token", msg.Token, "addr", addr, "err", err)
			}
			if sendErr := sendToServer(c, &server.CantConnectToPeer{Token: msg.Token, Username: msg.Username}); sendErr != nil && c.logger != nil {
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

		if c.logger != nil {
			c.logger.Info("peer connection established", "user", msg.Username, "type", msg.Type, "path", "indirect-inbound")
		}
		// Full handling of an established mirror connection (queued
		// transfers, uploads, ...) is not implemented yet; close it rather
		// than leak the socket. See #54.
		_ = conn.Close()
	}()
}

// handleCantConnectToPeer handles the server telling us a peer we asked it
// to relay a connection request to (via ConnectPeer's indirect fallback)
// reported it could not connect back. It fails the matching pending
// indirect-connection attempt, if one is still registered for the token.
func (c *Client) handleCantConnectToPeer(msg server.CantConnectToPeer) {
	c.pendingMu.Lock()
	attempt, ok := c.pending[msg.Token]
	if ok {
		delete(c.pending, msg.Token)
	}
	c.pendingMu.Unlock()

	if !ok {
		if c.logger != nil {
			c.logger.Debug("received cant-connect-to-peer for unknown token", "username", msg.Username, "token", msg.Token)
		}
		return
	}

	select {
	case attempt.done <- pendingResult{err: errPeerCantConnectBack}:
	default:
	}
}
