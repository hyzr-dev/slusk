package soulseek

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
)

// waitForSession polls the registry until a session for key appears or the
// deadline passes.
func waitForSession(t *testing.T, c *Client, key sessionKey, timeout time.Duration) *peerSession {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := c.sessions.Get(key); s != nil {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// TestGetOrConnectPeerSessionReusesExisting verifies that when a "P" session to
// the peer already exists (e.g. an inbound PeerInit from a prior search),
// getOrConnectPeerSession returns that exact session without asking the server
// to resolve the peer's address again - so search (#54) and downloads (#55)
// share the single connection the protocol allows per peer.
func TestGetOrConnectPeerSessionReusesExisting(t *testing.T) {
	c, addr := startConnectedClient(t, func(conn net.Conn) {
		// If getOrConnectPeerSession wrongly tried to resolve an address, it
		// would send a GetPeerAddress the test would see here; we simply drop
		// anything and assert reuse via the returned pointer below.
		_, _ = io.Copy(io.Discard, conn)
	})

	// Establish an inbound "P" session by dialing the client's listener.
	inbound, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial client listener: %v", err)
	}
	defer inbound.Close()
	if _, err := peer.Write(inbound, &peer.PeerInit{Username: "friend", ConnectionType: peer.ConnectionType}, false); err != nil {
		t.Fatalf("write peer init: %v", err)
	}

	key := sessionKey{username: "friend", connType: peer.ConnectionType}
	existing := waitForSession(t, c, key, 2*time.Second)
	if existing == nil {
		t.Fatal("inbound PeerInit was not retained as a session")
	}

	got, err := c.getOrConnectPeerSession(context.Background(), "friend")
	if err != nil {
		t.Fatalf("getOrConnectPeerSession: %v", err)
	}
	if got != existing {
		t.Errorf("getOrConnectPeerSession returned a new session %p, want the existing %p", got, existing)
	}
}

// TestGetOrConnectPeerSessionDirectEstablish verifies the direct-dial
// establishment path: with no existing session, getOrConnectPeerSession
// resolves the address, dials the peer, sends PeerInit, and registers a shared
// session; a second call returns the same session rather than dialing again.
func TestGetOrConnectPeerSessionDirectEstablish(t *testing.T) {
	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	peerInits := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := peerLn.Accept()
			if err != nil {
				return
			}
			reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
			if err != nil {
				_ = conn.Close()
				return
			}
			pi := &peer.PeerInit{}
			if err := pi.Deserialize(reader); err != nil {
				_ = conn.Close()
				return
			}
			peerInits <- struct{}{}
			// Keep the connection open so the session's read loop stays alive
			// for the duration of the test.
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()

	addrRequests := make(chan struct{}, 4)
	c, _ := startConnectedClient(t, func(conn net.Conn) {
		for {
			code, body, err := readRawFrame(conn)
			if err != nil {
				return
			}
			if code != uint32(server.CodeGetPeerAddress) {
				continue
			}
			username, err := parseGetPeerAddressRequest(body)
			if err != nil {
				t.Errorf("parse get peer address request: %v", err)
				return
			}
			addrRequests <- struct{}{}
			writeGetPeerAddressResponse(t, conn, username, peerAddr.IP, peerAddr.Port, 0)
		}
	})

	session, err := c.getOrConnectPeerSession(context.Background(), "friend")
	if err != nil {
		t.Fatalf("getOrConnectPeerSession: %v", err)
	}
	if session == nil {
		t.Fatal("getOrConnectPeerSession returned a nil session")
	}
	if session.key.username != "friend" || session.key.connType != peer.ConnectionType {
		t.Errorf("session key = %+v, want friend/P", session.key)
	}
	if session.initiator != sessionInitiatorLocal || session.role != sessionRoleOrdinary {
		t.Errorf("session metadata = initiator %v role %v, want local/ordinary", session.initiator, session.role)
	}
	if got := c.sessions.Get(sessionKey{username: "friend", connType: peer.ConnectionType}); got != session {
		t.Error("established session was not retained in the registry")
	}

	select {
	case <-peerInits:
	case <-time.After(2 * time.Second):
		t.Fatal("fake peer never saw a PeerInit frame")
	}

	// A second call reuses the registered session: no new dial, no new address
	// resolution.
	again, err := c.getOrConnectPeerSession(context.Background(), "friend")
	if err != nil {
		t.Fatalf("getOrConnectPeerSession (second call): %v", err)
	}
	if again != session {
		t.Errorf("second call returned a new session %p, want the reused %p", again, session)
	}
	select {
	case <-peerInits:
		t.Error("second call dialed the peer again instead of reusing the session")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestGetOrConnectPeerSessionIndirect verifies the indirect (NAT-traversal)
// establishment path yields a registered, shared session: the direct dial
// fails, the client relays a ConnectToPeer, the peer dials back with
// PierceFirewall, and the resulting socket becomes a registered "P" session.
func TestGetOrConnectPeerSessionIndirect(t *testing.T) {
	var listenAddr string
	c, addr := startConnectedClient(t, func(conn net.Conn) {
		code, body, err := readRawFrame(conn)
		if err != nil || code != uint32(server.CodeGetPeerAddress) {
			t.Logf("read get peer address request: code=%d err=%v", code, err)
			return
		}
		username, err := parseGetPeerAddressRequest(body)
		if err != nil {
			t.Errorf("parse get peer address request: %v", err)
			return
		}
		// Address nothing listens on: forces the direct dial to fail.
		deadLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("listen for dead port: %v", err)
			return
		}
		deadAddr := deadLn.Addr().(*net.TCPAddr)
		_ = deadLn.Close()
		writeGetPeerAddressResponse(t, conn, username, deadAddr.IP, deadAddr.Port, 0)

		code, body, err = readRawFrame(conn)
		if err != nil || code != uint32(server.CodeConnectToPeer) {
			t.Logf("read connect to peer request: code=%d err=%v", code, err)
			return
		}
		token, _, _, err := parseConnectToPeerRequest(body)
		if err != nil {
			t.Errorf("parse connect to peer request: %v", err)
			return
		}

		// Play the peer: dial the client's listener back and pierce with the
		// relayed token, then keep the socket open for the session read loop.
		peerConn, err := net.Dial("tcp", listenAddr)
		if err != nil {
			t.Errorf("mock peer dial back: %v", err)
			return
		}
		if _, err := peer.Write(peerConn, &peer.PierceFirewall{Token: soul.Token(token)}, false); err != nil {
			t.Errorf("mock peer write pierce firewall: %v", err)
			return
		}
		go func() { _, _ = io.Copy(io.Discard, peerConn) }()
		_, _ = io.Copy(io.Discard, conn)
	})
	listenAddr = addr

	session, err := c.getOrConnectPeerSession(context.Background(), "friend")
	if err != nil {
		t.Fatalf("getOrConnectPeerSession: %v", err)
	}
	if session == nil {
		t.Fatal("getOrConnectPeerSession returned a nil session over the indirect path")
	}
	if session.initiator != sessionInitiatorLocal {
		t.Errorf("session initiator = %v, want local (we initiated the indirect dance)", session.initiator)
	}
	if got := c.sessions.Get(sessionKey{username: "friend", connType: peer.ConnectionType}); got != session {
		t.Error("indirect session was not retained in the registry")
	}
}
