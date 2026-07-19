package soulseek

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// acceptPeerErrBackoff is how long acceptPeers pauses after a transient
// Accept error so a persistent descriptor failure cannot busy-spin.
const acceptPeerErrBackoff = 100 * time.Millisecond

func (c *Client) acquireInboundLease() *inboundLease {
	select {
	case c.inboundSlots <- struct{}{}:
		return &inboundLease{release: func() { <-c.inboundSlots }}
	default:
		return nil
	}
}

func (c *Client) trackHandshake(conn net.Conn) {
	c.handshakeMu.Lock()
	c.handshakeConns[conn] = struct{}{}
	c.handshakeMu.Unlock()
}

func (c *Client) untrackHandshake(conn net.Conn) {
	c.handshakeMu.Lock()
	delete(c.handshakeConns, conn)
	c.handshakeMu.Unlock()
}

func (c *Client) closeHandshakes() {
	c.handshakeMu.Lock()
	conns := make([]net.Conn, 0, len(c.handshakeConns))
	for conn := range c.handshakeConns {
		conns = append(conns, conn)
	}
	c.handshakeMu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// acceptPeers admits at most the client-wide cap. A permit is acquired before
// a handshake goroutine starts and remains attached to an internally retained
// inbound session (or caller-owned indirect PeerConn) until that owner closes.
func (c *Client) acceptPeers(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			if c.logger != nil {
				c.logger.Debug("accept peer connection", "err", err)
			}
			time.Sleep(acceptPeerErrBackoff)
			continue
		}

		lease := c.acquireInboundLease()
		if lease == nil {
			_ = conn.Close()
			continue
		}
		c.trackHandshake(conn)
		if !c.startTracked(func() { c.handlePeerConn(ctx, conn, lease) }) {
			c.untrackHandshake(conn)
			_ = conn.Close()
			lease.Release()
		}
	}
}

// handlePeerConn completes the init handshake before transferring the socket
// to exactly one owner. Unsupported types and unknown tokens are closed.
func (c *Client) handlePeerConn(ctx context.Context, conn net.Conn, lease *inboundLease) {
	claimed := false
	defer func() {
		c.untrackHandshake(conn)
		if !claimed {
			_ = conn.Close()
			lease.Release()
		}
	}()

	if err := conn.SetReadDeadline(time.Now().Add(c.cfg.peerInitTimeout)); err != nil {
		return
	}

	reader, _, code, err := peer.Read(peer.CodeInit(0), conn, false)
	if err != nil {
		if c.logger != nil {
			c.logger.Debug("read peer init frame", "remote", conn.RemoteAddr(), "err", err)
		}
		return
	}

	switch code {
	case peer.CodePierceFirewall:
		pf := &peer.PierceFirewall{}
		if err := pf.Deserialize(reader); err != nil {
			return
		}
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			return
		}
		if c.completePendingDial(pf.Token, conn, lease) {
			claimed = true
			return
		}
		if c.logger != nil {
			c.logger.Warn("pierce firewall with unknown token", "remote", conn.RemoteAddr(), "token", pf.Token)
		}

	case peer.CodePeerInit:
		pi := &peer.PeerInit{}
		if err := pi.Deserialize(reader); err != nil {
			return
		}
		if pi.ConnectionType != peer.ConnectionType && pi.ConnectionType != distributed.ConnectionType {
			return
		}
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			return
		}
		role := sessionRoleOrdinary
		var generation uint64
		if pi.ConnectionType == distributed.ConnectionType {
			role = sessionRoleChild
			c.mu.Lock()
			if c.serverConn != nil {
				generation = c.serverGeneration
			}
			c.mu.Unlock()
			if generation == 0 {
				return
			}
		}
		candidate := c.newSession(conn, sessionKey{username: pi.Username, connType: pi.ConnectionType}, sessionInitiatorRemote, role, generation, lease)
		winner := c.registerSession(candidate)
		claimed = true
		if pi.ConnectionType == distributed.ConnectionType && winner.generation == generation && !c.isServerGenerationActive(generation) {
			winner.Close(errNoServerConnection)
		}
		if c.logger != nil {
			c.logger.Info("incoming peer session", "username", pi.Username, "type", pi.ConnectionType, "remote", conn.RemoteAddr())
		}

	default:
		if c.logger != nil {
			c.logger.Debug("unexpected peer init code", "remote", conn.RemoteAddr(), "code", code)
		}
	}
}
