package soulseek

import (
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

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

// handleConnectToPeer handles an incoming server.ConnectToPeer notification:
// another peer has asked the server to relay a connection request to us
// (the "indirect"/NAT-traversal path). Completed in a follow-up commit,
// which dials the peer back and replies with PierceFirewall.
func (c *Client) handleConnectToPeer(msg server.ConnectToPeer) {
	if c.logger != nil {
		c.logger.Debug("received connect-to-peer request", "username", msg.Username, "token", msg.Token, "type", msg.Type)
	}
}

// handleCantConnectToPeer handles the server telling us a peer we asked it
// to relay a connection request to reported it could not connect back.
// Completed in a follow-up commit, which fails the matching pending
// indirect-connection attempt.
func (c *Client) handleCantConnectToPeer(msg server.CantConnectToPeer) {
	if c.logger != nil {
		c.logger.Debug("received cant-connect-to-peer", "username", msg.Username, "token", msg.Token)
	}
}
