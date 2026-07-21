package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
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
	deadline = time.Now().Add(time.Second)
	for s.queuedWriteBytes.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.queuedWriteBytes.Load(); got != 0 {
		t.Fatalf("queued bytes after successful writes = %d, want 0", got)
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

func TestInboundOrdinaryPSessionHasAbsoluteLifetimeWhileActive(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.peerIdleTimeout = time.Second
	c.cfg.inboundPeerSessionLifetime = 60 * time.Millisecond
	startSessionLifecycle(t, c)
	lease := c.acquireInboundLease()
	if lease == nil {
		t.Fatal("failed to acquire inbound lease")
	}
	local, remote := net.Pipe()
	s := c.newSession(local, sessionKey{username: "active", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, lease)
	c.registerSession(s)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		response := &peer.FileSearchResponse{Username: "active", Token: 999}
		for {
			if _, err := peer.Write(remote, response, false); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("active inbound P session exceeded its absolute lifetime")
	}
	_ = remote.Close()
	<-writerDone
	if got := len(c.inboundSlots); got != 0 {
		t.Fatalf("inbound permits in use = %d, want 0", got)
	}
}

func TestSessionDeclaredSizeLimitsRejectBeforePayload(t *testing.T) {
	tests := []struct {
		name     string
		connType soul.ConnectionType
		size     uint32
	}{
		{name: "ordinary P", connType: peer.ConnectionType, size: maxOrdinaryPeerFrameSize + 1},
		{name: "distributed D", connType: distributed.ConnectionType, size: maxDistributedFrameSize + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
			startSessionLifecycle(t, c)
			local, remote := net.Pipe()
			defer remote.Close()
			s := c.newSession(local, sessionKey{username: "hostile", connType: tt.connType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
			defer s.Close(errors.New("test complete"))
			writeDone := make(chan error, 1)
			go func() { writeDone <- binary.Write(remote, binary.LittleEndian, tt.size) }()
			_, err := s.readFrame()
			if !errors.Is(err, soul.ErrMessageTooLarge) {
				t.Fatalf("readFrame error = %v, want ErrMessageTooLarge", err)
			}
			if err := <-writeDone; err != nil {
				t.Fatalf("write declaration: %v", err)
			}
		})
	}
}

func TestOrdinaryWriteQueueReleasesAccountingOnRefusalAndClose(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.sessionWriteQueue = 32
	startSessionLifecycle(t, c)
	local, remote := net.Pipe()
	defer remote.Close()
	s := c.newSession(local, sessionKey{username: "browser", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)

	large := make([]byte, int(maxOrdinaryPeerQueuedBytes/2)+1)
	if !s.TrySend(large) {
		t.Fatal("first legal large P frame was rejected")
	}
	for i := 0; i < cap(s.writes); i++ {
		if s.TrySend(large) {
			t.Fatalf("repeated large frame %d exceeded the ordinary-session byte budget", i)
		}
	}
	if got, want := len(s.writes), 1; got != want {
		t.Fatalf("retained frames = %d, want %d", got, want)
	}
	if got, want := s.queuedWriteBytes.Load(), int64(len(large)); got != want {
		t.Fatalf("queued bytes after refusals = %d, want %d", got, want)
	}

	s.Close(errors.New("test complete"))
	if got := s.queuedWriteBytes.Load(); got != 0 {
		t.Fatalf("queued bytes after close = %d, want 0", got)
	}
	if s.TrySend([]byte("small download frame")) {
		t.Fatal("closed session accepted a frame")
	}
	if got := s.queuedWriteBytes.Load(); got != 0 {
		t.Fatalf("refusal after close changed accounting to %d", got)
	}
}

func TestDistributedWriteQueueEnforcesByteBudget(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.sessionWriteQueue = 3
	startSessionLifecycle(t, c)
	local, remote := net.Pipe()
	defer remote.Close()
	s := c.newSession(local, sessionKey{username: "child", connType: distributed.ConnectionType}, sessionInitiatorRemote, sessionRoleChild, 0, nil)
	defer s.Close(errors.New("test complete"))
	frame := make([]byte, int(maxDistributedFrameSize)+4)
	if !s.TrySend(frame) || !s.TrySend(frame) || !s.TrySend(frame) {
		t.Fatal("legal queued D frames were rejected within byte budget")
	}
	if s.TrySend([]byte{0}) {
		t.Fatal("D queue accepted bytes beyond its protocol limit times queue depth")
	}
	if got, want := s.queuedWriteBytes.Load(), s.maxQueuedBytes; got != want {
		t.Fatalf("queued bytes = %d, want budget %d", got, want)
	}
}

func TestTokenAllocatorIdentitySafeRelease(t *testing.T) {
	a := newTokenAllocator()
	reservation := a.Reserve()
	if a.reservations[reservation.token] != reservation {
		t.Fatal("token was not reserved by identity")
	}
	reservation.Release()
	reservation.Release()
	if _, ok := a.reservations[reservation.token]; ok {
		t.Fatal("idempotent release left token reserved")
	}
}

func TestTokenAllocatorStaleReleaseDoesNotFreeReplacement(t *testing.T) {
	a := newTokenAllocator()
	stale := a.Reserve()
	replacement := &tokenReservation{allocator: a, token: stale.token}
	a.mu.Lock()
	a.reservations[stale.token] = replacement
	a.mu.Unlock()

	stale.Release()
	if a.reservations[stale.token] != replacement {
		t.Fatal("stale release freed the replacement reservation")
	}
	replacement.Release()
	if _, ok := a.reservations[stale.token]; ok {
		t.Fatal("replacement release left token reserved")
	}
}

type testSessionHooks struct {
	establishedFn func(*peerSession)
	frameFn       func(*peerSession, sessionFrame) error
	closedFn      func(*peerSession, error)
}

func (h testSessionHooks) established(s *peerSession) {
	if h.establishedFn != nil {
		h.establishedFn(s)
	}
}

func (h testSessionHooks) frame(s *peerSession, frame sessionFrame) error {
	if h.frameFn != nil {
		return h.frameFn(s, frame)
	}
	return nil
}

func (h testSessionHooks) closed(s *peerSession, err error) {
	if h.closedFn != nil {
		h.closedFn(s, err)
	}
}

func TestSessionClosedHookCanObserveDoneAndReenterClose(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)
	hookDone := make(chan struct{})
	var calls atomic.Int32
	c.sessionHooks = testSessionHooks{closedFn: func(s *peerSession, _ error) {
		<-s.done
		s.Close(errors.New("reentrant close"))
		calls.Add(1)
		close(hookDone)
	}}
	a, b := net.Pipe()
	defer b.Close()
	s := c.newSession(a, sessionKey{username: "friend", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	c.registerSession(s)

	go s.Close(errors.New("test close"))
	select {
	case <-hookDone:
	case <-time.After(time.Second):
		t.Fatal("closed hook deadlocked observing done or re-entering Close")
	}
	s.Close(errors.New("repeat close"))
	if got := calls.Load(); got != 1 {
		t.Fatalf("closed hook calls = %d, want 1", got)
	}
}

func TestSessionInboundFramesDeliverCompleteWire(t *testing.T) {
	tests := []struct {
		name     string
		connType soul.ConnectionType
		code     int
		write    func(io.Writer) ([]byte, error)
	}{
		{
			name:     "P",
			connType: peer.ConnectionType,
			code:     int(peer.CodeUserInfoRequest),
			write: func(w io.Writer) ([]byte, error) {
				var expected bytes.Buffer
				if _, err := peer.Write(&expected, &peer.UserInfoRequest{}, false); err != nil {
					return nil, err
				}
				_, err := w.Write(expected.Bytes())
				return expected.Bytes(), err
			},
		},
		{
			name:     "D",
			connType: distributed.ConnectionType,
			code:     int(distributed.CodeBranchLevel),
			write: func(w io.Writer) ([]byte, error) {
				var expected bytes.Buffer
				if _, err := distributed.Write(&expected, &distributed.BranchLevel{Level: 7}); err != nil {
					return nil, err
				}
				_, err := w.Write(expected.Bytes())
				return expected.Bytes(), err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
			startSessionLifecycle(t, c)
			frames := make(chan sessionFrame, 1)
			c.sessionHooks = testSessionHooks{frameFn: func(_ *peerSession, frame sessionFrame) error {
				frames <- frame
				return nil
			}}
			a, b := net.Pipe()
			defer b.Close()
			s := c.newSession(a, sessionKey{username: "friend", connType: tt.connType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
			c.registerSession(s)

			expected, err := tt.write(b)
			if err != nil {
				t.Fatalf("write frame: %v", err)
			}
			select {
			case frame := <-frames:
				if frame.connType != tt.connType || frame.code != tt.code || !bytes.Equal(frame.wire, expected) {
					t.Fatalf("frame = {type:%q code:%d wire:%x}, want {type:%q code:%d wire:%x}", frame.connType, frame.code, frame.wire, tt.connType, tt.code, expected)
				}
			case <-time.After(time.Second):
				t.Fatal("frame hook was not called")
			}
			s.Close(errors.New("test complete"))
		})
	}
}

func TestSessionFrameHookErrorClosesOnlySession(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)
	hookErr := errors.New("reject frame")
	closedErr := make(chan error, 1)
	c.sessionHooks = testSessionHooks{
		frameFn:  func(*peerSession, sessionFrame) error { return hookErr },
		closedFn: func(_ *peerSession, err error) { closedErr <- err },
	}
	a, b := net.Pipe()
	defer b.Close()
	key := sessionKey{username: "friend", connType: distributed.ConnectionType}
	s := c.newSession(a, key, sessionInitiatorRemote, sessionRoleChild, 0, nil)
	c.registerSession(s)
	if _, err := distributed.Write(b, &distributed.BranchLevel{Level: 3}); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	select {
	case err := <-closedErr:
		if !errors.Is(err, hookErr) {
			t.Fatalf("closed hook error = %v, want %v", err, hookErr)
		}
	case <-time.After(time.Second):
		t.Fatal("hook error did not close session")
	}
	if got := c.sessions.Get(key); got != nil {
		t.Fatal("hook-error session remained registered")
	}
}

func TestMirrorDAttachmentRejectedAfterGenerationTeardown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peerClosed := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			peerClosed <- err
			return
		}
		defer conn.Close()
		if _, _, _, err := peer.Read(peer.CodeInit(0), conn, false); err != nil {
			peerClosed <- err
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var one [1]byte
		_, err = conn.Read(one[:])
		peerClosed <- err
	}()

	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)
	const generation = 7

	// Order teardown after the old snapshot point but before the mirror D
	// attachment. Registration and generation invalidation share the registry
	// lock, so this late attachment must be rejected rather than escape cleanup.
	c.sessions.CloseGeneration(distributed.ConnectionType, generation, errNoServerConnection)
	addr := ln.Addr().(*net.TCPAddr)
	c.handleConnectToPeer(context.Background(), generation, server.ConnectToPeer{
		Token:    42,
		Username: "mirror",
		Type:     distributed.ConnectionType,
		IP:       addr.IP,
		Port:     addr.Port,
	})

	select {
	case err := <-peerClosed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("mirror peer close error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late mirror D attachment was not closed")
	}
	key := sessionKey{username: "mirror", connType: distributed.ConnectionType}
	if got := c.sessions.Get(key); got != nil {
		t.Fatal("late mirror D attachment survived in registry")
	}
}

// TestWriteFullWriteDeadlineUnblocksStalledPeer locks the rolling write
// deadline: a peer that never drains its receive buffer must not pin the writer
// goroutine forever. net.Pipe is synchronous/unbuffered, so a Write blocks until
// the other end reads — which it never does here — so only the deadline can
// unblock it.
func TestWriteFullWriteDeadlineUnblocksStalledPeer(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close() // deliberately never read from c2

	done := make(chan error, 1)
	go func() { done <- writeFull(c1, make([]byte, 1<<16), 50*time.Millisecond) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writeFull to a non-draining peer returned nil, want a timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeFull blocked well past its write deadline — writer goroutine pinned")
	}
}
