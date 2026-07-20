package soulseek

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

// --- wire-format helpers, built directly against encoding/binary rather
// than the vendored internal helper package, since that package is
// restricted to import paths rooted under .../soul/. ---

func writeUint32(w io.Writer, v uint32) error {
	return binary.Write(w, binary.LittleEndian, v)
}

func writeString(w io.Writer, s string) error {
	if err := writeUint32(w, uint32(len(s))); err != nil {
		return err
	}
	_, err := w.Write([]byte(s))
	return err
}

func writeBool(w io.Writer, v bool) error {
	b := byte(0)
	if v {
		b = 1
	}
	_, err := w.Write([]byte{b})
	return err
}

// packFrame prefixes payload (which must start with the 4-byte code) with
// its own length, matching the Soulseek wire format.
func packFrame(payload []byte) []byte {
	buf := new(bytes.Buffer)
	_ = writeUint32(buf, uint32(len(payload)))
	buf.Write(payload)
	return buf.Bytes()
}

func loginSuccessFrame(t *testing.T) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeLogin)))
	mustWrite(t, writeBool(payload, true))
	mustWrite(t, writeString(payload, "welcome"))
	mustWrite(t, writeUint32(payload, 0x01020304))
	mustWrite(t, writeString(payload, "deadbeef"))
	return packFrame(payload.Bytes())
}

func loginFailureFrame(t *testing.T, errMessage string) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeLogin)))
	mustWrite(t, writeBool(payload, false))
	mustWrite(t, writeString(payload, errMessage))
	return packFrame(payload.Bytes())
}

func reloggedFrame(t *testing.T) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeRelogged)))
	return packFrame(payload.Bytes())
}

func serverUint32Frame(t *testing.T, code server.Code, value uint32) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(code)))
	mustWrite(t, writeUint32(payload, value))
	return packFrame(payload.Bytes())
}

func getUserStatsResponseFrame(t *testing.T, username string, speed uint32) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeGetUserStats)))
	mustWrite(t, writeString(payload, username))
	for _, value := range []uint32{speed, 3, 0x11223344, 50, 5} {
		mustWrite(t, writeUint32(payload, value))
	}
	return packFrame(payload.Bytes())
}

func watchUserResponseFrame(t *testing.T, username string, speed uint32) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeWatchUser)))
	mustWrite(t, writeString(payload, username))
	mustWrite(t, writeBool(payload, true))
	for _, value := range []uint32{uint32(server.StatusOnline), speed, 2, 0xaabbccdd, 40, 4} {
		mustWrite(t, writeUint32(payload, value))
	}
	mustWrite(t, writeString(payload, "SE"))
	return packFrame(payload.Bytes())
}

func embeddedSearchFrame(t *testing.T) []byte {
	t.Helper()
	search := &distributed.Search{Username: "searcher", Token: 1, Query: "query"}
	wire, err := search.Serialize(search)
	if err != nil {
		t.Fatalf("serialize distributed search: %v", err)
	}
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeEmbeddedMessage)))
	mustWrite(t, payload.WriteByte(byte(distributed.CodeSearch)))
	_, _ = payload.Write(wire[5:])
	return packFrame(payload.Bytes())
}

// oversizedFrame declares a size far beyond any sane message cap, with no
// body to back it.
func oversizedFrame() []byte {
	buf := new(bytes.Buffer)
	_ = writeUint32(buf, uint32(1<<30))
	return buf.Bytes()
}

func mustWrite(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("build test frame: %v", err)
	}
}

// drainLoginRequest reads and discards the client's login request frame off
// the wire so the fake server can proceed to write its response.
func drainLoginRequest(conn net.Conn) error {
	var size uint32
	if err := binary.Read(conn, binary.LittleEndian, &size); err != nil {
		return fmt.Errorf("read size: %w", err)
	}
	buf := make([]byte, size)
	_, err := io.ReadFull(conn, buf)
	return err
}

// readFrameCode reads one raw frame off the wire and returns its code (the
// first 4 bytes of the frame's declared payload).
func readFrameCode(conn net.Conn) (uint32, error) {
	payload, err := readFramePayload(conn)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(payload[:4]), nil
}

func readFramePayload(conn net.Conn) ([]byte, error) {
	var size uint32
	if err := binary.Read(conn, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	if len(buf) < 4 {
		return nil, fmt.Errorf("frame too short for a code")
	}
	return buf, nil
}

func drainInitialTreeAdvertisements(conn net.Conn) error {
	want := []uint32{
		uint32(server.CodeHaveNoParent),
		uint32(server.CodeBranchRoot),
		uint32(server.CodeBranchLevel),
		uint32(server.CodeAcceptChildren),
		uint32(server.CodeGetUserStats),
	}
	for _, wantCode := range want {
		code, err := readFrameCode(conn)
		if err != nil {
			return err
		}
		if code != wantCode {
			return fmt.Errorf("initial tree code = %d, want %d", code, wantCode)
		}
	}
	return nil
}

// --- fake server harness ---

type fakeServer struct {
	ln      net.Listener
	accepts atomic.Int32
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &fakeServer{ln: ln}
}

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

// serve accepts connections in the background and hands each one to handle.
// handle runs in its own goroutine per connection; test assertions on
// behavior observed inside handle must use t.Errorf/t.Logf, not t.Fatalf,
// since FailNow may only be called from the test's own goroutine.
func (f *fakeServer) serve(t *testing.T, handle func(conn net.Conn)) {
	t.Helper()
	go func() {
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				return
			}
			f.accepts.Add(1)
			go handle(conn)
		}
	}()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForState(t *testing.T, c *Client, want ConnState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Status().State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state %q, last status: %+v", want, c.Status())
}

func TestClientLoginSuccess(t *testing.T) {
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
		// Keep the connection open; drain anything the client sends
		// (e.g. pings) until the test tears down.
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	waitForState(t, c, StateConnected, 2*time.Second)

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestClientRequestsOwnStatsAndEnablesChildrenFromRawResponse(t *testing.T) {
	srv := newFakeServer(t)
	observed := make(chan error, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			observed <- err
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			observed <- err
			return
		}
		if code, err := readFrameCode(conn); err != nil || code != uint32(server.CodeSetListenPort) {
			observed <- fmt.Errorf("set listen port: code=%d err=%v", code, err)
			return
		}
		for _, want := range []server.Code{server.CodeHaveNoParent, server.CodeBranchRoot, server.CodeBranchLevel, server.CodeAcceptChildren} {
			if code, err := readFrameCode(conn); err != nil || code != uint32(want) {
				observed <- fmt.Errorf("initial tree message: code=%d want=%d err=%v", code, want, err)
				return
			}
		}
		statsRequest, err := readFramePayload(conn)
		if err != nil {
			observed <- err
			return
		}
		if code := binary.LittleEndian.Uint32(statsRequest[:4]); code != uint32(server.CodeGetUserStats) {
			observed <- fmt.Errorf("stats request code=%d", code)
			return
		}
		requestReader := bytes.NewReader(statsRequest[4:])
		var usernameSize uint32
		if err := binary.Read(requestReader, binary.LittleEndian, &usernameSize); err != nil {
			observed <- err
			return
		}
		username := make([]byte, usernameSize)
		if _, err := io.ReadFull(requestReader, username); err != nil || string(username) != "me" || requestReader.Len() != 0 {
			observed <- fmt.Errorf("stats request username=%q trailing=%d err=%v", username, requestReader.Len(), err)
			return
		}

		for _, frame := range [][]byte{
			serverUint32Frame(t, server.CodeParentMinSpeed, 100),
			serverUint32Frame(t, server.CodeParentSpeedRatio, 2),
			getUserStatsResponseFrame(t, "me", 200),
			embeddedSearchFrame(t),
		} {
			if _, err := conn.Write(frame); err != nil {
				observed <- err
				return
			}
		}
		for {
			payload, err := readFramePayload(conn)
			if err != nil {
				observed <- err
				return
			}
			if binary.LittleEndian.Uint32(payload[:4]) == uint32(server.CodeAcceptChildren) && len(payload) == 5 && payload[4] == 1 {
				observed <- nil
				_, _ = io.Copy(io.Discard, conn)
				return
			}
		}
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p"}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	select {
	case err := <-observed:
		if err != nil {
			t.Fatalf("startup/stats exchange: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AcceptChildren(true) from raw stats response")
	}
	c.tree.mu.Lock()
	capacity, uploadKnown := c.tree.capacity, c.tree.uploadKnown
	c.tree.mu.Unlock()
	if capacity != 1 || !uploadKnown {
		t.Fatalf("tree capacity=%d uploadKnown=%v, want 1/true", capacity, uploadKnown)
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestClientValidRawWatchUserDoesNotDropServerConnection(t *testing.T) {
	srv := newFakeServer(t)
	pingSeen := make(chan error, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			pingSeen <- err
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			pingSeen <- err
			return
		}
		if code, err := readFrameCode(conn); err != nil || code != uint32(server.CodeSetListenPort) {
			pingSeen <- fmt.Errorf("set listen port: code=%d err=%v", code, err)
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			pingSeen <- err
			return
		}
		if _, err := conn.Write(watchUserResponseFrame(t, "me", 3456)); err != nil {
			pingSeen <- err
			return
		}
		code, err := readFrameCode(conn)
		if err != nil || code != uint32(server.CodePing) {
			pingSeen <- fmt.Errorf("post-watch frame code=%d err=%v", code, err)
			return
		}
		pingSeen <- nil
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p"}, testLogger())
	c.cfg.pingInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	select {
	case err := <-pingSeen:
		if err != nil {
			t.Fatalf("raw WatchUser exchange: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server connection did not survive valid raw WatchUser frame")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestClientInvalidPass(t *testing.T) {
	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginRequest(conn); err != nil {
			t.Logf("drain login request: %v", err)
			return
		}
		if _, err := conn.Write(loginFailureFrame(t, server.ErrInvalidPass.Error())); err != nil {
			t.Logf("write login failure: %v", err)
		}
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Run(ctx)
	if !errors.Is(err, server.ErrInvalidPass) {
		t.Fatalf("Run() = %v, want wrapping ErrInvalidPass", err)
	}
	if c.Status().State != StateFailed {
		t.Fatalf("Status().State = %q, want %q", c.Status().State, StateFailed)
	}

	// Give a would-be reconnect a moment to happen, then confirm it did not.
	time.Sleep(100 * time.Millisecond)
	if n := srv.accepts.Load(); n != 1 {
		t.Fatalf("accepts = %d, want exactly 1 (no reconnect after a terminal error)", n)
	}
}

func TestClientReconnectAfterServerClose(t *testing.T) {
	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		if err := drainLoginRequest(conn); err != nil {
			t.Logf("drain login request: %v", err)
			conn.Close()
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			t.Logf("write login success: %v", err)
			conn.Close()
			return
		}
		if srv.accepts.Load() == 1 {
			// First connection: close immediately after login to force a
			// transient failure and reconnect.
			conn.Close()
			return
		}
		// Second connection onward: stay up.
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())
	c.cfg.backoffBase = 5 * time.Millisecond
	c.cfg.backoffCap = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && srv.accepts.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := srv.accepts.Load(); n < 2 {
		t.Fatalf("accepts = %d, want >= 2 (a reconnect after the first close)", n)
	}

	waitForState(t, c, StateConnected, 2*time.Second)
	if got := c.Status().ConsecutiveFailures; got != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0 after a successful reconnect", got)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestClientOversizedFrameReconnects(t *testing.T) {
	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		if err := drainLoginRequest(conn); err != nil {
			t.Logf("drain login request: %v", err)
			conn.Close()
			return
		}
		if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
			t.Logf("write login success: %v", err)
			conn.Close()
			return
		}
		if srv.accepts.Load() == 1 {
			// First connection: send a bogus oversized frame, then let the
			// connection sit; the client should give up reading and
			// reconnect on its own.
			_, _ = conn.Write(oversizedFrame())
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())
	c.cfg.backoffBase = 5 * time.Millisecond
	c.cfg.backoffCap = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && srv.accepts.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := srv.accepts.Load(); n < 2 {
		t.Fatalf("accepts = %d, want >= 2 (a reconnect after the oversized frame)", n)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestClientRelogged(t *testing.T) {
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
		if _, err := readFrameCode(conn); err != nil { // SetListenPort
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			return
		}
		if _, err := conn.Write(reloggedFrame(t)); err != nil {
			t.Logf("write relogged: %v", err)
		}
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Run(ctx)
	if !errors.Is(err, errRelogged) {
		t.Fatalf("Run() = %v, want wrapping errRelogged", err)
	}
	if c.Status().State != StateFailed {
		t.Fatalf("Status().State = %q, want %q", c.Status().State, StateFailed)
	}

	time.Sleep(100 * time.Millisecond)
	if n := srv.accepts.Load(); n != 1 {
		t.Fatalf("accepts = %d, want exactly 1 (no reconnect after Relogged)", n)
	}
}

func TestClientCtxCancelWhileConnected(t *testing.T) {
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
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	waitForState(t, c, StateConnected, 2*time.Second)

	start := time.Now()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v to return after ctx cancel, want prompt return", elapsed)
	}
}

func TestClientCtxCancelDuringBackoff(t *testing.T) {
	// Point at an address nothing is listening on so every dial fails,
	// forcing the client into its backoff sleep.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // close immediately: nothing will accept connections here

	c := New(Config{Address: addr, Username: "u", Password: "p"}, testLogger())
	c.cfg.dialTimeout = 200 * time.Millisecond
	c.cfg.backoffBase = time.Minute
	c.cfg.backoffCap = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Give the client time to fail its dial once and enter the backoff sleep.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return promptly during backoff after ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v to return during backoff, want prompt return", elapsed)
	}
}

func TestClientStalledHandshakeTimesOut(t *testing.T) {
	// The fake server accepts the TCP connection but never reads or writes
	// anything, simulating a server that is up but never speaks the
	// protocol. Without a deadline on the handshake, login's Read would
	// block forever; with one, the connect attempt fails and Run retries.
	srv := newFakeServer(t)
	srv.serve(t, func(conn net.Conn) {
		t.Cleanup(func() { _ = conn.Close() })
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())
	c.cfg.dialTimeout = 100 * time.Millisecond
	c.cfg.backoffBase = 20 * time.Millisecond
	c.cfg.backoffCap = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && srv.accepts.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := srv.accepts.Load(); n < 2 {
		t.Fatalf("accepts = %d, want >= 2 (handshake deadline expired and Run retried)", n)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestClientCtxCancelDuringStalledHandshake(t *testing.T) {
	// The fake server accepts the connection but never responds to the
	// login request. With a handshake deadline much longer than the test's
	// patience, ctx cancellation - not the deadline - must be what unblocks
	// the stalled Read promptly.
	srv := newFakeServer(t)
	accepted := make(chan struct{}, 1)
	srv.serve(t, func(conn net.Conn) {
		t.Cleanup(func() { _ = conn.Close() })
		accepted <- struct{}{}
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())
	c.cfg.dialTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never accepted a connection")
	}
	// Give the client a moment to be blocked in the handshake read.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return promptly during a stalled handshake after ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v to return during a stalled handshake, want prompt return", elapsed)
	}
}

func TestClientSendsPing(t *testing.T) {
	srv := newFakeServer(t)
	pingSeen := make(chan uint32, 1)
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
		// The client sends SetListenPort and its initial distributed-tree
		// advertisements after login, before the first ping.
		if _, err := readFrameCode(conn); err != nil {
			t.Logf("read set listen port frame after login: %v", err)
			return
		}
		if err := drainInitialTreeAdvertisements(conn); err != nil {
			t.Logf("read initial tree advertisements: %v", err)
			return
		}
		code, err := readFrameCode(conn)
		if err != nil {
			t.Logf("read frame after login: %v", err)
			return
		}
		pingSeen <- code
	})

	c := New(Config{Address: srv.addr(), Username: "u", Password: "p"}, testLogger())
	c.cfg.pingInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case code := <-pingSeen:
		if code != uint32(server.CodePing) {
			t.Fatalf("code = %d, want %d (CodePing)", code, server.CodePing)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a ping")
	}
}
