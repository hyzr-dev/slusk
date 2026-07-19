package soulseek

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

func startSessionLifecycle(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.beginLifecycle(ctx); err != nil {
		t.Fatalf("beginLifecycle: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		c.stopLifecycle(ln)
	})
}

func TestSessionRegistryDuplicateAndIdentitySafeRemoval(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)

	a1, b1 := net.Pipe()
	defer b1.Close()
	first := c.newSession(a1, sessionKey{username: "friend", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	if got := c.registerSession(first); got != first {
		t.Fatal("first session did not win registration")
	}

	a2, b2 := net.Pipe()
	defer b2.Close()
	second := c.newSession(a2, first.key, sessionInitiatorLocal, sessionRoleOrdinary, 0, nil)
	if got := c.registerSession(second); got != first {
		t.Fatal("safe default did not retain existing duplicate")
	}
	select {
	case <-second.done:
	case <-time.After(time.Second):
		t.Fatal("duplicate newcomer was not closed")
	}

	// A stale loser/reader cannot remove the active winner.
	if c.sessions.RemoveIfSame(first.key, second) {
		t.Fatal("identity-unsafe removal removed a different session")
	}
	if got := c.sessions.Get(first.key); got != first {
		t.Fatal("active winner missing after stale removal")
	}
	first.Close(errors.New("test complete"))
}

func TestPrivateDirectSessionReusesOpaqueWinner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepts := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader, _, _, err := peer.Read(peer.CodeInit(0), conn, false)
		if err == nil {
			pi := &peer.PeerInit{}
			err = pi.Deserialize(reader)
		}
		if err == nil {
			accepts <- 1
		}
		_, _ = io.Copy(io.Discard, conn)
	}()

	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)
	target := sessionTarget{username: "friend", connType: distributed.ConnectionType, address: ln.Addr().String()}
	first, err := c.getOrEstablishSession(context.Background(), target, sessionInitiatorLocal, sessionRoleParent, 0)
	if err != nil {
		t.Fatalf("first establishment: %v", err)
	}
	second, err := c.getOrEstablishSession(context.Background(), target, sessionInitiatorLocal, sessionRoleParent, 0)
	if err != nil {
		t.Fatalf("second establishment: %v", err)
	}
	if first != second {
		t.Fatal("private establishment did not reuse the registry winner")
	}
	if first.conn == nil || first.initiator != sessionInitiatorLocal || first.generation != 0 {
		t.Fatal("opaque session metadata was not retained")
	}
	select {
	case n := <-accepts:
		if n != 1 {
			t.Fatalf("accepts = %d, want 1", n)
		}
	case <-time.After(time.Second):
		t.Fatal("peer did not receive the completed PeerInit handshake")
	}
	first.Close(errors.New("test complete"))
}

type serializedWriteConn struct {
	closed     chan struct{}
	closeOnce  sync.Once
	inWrite    atomic.Int32
	concurrent atomic.Bool
	writes     atomic.Int32
}

func newSerializedWriteConn() *serializedWriteConn {
	return &serializedWriteConn{closed: make(chan struct{})}
}
func (c *serializedWriteConn) Read([]byte) (int, error) { <-c.closed; return 0, net.ErrClosed }
func (c *serializedWriteConn) Write(p []byte) (int, error) {
	if c.inWrite.Add(1) != 1 {
		c.concurrent.Store(true)
	}
	time.Sleep(time.Millisecond)
	c.writes.Add(1)
	c.inWrite.Add(-1)
	return len(p), nil
}
func (c *serializedWriteConn) Close() error                   { c.closeOnce.Do(func() { close(c.closed) }); return nil }
func (*serializedWriteConn) LocalAddr() net.Addr              { return dummyAddr("local") }
func (*serializedWriteConn) RemoteAddr() net.Addr             { return dummyAddr("remote") }
func (*serializedWriteConn) SetDeadline(time.Time) error      { return nil }
func (*serializedWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*serializedWriteConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

func TestSessionSerializesBoundedWrites(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.sessionWriteQueue = 64
	startSessionLifecycle(t, c)
	conn := newSerializedWriteConn()
	s := c.newSession(conn, sessionKey{username: "friend", connType: distributed.ConnectionType}, sessionInitiatorLocal, sessionRoleParent, 1, nil)
	c.registerSession(s)

	for i := 0; i < 32; i++ {
		if !s.TrySend([]byte{byte(i)}) {
			t.Fatalf("TrySend(%d) unexpectedly rejected within queue bound", i)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for conn.writes.Load() != 32 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := conn.writes.Load(); got != 32 {
		t.Fatalf("writes = %d, want 32", got)
	}
	if conn.concurrent.Load() {
		t.Fatal("underlying connection observed concurrent writes")
	}
	s.Close(errors.New("test complete"))
	if s.TrySend([]byte("late")) {
		t.Fatal("TrySend accepted a frame after close")
	}
}

func TestOrdinaryPSessionRetiresWhenIdleAndReleasesLeaseOnce(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.peerIdleTimeout = 30 * time.Millisecond
	startSessionLifecycle(t, c)
	lease := c.acquireInboundLease()
	if lease == nil {
		t.Fatal("failed to acquire inbound lease")
	}
	a, b := net.Pipe()
	defer b.Close()
	s := c.newSession(a, sessionKey{username: "idle", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, lease)
	c.registerSession(s)

	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("ordinary P session did not retire after idle timeout")
	}
	if got := len(c.inboundSlots); got != 0 {
		t.Fatalf("inbound permits in use = %d, want 0", got)
	}
	s.Close(errors.New("second close"))
	if got := len(c.inboundSlots); got != 0 {
		t.Fatalf("permit changed after repeated close: %d", got)
	}
}

func TestTokenAllocatorIdentitySafeRelease(t *testing.T) {
	a := newTokenAllocator()
	owner := &struct{ name string }{"pending"}
	token, release := a.Reserve(owner)
	if _, ok := a.owners[token]; !ok {
		t.Fatal("token was not reserved")
	}
	release()
	release()
	if _, ok := a.owners[token]; ok {
		t.Fatal("idempotent release left token reserved")
	}
}
