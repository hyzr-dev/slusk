package soulseek

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

type scriptedAcceptResult struct {
	conn net.Conn
	err  error
}

type scriptedPeerListener struct {
	calls   chan struct{}
	results chan scriptedAcceptResult
	closed  chan struct{}
	once    sync.Once
}

func newScriptedPeerListener() *scriptedPeerListener {
	return &scriptedPeerListener{
		calls:   make(chan struct{}),
		results: make(chan scriptedAcceptResult),
		closed:  make(chan struct{}),
	}
}

func (l *scriptedPeerListener) Accept() (net.Conn, error) {
	select {
	case l.calls <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}

	select {
	case result := <-l.results:
		return result.conn, result.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *scriptedPeerListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *scriptedPeerListener) Addr() net.Addr { return &net.TCPAddr{} }

func waitForAcceptCall(t *testing.T, l *scriptedPeerListener, timeout time.Duration) {
	t.Helper()
	select {
	case <-l.calls:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for listener Accept call")
	}
}

type controlledRetryWait struct {
	calls    chan time.Duration
	releases chan struct{}
}

func newControlledRetryWait() *controlledRetryWait {
	return &controlledRetryWait{
		calls:    make(chan time.Duration),
		releases: make(chan struct{}),
	}
}

func (w *controlledRetryWait) wait(ctx context.Context, delay time.Duration) bool {
	select {
	case w.calls <- delay:
	case <-ctx.Done():
		return false
	}

	select {
	case <-w.releases:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitForAcceptWithoutRetryWait(
	t *testing.T,
	l *scriptedPeerListener,
	wait *controlledRetryWait,
	timeout time.Duration,
) {
	t.Helper()
	select {
	case <-l.calls:
	case delay := <-wait.calls:
		t.Fatalf("retry wait called with %v before the next Accept", delay)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for listener Accept call")
	}
}

func TestAcceptPeerRetryDelay(t *testing.T) {
	want := []time.Duration{
		0,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second,
		time.Second,
	}
	for i, wantDelay := range want {
		consecutiveErrors := i + 1
		if got := acceptPeerRetryDelay(consecutiveErrors); got != wantDelay {
			t.Errorf("acceptPeerRetryDelay(%d) = %v, want %v", consecutiveErrors, got, wantDelay)
		}
	}
}

func TestAcceptPeersImmediateFirstRetryAndResetOnSuccess(t *testing.T) {
	const timeout = 5 * time.Second

	c := New(Config{}, nil)
	c.inboundSlots = make(chan struct{}, 1)
	occupiedLease := c.acquireInboundLease()
	if occupiedLease == nil {
		t.Fatal("failed to occupy inbound lease slot")
	}
	defer occupiedLease.Release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newScriptedPeerListener()
	defer ln.Close()
	wait := newControlledRetryWait()

	done := make(chan struct{})
	go func() {
		c.acceptPeersWithRetryWait(ctx, ln, wait.wait)
		close(done)
	}()

	waitForAcceptCall(t, ln, timeout)
	ln.results <- scriptedAcceptResult{err: errors.New("first transient accept error")}
	waitForAcceptWithoutRetryWait(t, ln, wait, timeout)

	ln.results <- scriptedAcceptResult{err: errors.New("second transient accept error")}
	select {
	case delay := <-wait.calls:
		if delay != 100*time.Millisecond {
			t.Fatalf("second-error retry wait = %v, want 100ms", delay)
		}
	case <-ln.calls:
		t.Fatal("second retry reached Accept without waiting")
	case <-time.After(timeout):
		t.Fatal("timed out waiting for second-error retry wait")
	}
	select {
	case <-ln.calls:
		t.Fatal("second retry reached Accept before wait was released")
	default:
	}
	wait.releases <- struct{}{}
	waitForAcceptCall(t, ln, timeout)

	accepted, peer := net.Pipe()
	defer peer.Close()
	ln.results <- scriptedAcceptResult{conn: accepted}
	waitForAcceptCall(t, ln, timeout)

	type readResult struct {
		n   int
		err error
	}
	peerRead := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 1)
		n, err := peer.Read(buf)
		peerRead <- readResult{n: n, err: err}
	}()
	select {
	case result := <-peerRead:
		if result.n != 0 || !errors.Is(result.err, io.EOF) {
			t.Fatalf("rejected peer read = (%d, %v), want (0, EOF)", result.n, result.err)
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for rejected peer connection to close")
	}

	ln.results <- scriptedAcceptResult{err: errors.New("isolated error after success")}
	waitForAcceptWithoutRetryWait(t, ln, wait, timeout)

	ln.results <- scriptedAcceptResult{err: net.ErrClosed}
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("acceptPeers did not return after net.ErrClosed")
	}
}

func TestWaitForAcceptPeerRetryCancellation(t *testing.T) {
	const timeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(started)
		result <- waitForAcceptPeerRetry(ctx, time.Hour)
	}()

	<-started
	cancel()
	select {
	case completed := <-result:
		if completed {
			t.Fatal("waitForAcceptPeerRetry completed after context cancellation")
		}
	case <-time.After(timeout):
		t.Fatal("waitForAcceptPeerRetry did not return after context cancellation")
	}
}

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
