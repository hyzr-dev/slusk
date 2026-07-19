package soulseek

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

func TestInboundCapCountsHandshakesAndRetainedSessions(t *testing.T) {
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

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", ListenAddr: "127.0.0.1:0"}, testLogger())
	c.cfg.peerInitTimeout = 5 * time.Second
	c.inboundSlots = make(chan struct{}, 1) // unexported cap test seam
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)
	addr := net.JoinHostPort("127.0.0.1", fmtPort(c.listenPort))

	stalled, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	deadline := time.Now().Add(time.Second)
	for len(c.inboundSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(c.inboundSlots) != 1 {
		t.Fatal("stalled handshake did not consume the global permit")
	}

	overload, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	assertClosedPromptly(t, overload)

	_ = stalled.Close()
	deadline = time.Now().Add(time.Second)
	for len(c.inboundSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(c.inboundSlots) != 0 {
		t.Fatal("failed handshake did not release the permit")
	}

	retained, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	if _, err := peer.Write(retained, &peer.PeerInit{Username: "friend", ConnectionType: peer.ConnectionType}, false); err != nil {
		t.Fatal(err)
	}
	key := sessionKey{username: "friend", connType: peer.ConnectionType}
	deadline = time.Now().Add(time.Second)
	for c.sessions.Get(key) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s := c.sessions.Get(key)
	if s == nil || len(c.inboundSlots) != 1 {
		t.Fatal("retained inbound session did not retain its permit")
	}

	overload, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	assertClosedPromptly(t, overload)

	s.Close(errors.New("test release"))
	deadline = time.Now().Add(time.Second)
	for len(c.inboundSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(c.inboundSlots) != 0 {
		t.Fatal("session close did not release the permit")
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not join listener/session goroutines")
	}
}

func assertClosedPromptly(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-cap connection remained open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("over-cap connection was not closed immediately")
	}
}

func fmtPort(port int) string {
	// Avoid fmt.Sprintf in the test's hot setup while keeping address
	// construction explicit.
	return strconv.Itoa(port)
}
