package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
)

// --- generic raw-frame helpers shared by the ConnectPeer/mirror tests ---

// readRawFrame reads one frame off conn and splits it into its 4-byte code
// and the remaining body, without decoding the body further.
func readRawFrame(conn net.Conn) (code uint32, body []byte, err error) {
	var size uint32
	if err = binary.Read(conn, binary.LittleEndian, &size); err != nil {
		return 0, nil, err
	}
	buf := make([]byte, size)
	if _, err = io.ReadFull(conn, buf); err != nil {
		return 0, nil, err
	}
	if len(buf) < 4 {
		return 0, nil, fmt.Errorf("frame too short for a code")
	}
	return binary.LittleEndian.Uint32(buf[:4]), buf[4:], nil
}

// readLPString reads a length-prefixed string, Soulseek wire style.
func readLPString(r *bytes.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// wireIPBytes returns ip's 4 octets in the order internal.ReadIP expects on
// the wire: internal.ReadUint32 (little-endian) followed by
// binary.BigEndian.PutUint32 reconstructs the natural a.b.c.d order, which
// means the wire bytes are ip's octets reversed.
func wireIPBytes(ip net.IP) []byte {
	v4 := ip.To4()
	return []byte{v4[3], v4[2], v4[1], v4[0]}
}

// writeGetPeerAddressResponse writes a server.GetPeerAddress answer frame,
// as if from the server, to conn.
func writeGetPeerAddressResponse(t *testing.T, conn net.Conn, username string, ip net.IP, port, obfuscatedPort int) {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeGetPeerAddress)))
	mustWrite(t, writeString(payload, username))
	payload.Write(wireIPBytes(ip))
	mustWrite(t, writeUint32(payload, uint32(port)))
	mustWrite(t, writeUint32(payload, uint32(obfuscatedPort)))
	if _, err := conn.Write(packFrame(payload.Bytes())); err != nil {
		t.Fatalf("write get peer address response: %v", err)
	}
}

// writeCantConnectToPeer writes a server.CantConnectToPeer frame, as if from
// the server, to conn.
func writeCantConnectToPeer(t *testing.T, conn net.Conn, token uint32, username string) {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeCantConnectToPeer)))
	mustWrite(t, writeUint32(payload, token))
	mustWrite(t, writeString(payload, username))
	if _, err := conn.Write(packFrame(payload.Bytes())); err != nil {
		t.Fatalf("write cant connect to peer: %v", err)
	}
}

// parseConnectToPeerRequest parses the body (post-code) of a client-sent
// server.ConnectToPeer request: token, username, connection type.
func parseConnectToPeerRequest(body []byte) (token uint32, username, connType string, err error) {
	r := bytes.NewReader(body)
	if err = binary.Read(r, binary.LittleEndian, &token); err != nil {
		return
	}
	if username, err = readLPString(r); err != nil {
		return
	}
	connType, err = readLPString(r)
	return
}

// parseGetPeerAddressRequest parses the body (post-code) of a client-sent
// server.GetPeerAddress request: just a username.
func parseGetPeerAddressRequest(body []byte) (string, error) {
	return readLPString(bytes.NewReader(body))
}

// parseCantConnectToPeerRequest parses the body (post-code) of a
// client-sent server.CantConnectToPeer notification: token, username.
func parseCantConnectToPeerRequest(body []byte) (token uint32, username string, err error) {
	r := bytes.NewReader(body)
	if err = binary.Read(r, binary.LittleEndian, &token); err != nil {
		return
	}
	username, err = readLPString(r)
	return
}

// startConnectedClient dials handle (a fake soulseek server that has already
// completed the login handshake and, per test, answers GetPeerAddress/
// ConnectToPeer/CantConnectToPeer requests) and returns the Client once it
// reports StateConnected, along with its bound peer-listener address.
func startConnectedClient(t *testing.T, handle func(conn net.Conn)) (*Client, string) {
	t.Helper()

	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			t.Logf("drain login request: %v", err)
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			t.Logf("write login success: %v", err)
			return
		}
		// The client sends SetListenPort and its initial tree state after login.
		if _, _, err := readRawFrame(conn); err != nil {
			t.Logf("read set listen port: %v", err)
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			t.Logf("read initial tree advertisements: %v", err)
			return
		}
		handle(conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.peerDialTimeout = 200 * time.Millisecond
	c.cfg.establishTimeout = 2 * time.Second
	c.cfg.allowLoopbackPeerDial = true

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.Run(ctx) }()

	waitForState(t, c, StateConnected, 2*time.Second)
	return c, fmt.Sprintf("127.0.0.1:%d", c.listenPort)
}

func TestConnectPeerDirectSuccess(t *testing.T) {
	// A peer listening on its own loopback port, standing in for a directly
	// reachable peer.
	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	peerInitSeen := make(chan struct{ username, connType string }, 1)
	go func() {
		conn, err := peerLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read peer init: %v", err)
			return
		}
		pi := &peer.PeerInit{}
		if err := pi.Deserialize(reader); err != nil {
			t.Logf("fake peer: deserialize peer init: %v", err)
			return
		}
		peerInitSeen <- struct{ username, connType string }{pi.Username, string(pi.ConnectionType)}
	}()

	c, _ := startConnectedClient(t, func(conn net.Conn) {
		code, body, err := readRawFrame(conn)
		if err != nil {
			t.Logf("read frame: %v", err)
			return
		}
		if code != uint32(server.CodeGetPeerAddress) {
			t.Errorf("code = %d, want CodeGetPeerAddress", code)
			return
		}
		username, err := parseGetPeerAddressRequest(body)
		if err != nil {
			t.Errorf("parse get peer address request: %v", err)
			return
		}
		writeGetPeerAddressResponse(t, conn, username, peerAddr.IP, peerAddr.Port, 0)
		_, _ = io.Copy(io.Discard, conn)
	})

	conn, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
	if err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer conn.Close()
	if conn.Username != "friend" {
		t.Errorf("Username = %q, want friend", conn.Username)
	}

	select {
	case got := <-peerInitSeen:
		if got.username != "me" {
			t.Errorf("PeerInit.Username = %q, want me", got.username)
		}
		if got.connType != string(peer.ConnectionType) {
			t.Errorf("PeerInit.ConnectionType = %q, want %q", got.connType, peer.ConnectionType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake peer never saw a PeerInit frame")
	}
}

func TestConnectPeerIndirectSuccess(t *testing.T) {
	var listenAddr string
	c, addr := startConnectedClient(t, func(conn net.Conn) {
		code, body, err := readRawFrame(conn)
		if err != nil {
			t.Logf("read get peer address request: %v", err)
			return
		}
		if code != uint32(server.CodeGetPeerAddress) {
			t.Errorf("code = %d, want CodeGetPeerAddress", code)
			return
		}
		username, err := parseGetPeerAddressRequest(body)
		if err != nil {
			t.Errorf("parse get peer address request: %v", err)
			return
		}
		// Answer with an address nothing listens on, forcing the direct
		// dial to fail and the indirect fallback to kick in.
		deadLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("listen for dead port: %v", err)
			return
		}
		deadAddr := deadLn.Addr().(*net.TCPAddr)
		_ = deadLn.Close()
		writeGetPeerAddressResponse(t, conn, username, deadAddr.IP, deadAddr.Port, 0)

		code, body, err = readRawFrame(conn)
		if err != nil {
			t.Logf("read connect to peer request: %v", err)
			return
		}
		if code != uint32(server.CodeConnectToPeer) {
			t.Errorf("code = %d, want CodeConnectToPeer", code)
			return
		}
		token, _, _, err := parseConnectToPeerRequest(body)
		if err != nil {
			t.Errorf("parse connect to peer request: %v", err)
			return
		}

		// Play the peer's part: dial the client's own listener back and
		// complete with PierceFirewall carrying the relayed token.
		peerConn, err := net.Dial("tcp", listenAddr)
		if err != nil {
			t.Errorf("mock peer dial back: %v", err)
			return
		}
		defer peerConn.Close()
		if _, err := peer.Write(peerConn, &peer.PierceFirewall{Token: soul.Token(token)}, false); err != nil {
			t.Errorf("mock peer write pierce firewall: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	listenAddr = addr

	conn, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
	if err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer conn.Close()
	if conn.Username != "friend" {
		t.Errorf("Username = %q, want friend", conn.Username)
	}
}

func TestConnectPeerIndirectTokenMismatchTimesOut(t *testing.T) {
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
		deadLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("listen for dead port: %v", err)
			return
		}
		deadAddr := deadLn.Addr().(*net.TCPAddr)
		_ = deadLn.Close()
		writeGetPeerAddressResponse(t, conn, username, deadAddr.IP, deadAddr.Port, 0)

		if _, _, err := readRawFrame(conn); err != nil {
			t.Logf("read connect to peer request: %v", err)
			return
		}

		// Complete with a token that does not match any pending attempt.
		peerConn, err := net.Dial("tcp", listenAddr)
		if err != nil {
			t.Errorf("mock peer dial back: %v", err)
			return
		}
		if _, err := peer.Write(peerConn, &peer.PierceFirewall{Token: soul.Token(0xDEADBEEF)}, false); err != nil {
			t.Errorf("mock peer write pierce firewall: %v", err)
		}
		buf := make([]byte, 1)
		_ = peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := peerConn.Read(buf); err == nil {
			t.Errorf("expected the mismatched-token connection to be closed")
		}
		_ = peerConn.Close()
		_, _ = io.Copy(io.Discard, conn)
	})
	listenAddr = addr

	c.cfg.establishTimeout = 500 * time.Millisecond
	_, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
	if err == nil {
		t.Fatal("ConnectPeer: expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want it to mention a timeout", err)
	}
}

func TestConnectPeerCantConnectToPeerFails(t *testing.T) {
	c, _ := startConnectedClient(t, func(conn net.Conn) {
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
		writeCantConnectToPeer(t, conn, token, username)
		_, _ = io.Copy(io.Discard, conn)
	})

	_, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
	if err == nil {
		t.Fatal("ConnectPeer: expected an error, got nil")
	}
	if !errors.Is(err, errPeerCantConnectBack) {
		t.Errorf("err = %v, want it to wrap errPeerCantConnectBack", err)
	}
	if !strings.Contains(err.Error(), "direct dial") {
		t.Errorf("err = %v, want it to mention the direct dial failure too", err)
	}
}

func TestConnectPeerServerDisconnectFailsPending(t *testing.T) {
	srv := newFakeServer(t)
	var serverConn net.Conn
	connReady := make(chan struct{})
	srv.serve(t, func(conn net.Conn) {
		if err := drainLoginRequest(conn); err != nil {
			t.Logf("drain login request: %v", err)
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			t.Logf("write login success: %v", err)
			return
		}
		if _, _, err := readRawFrame(conn); err != nil { // SetListenPort
			t.Logf("read set listen port: %v", err)
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}
		serverConn = conn
		close(connReady)
		// Read (and drop) the GetPeerAddress request, then go silent and let
		// the test close the connection out from under ConnectPeer.
		_, _, _ = readRawFrame(conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.establishTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	<-connReady

	connectDone := make(chan error, 1)
	go func() {
		_, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
		connectDone <- err
	}()

	// Give ConnectPeer a moment to register its GetPeerAddress waiter, then
	// sever the server connection.
	time.Sleep(100 * time.Millisecond)
	_ = serverConn.Close()

	select {
	case err := <-connectDone:
		if err == nil {
			t.Fatal("ConnectPeer: expected an error after the server connection was lost, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectPeer did not return promptly after the server connection was lost")
	}
}

func TestConnectPeerServerDisconnectFailsPendingIndirectDance(t *testing.T) {
	// Regression test for the errNoServerConnection doc comment: previously
	// only GetPeerAddress waiters were failed when the server connection was
	// lost (failAllAddrWaiters); an indirect ConnectPeer dance already past
	// address resolution and waiting on a PierceFirewall/CantConnectToPeer
	// that can now never arrive would hang until ctx's own timeout instead
	// of failing promptly. See failAllPendingAttempts in peers.go.
	srv := newFakeServer(t)
	var serverConn net.Conn
	connectToPeerSeen := make(chan struct{})
	srv.serve(t, func(conn net.Conn) {
		if err := drainLoginRequest(conn); err != nil {
			t.Logf("drain login request: %v", err)
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			t.Logf("write login success: %v", err)
			return
		}
		if _, _, err := readRawFrame(conn); err != nil { // SetListenPort
			t.Logf("read set listen port: %v", err)
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}
		serverConn = conn

		code, body, err := readRawFrame(conn) // GetPeerAddress
		if err != nil || code != uint32(server.CodeGetPeerAddress) {
			t.Logf("read get peer address request: code=%d err=%v", code, err)
			return
		}
		username, err := parseGetPeerAddressRequest(body)
		if err != nil {
			t.Errorf("parse get peer address request: %v", err)
			return
		}
		// Answer with an address nothing listens on, forcing the direct
		// dial to fail and the indirect fallback to kick in.
		deadLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("listen for dead port: %v", err)
			return
		}
		deadAddr := deadLn.Addr().(*net.TCPAddr)
		_ = deadLn.Close()
		writeGetPeerAddressResponse(t, conn, username, deadAddr.IP, deadAddr.Port, 0)

		if _, _, err := readRawFrame(conn); err != nil { // ConnectToPeer
			t.Logf("read connect to peer request: %v", err)
			return
		}
		close(connectToPeerSeen)
		// Go silent and let the test sever the connection out from under the
		// now-pending indirect dance.
		_, _, _ = readRawFrame(conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.establishTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	connectDone := make(chan error, 1)
	go func() {
		_, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
		connectDone <- err
	}()

	select {
	case <-connectToPeerSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the ConnectToPeer request")
	}
	_ = serverConn.Close()

	select {
	case err := <-connectDone:
		if err == nil {
			t.Fatal("ConnectPeer: expected an error after the server connection was lost, got nil")
		}
		if !errors.Is(err, errNoServerConnection) {
			t.Errorf("err = %v, want it to wrap errNoServerConnection", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectPeer did not return promptly after the server connection was lost")
	}
}

func TestHandleConnectToPeerMirrorSuccess(t *testing.T) {
	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake peer: %v", err)
	}
	defer peerLn.Close()
	peerAddr := peerLn.Addr().(*net.TCPAddr)

	pierceSeen := make(chan soul.Token, 1)
	releasePeer := make(chan struct{})
	go func() {
		conn, err := peerLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
		if err != nil {
			t.Logf("fake peer: read: %v", err)
			return
		}
		pf := &peer.PierceFirewall{}
		if err := pf.Deserialize(reader); err != nil {
			t.Logf("fake peer: deserialize pierce firewall: %v", err)
			return
		}
		pierceSeen <- pf.Token
		<-releasePeer
	}()

	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			return
		}
		if _, _, err := readRawFrame(conn); err != nil { // SetListenPort
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}

		payload := new(bytes.Buffer)
		mustWrite(t, writeUint32(payload, uint32(server.CodeConnectToPeer)))
		mustWrite(t, writeString(payload, "friend"))
		mustWrite(t, writeString(payload, string(peer.ConnectionType)))
		payload.Write(wireIPBytes(peerAddr.IP))
		mustWrite(t, writeUint32(payload, uint32(peerAddr.Port)))
		mustWrite(t, writeUint32(payload, 4242)) // token
		mustWrite(t, writeBool(payload, false))  // privileged
		mustWrite(t, writeUint32(payload, 0))    // obfuscated port
		if _, err := conn.Write(packFrame(payload.Bytes())); err != nil {
			t.Logf("write connect to peer: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.allowLoopbackPeerDial = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	select {
	case token := <-pierceSeen:
		if token != soul.Token(4242) {
			t.Errorf("PierceFirewall.Token = %d, want 4242", token)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake peer never saw a PierceFirewall frame")
	}

	key := sessionKey{username: "friend", connType: peer.ConnectionType}
	deadline := time.Now().Add(2 * time.Second)
	for c.sessions.Get(key) == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	session := c.sessions.Get(key)
	if session == nil {
		t.Fatal("successful mirror connection was not retained in the private registry")
	}
	if session.initiator != sessionInitiatorRemote {
		t.Fatalf("mirror logical initiator = %v, want remote", session.initiator)
	}
	close(releasePeer)
}

func TestHandleConnectToPeerMirrorDialFailureSendsCantConnect(t *testing.T) {
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for dead port: %v", err)
	}
	deadAddr := deadLn.Addr().(*net.TCPAddr)
	_ = deadLn.Close()

	cantConnectSeen := make(chan struct {
		token    uint32
		username string
	}, 1)

	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			return
		}
		if _, _, err := readRawFrame(conn); err != nil { // SetListenPort
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}

		payload := new(bytes.Buffer)
		mustWrite(t, writeUint32(payload, uint32(server.CodeConnectToPeer)))
		mustWrite(t, writeString(payload, "friend"))
		mustWrite(t, writeString(payload, string(peer.ConnectionType)))
		payload.Write(wireIPBytes(deadAddr.IP))
		mustWrite(t, writeUint32(payload, uint32(deadAddr.Port)))
		mustWrite(t, writeUint32(payload, 7777)) // token
		mustWrite(t, writeBool(payload, false))
		mustWrite(t, writeUint32(payload, 0))
		if _, err := conn.Write(packFrame(payload.Bytes())); err != nil {
			t.Logf("write connect to peer: %v", err)
			return
		}

		code, body, err := readRawFrame(conn)
		if err != nil {
			t.Logf("read cant connect to peer: %v", err)
			return
		}
		if code != uint32(server.CodeCantConnectToPeer) {
			t.Errorf("code = %d, want CodeCantConnectToPeer", code)
			return
		}
		token, username, err := parseCantConnectToPeerRequest(body)
		if err != nil {
			t.Errorf("parse cant connect to peer: %v", err)
			return
		}
		cantConnectSeen <- struct {
			token    uint32
			username string
		}{token, username}
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.peerDialTimeout = 200 * time.Millisecond
	c.cfg.allowLoopbackPeerDial = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	select {
	case got := <-cantConnectSeen:
		if got.token != 7777 {
			t.Errorf("token = %d, want 7777", got.token)
		}
		if got.username != "friend" {
			t.Errorf("username = %q, want friend", got.username)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw a CantConnectToPeer frame")
	}
}

// TestHandleConnectToPeerBlockedAddressSendsCantConnect is a regression test
// for threat T12: the server may relay a ConnectToPeer with a link-local or
// other blocked address (see validatePeerDialAddr in addrguard.go).
// handleConnectToPeer must refuse to dial it and report CantConnectToPeer
// back to the server, exactly like a real dial failure, without ever
// attempting the connection. This uses a link-local address rather than
// loopback because (*Client).validateDialAddr carves out loopback for the
// test suite's own fake TCP peers (see its comment) - link-local has no such
// carve-out, so it still exercises the real block.
func TestHandleConnectToPeerBlockedAddressSendsCantConnect(t *testing.T) {
	cantConnectSeen := make(chan struct {
		token    uint32
		username string
	}, 1)

	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			return
		}
		if _, _, err := readRawFrame(conn); err != nil { // SetListenPort
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}

		payload := new(bytes.Buffer)
		mustWrite(t, writeUint32(payload, uint32(server.CodeConnectToPeer)))
		mustWrite(t, writeString(payload, "friend"))
		mustWrite(t, writeString(payload, string(peer.ConnectionType)))
		payload.Write(wireIPBytes(net.ParseIP("169.254.1.1")))
		mustWrite(t, writeUint32(payload, 12345)) // port
		mustWrite(t, writeUint32(payload, 9999))  // token
		mustWrite(t, writeBool(payload, false))
		mustWrite(t, writeUint32(payload, 0))
		if _, err := conn.Write(packFrame(payload.Bytes())); err != nil {
			t.Logf("write connect to peer: %v", err)
			return
		}

		code, body, err := readRawFrame(conn)
		if err != nil {
			t.Logf("read cant connect to peer: %v", err)
			return
		}
		if code != uint32(server.CodeCantConnectToPeer) {
			t.Errorf("code = %d, want CodeCantConnectToPeer", code)
			return
		}
		token, username, err := parseCantConnectToPeerRequest(body)
		if err != nil {
			t.Errorf("parse cant connect to peer: %v", err)
			return
		}
		cantConnectSeen <- struct {
			token    uint32
			username string
		}{token, username}
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.peerDialTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	select {
	case got := <-cantConnectSeen:
		if got.token != 9999 {
			t.Errorf("token = %d, want 9999", got.token)
		}
		if got.username != "friend" {
			t.Errorf("username = %q, want friend", got.username)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw a CantConnectToPeer frame for the blocked address")
	}

	// The blocked address must never have been retained as a session (no
	// dial was ever attempted).
	key := sessionKey{username: "friend", connType: peer.ConnectionType}
	if session := c.sessions.Get(key); session != nil {
		t.Fatalf("blocked address should not have produced a session, got %+v", session)
	}
}

// TestConnectPeerBlockedAddressFallsBackToIndirect is a regression test for
// threat T12: dialPeer must refuse to dial a server-supplied blocked address
// directly (via validatePeerDialAddr), but - unlike a server-supplied address
// with no reachable target at all - it must still fall back to the indirect
// NAT-traversal path exactly as it would for a failed direct dial, since the
// indirect path never dials the suspect address itself (the peer dials us
// back instead). This uses a link-local address rather than loopback because
// (*Client).validateDialAddr carves out loopback for the test suite's own
// fake TCP peers (see its comment) - link-local has no such carve-out, so it
// still exercises the real block. The direct dial to the blocked address must
// be skipped rather than attempted, so the whole call is asserted to
// complete well under peerDialTimeout.
func TestConnectPeerBlockedAddressFallsBackToIndirect(t *testing.T) {
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
		// Answer with a link-local address: the client must refuse to dial it
		// directly and fall back to the indirect dance instead.
		writeGetPeerAddressResponse(t, conn, username, net.ParseIP("169.254.1.1"), 12345, 0)

		code, body, err = readRawFrame(conn)
		if err != nil {
			t.Logf("read connect to peer request: %v", err)
			return
		}
		if code != uint32(server.CodeConnectToPeer) {
			t.Errorf("code = %d, want CodeConnectToPeer", code)
			return
		}
		token, _, _, err := parseConnectToPeerRequest(body)
		if err != nil {
			t.Errorf("parse connect to peer request: %v", err)
			return
		}

		// Play the peer's part: dial the client's own listener back and
		// complete with PierceFirewall carrying the relayed token.
		peerConn, err := net.Dial("tcp", listenAddr)
		if err != nil {
			t.Errorf("mock peer dial back: %v", err)
			return
		}
		defer peerConn.Close()
		if _, err := peer.Write(peerConn, &peer.PierceFirewall{Token: soul.Token(token)}, false); err != nil {
			t.Errorf("mock peer write pierce firewall: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	listenAddr = addr

	start := time.Now()
	conn, err := c.ConnectPeer(context.Background(), "friend", peer.ConnectionType)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer conn.Close()
	if conn.Username != "friend" {
		t.Errorf("Username = %q, want friend", conn.Username)
	}
	if elapsed >= c.cfg.peerDialTimeout {
		t.Errorf("ConnectPeer took %v, want well under peerDialTimeout %v - the direct dial to the blocked address should have been skipped, not attempted and timed out", elapsed, c.cfg.peerDialTimeout)
	}
}

func TestHandlePeerConnIncomingPeerInitRetainedAsSession(t *testing.T) {
	c, addr := startConnectedClient(t, func(conn net.Conn) {
		_, _ = io.Copy(io.Discard, conn)
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial client listener: %v", err)
	}
	defer conn.Close()

	if _, err := peer.Write(conn, &peer.PeerInit{Username: "friend", ConnectionType: peer.ConnectionType}, false); err != nil {
		t.Fatalf("write peer init: %v", err)
	}

	key := sessionKey{username: "friend", connType: peer.ConnectionType}
	deadline := time.Now().Add(2 * time.Second)
	var session *peerSession
	for time.Now().Before(deadline) {
		session = c.sessions.Get(key)
		if session != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if session == nil {
		t.Fatal("incoming PeerInit was not retained in the private registry")
	}
	if session.initiator != sessionInitiatorRemote || session.role != sessionRoleOrdinary {
		t.Fatalf("session metadata = initiator %v role %v, want remote/ordinary", session.initiator, session.role)
	}

	session.Close(errors.New("test complete"))
	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("read after session close: err = %v, want io.EOF", err)
	}
}
