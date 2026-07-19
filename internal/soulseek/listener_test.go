package soulseek

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// readSetListenPortFrame reads one raw frame off conn and parses it as a
// server.SetListenPort payload (which has no vendored Deserialize, since the
// server never sends this message to us - it's client-to-server only).
func readSetListenPortFrame(conn net.Conn) (port, obfuscatedPort int, err error) {
	var size uint32
	if err = binary.Read(conn, binary.LittleEndian, &size); err != nil {
		return 0, 0, err
	}
	buf := make([]byte, size)
	if _, err = io.ReadFull(conn, buf); err != nil {
		return 0, 0, err
	}
	if len(buf) < 12 {
		return 0, 0, fmt.Errorf("frame too short for SetListenPort: %d bytes", len(buf))
	}
	port = int(binary.LittleEndian.Uint32(buf[4:8]))
	obfuscatedPort = int(binary.LittleEndian.Uint32(buf[8:12]))
	return port, obfuscatedPort, nil
}

func TestClientSendsSetListenPortAfterLogin(t *testing.T) {
	srv := newFakeServer(t)
	type result struct{ port, obfPort int }
	seen := make(chan result, 1)
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
		port, obfPort, err := readSetListenPortFrame(conn)
		if err != nil {
			t.Logf("read set listen port: %v", err)
			return
		}
		seen <- result{port, obfPort}
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitForState(t, c, StateConnected, 2*time.Second)
	wantPort := c.listenPort
	if wantPort == 0 {
		t.Fatal("listenPort = 0, want the actual bound port")
	}

	select {
	case r := <-seen:
		if r.port != wantPort {
			t.Fatalf("port = %d, want %d (actual bound port)", r.port, wantPort)
		}
		if r.obfPort != 0 {
			t.Fatalf("obfuscated port = %d, want 0", r.obfPort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SetListenPort")
	}
}

func TestPeerListenerToleratesGarbageAndRecovers(t *testing.T) {
	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.peerInitTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitForState(t, c, StateConnected, 2*time.Second)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", c.listenPort)

	// 1. Garbage bytes, then close.
	conn1, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = conn1.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	_ = conn1.Close()

	// 2. An oversized declared size with no body behind it.
	conn2, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	oversized := make([]byte, 4)
	binary.LittleEndian.PutUint32(oversized, 1<<30)
	_, _ = conn2.Write(oversized)
	_ = conn2.Close()

	// 3. Connect and close immediately without sending anything, relying on
	// peerInitTimeout (or the immediate read error from the closed conn) to
	// unblock the per-connection goroutine.
	conn3, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn3.Close()

	// Give the listener a moment to process/reject all three above.
	time.Sleep(500 * time.Millisecond)

	// 4. A subsequent, well-formed frame must still be accepted and
	// processed, proving the accept loop survived the above without wedging.
	conn4, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn4.Close()
	if _, err := peer.Write(conn4, &peer.PierceFirewall{Token: soul.Token(1)}, false); err != nil {
		t.Fatalf("write pierce firewall: %v", err)
	}
	// No indirect ConnectPeer attempt is pending in this test, so the
	// listener treats the token as unknown and closes the connection; a
	// read returning any error (rather than hanging) confirms it was
	// processed instead of leaking.
	buf := make([]byte, 1)
	_ = conn4.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn4.Read(buf); err == nil {
		t.Fatal("expected the connection to be closed after an unknown-token PierceFirewall")
	}
}
