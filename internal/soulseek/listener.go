package soulseek

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// acceptPeers accepts incoming peer connections on ln until it is closed
// (which Run does when ctx is cancelled or Run itself returns). Each
// accepted connection is handed to handlePeerConn in its own goroutine so a
// slow or malicious peer cannot stall other connections.
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
			continue
		}

		go c.handlePeerConn(conn)
	}
}

// handlePeerConn reads the first frame of a freshly accepted peer
// connection - a PeerInit (a peer connecting to us directly) or a
// PierceFirewall (a peer completing a connection we asked the server to
// relay, see peers.go's indirect ConnectPeer path) - bounded by
// peerInitTimeout so a connection that never sends anything cannot leak.
//
// Full handling of an established peer connection (queued transfers,
// uploads, shared file listings, ...) is not implemented yet; every path
// below logs and closes the connection. See #54.
func (c *Client) handlePeerConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

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
			if c.logger != nil {
				c.logger.Debug("deserialize pierce firewall", "remote", conn.RemoteAddr(), "err", err)
			}
			return
		}
		// Matching pf.Token against a pending indirect ConnectPeer attempt
		// and handing the connection off to it is completed in a follow-up
		// commit; every token is treated as unknown here.
		if c.logger != nil {
			c.logger.Warn("pierce firewall with unknown token", "remote", conn.RemoteAddr(), "token", pf.Token)
		}

	case peer.CodePeerInit:
		pi := &peer.PeerInit{}
		if err := pi.Deserialize(reader); err != nil {
			if c.logger != nil {
				c.logger.Debug("deserialize peer init", "remote", conn.RemoteAddr(), "err", err)
			}
			return
		}
		if c.logger != nil {
			c.logger.Info("incoming peer connection", "user", pi.Username, "type", pi.ConnectionType, "remote", conn.RemoteAddr())
		}

	default:
		if c.logger != nil {
			c.logger.Debug("unexpected peer init code", "remote", conn.RemoteAddr(), "code", code)
		}
	}
}
