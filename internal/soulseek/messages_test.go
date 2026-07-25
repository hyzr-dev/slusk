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
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

// messageUserFrame builds a raw server.CodeMessageUser frame: uint32 id,
// uint32 timestamp, string username, string message, bool is_admin. See
// messages.go's handleIncomingPrivateMessage comment for why the first
// uint32 is a message id, not a user id.
func messageUserFrame(t *testing.T, id, timestamp uint32, username, body string, isAdmin bool) []byte {
	t.Helper()
	payload := new(bytes.Buffer)
	mustWrite(t, writeUint32(payload, uint32(server.CodeMessageUser)))
	mustWrite(t, writeUint32(payload, id))
	mustWrite(t, writeUint32(payload, timestamp))
	mustWrite(t, writeString(payload, username))
	mustWrite(t, writeString(payload, body))
	mustWrite(t, writeBool(payload, isAdmin))
	return packFrame(payload.Bytes())
}

// readWireString reads one length-prefixed string in the Soulseek wire
// format (uint32 length, then that many bytes) from r.
func readWireString(r io.Reader) (string, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return "", err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// drainLoginAndSteadyState drains the login handshake and the client's
// startup announcements (SetListenPort plus the initial distributed-tree
// advertisements), leaving conn positioned to observe whatever the test
// cares about next.
func drainLoginAndSteadyState(t *testing.T, conn net.Conn) error {
	t.Helper()
	if err := drainLoginRequest(conn); err != nil {
		return fmt.Errorf("drain login request: %w", err)
	}
	if _, err := conn.Write(loginSuccessFrame(t)); err != nil {
		return fmt.Errorf("write login success: %w", err)
	}
	if code, err := readFrameCode(conn); err != nil || code != uint32(server.CodeSetListenPort) {
		return fmt.Errorf("set listen port: code=%d err=%v", code, err)
	}
	return drainInitialTreeAdvertisements(conn)
}

// fakeSink is a MessageSink test double. block, when non-nil, is read from
// (or must be closed) before HandlePrivateMessage returns, letting tests
// hold a call open to observe that the read loop keeps dispatching other
// server messages while it does. calls fires (after storing the message)
// on every invocation, in call order, so a test can assert HandlePrivateMessage
// returned before some later externally-observed event.
type fakeSink struct {
	mu       sync.Mutex
	received []PrivateMessage
	err      error
	block    chan struct{}
	started  chan PrivateMessage
	calls    chan PrivateMessage
}

func newFakeSink() *fakeSink {
	return &fakeSink{
		started: make(chan PrivateMessage, 512),
		calls:   make(chan PrivateMessage, 512),
	}
}

func (s *fakeSink) HandlePrivateMessage(ctx context.Context, msg PrivateMessage) error {
	s.started <- msg
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.received = append(s.received, msg)
	err := s.err
	s.mu.Unlock()
	s.calls <- msg
	return err
}

func (s *fakeSink) messages() []PrivateMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PrivateMessage, len(s.received))
	copy(out, s.received)
	return out
}

// waitForCall waits for the next HandlePrivateMessage call to return.
func (s *fakeSink) waitForCall(t *testing.T, timeout time.Duration) PrivateMessage {
	t.Helper()
	select {
	case msg := <-s.calls:
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for sink to be called")
		return PrivateMessage{}
	}
}

// waitForStart waits for the next HandlePrivateMessage call to be entered,
// without waiting for it to return - the only signal available while block
// holds it open.
func (s *fakeSink) waitForStart(t *testing.T, timeout time.Duration) PrivateMessage {
	t.Helper()
	select {
	case msg := <-s.started:
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for sink to be entered")
		return PrivateMessage{}
	}
}

func TestIncomingPrivateMessagePersistsBeforeAck(t *testing.T) {
	srv := newFakeServer(t)
	result := make(chan error, 1)
	ackPayload := make(chan []byte, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		if _, err := conn.Write(messageUserFrame(t, 42, 1000, "alice", "hello", false)); err != nil {
			result <- err
			return
		}
		for {
			payload, err := readFramePayload(conn)
			if err != nil {
				result <- err
				return
			}
			if binary.LittleEndian.Uint32(payload[:4]) == uint32(server.CodeMessageAcked) {
				ackPayload <- payload
				result <- nil
				return
			}
		}
	})

	sink := newFakeSink()
	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", MessageSink: sink}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("server exchange: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ack")
	}

	payload := <-ackPayload
	if len(payload) != 8 {
		t.Fatalf("ack payload length = %d, want 8", len(payload))
	}
	if gotID := binary.LittleEndian.Uint32(payload[4:8]); gotID != 42 {
		t.Fatalf("ack message id = %d, want 42", gotID)
	}

	// The ack is only ever sent from runMessageWorker after
	// HandlePrivateMessage has already returned nil (see messages.go), so by
	// the time the ack frame was observed above the call must already be
	// recorded here - a non-blocking receive proves it, rather than assuming
	// the ordering.
	select {
	case msg := <-sink.calls:
		if msg.ServerMessageID != 42 || msg.Username != "alice" || msg.Body != "hello" {
			t.Fatalf("sink received %+v, want id=42 username=alice body=hello", msg)
		}
	default:
		t.Fatal("sink was not called before the ack was observed at the server")
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

func TestIncomingPrivateMessageNotAckedWhenSinkFails(t *testing.T) {
	srv := newFakeServer(t)
	result := make(chan error, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		if _, err := conn.Write(messageUserFrame(t, 7, 1000, "alice", "hello", false)); err != nil {
			result <- err
			return
		}
		// The sink always errors below, so the ack can never legitimately
		// arrive: whatever comes next must be the ping, proving the session
		// stayed up rather than that we got lucky with timing.
		code, err := readFrameCode(conn)
		if err != nil {
			result <- err
			return
		}
		if code == uint32(server.CodeMessageAcked) {
			result <- errors.New("received ack despite sink failure")
			return
		}
		if code != uint32(server.CodePing) {
			result <- fmt.Errorf("code = %d, want ping", code)
			return
		}
		result <- nil
		_, _ = io.Copy(io.Discard, conn)
	})

	sink := newFakeSink()
	sink.err = errors.New("persist failed")
	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", MessageSink: sink}, testLogger())
	c.cfg.pingInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ping after sink failure")
	}
	sink.waitForCall(t, 2*time.Second)

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

func TestIncomingPrivateMessageNotAckedWithoutSink(t *testing.T) {
	srv := newFakeServer(t)
	result := make(chan error, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		if _, err := conn.Write(messageUserFrame(t, 9, 1000, "alice", "hello", false)); err != nil {
			result <- err
			return
		}
		code, err := readFrameCode(conn)
		if err != nil {
			result <- err
			return
		}
		if code == uint32(server.CodeMessageAcked) {
			result <- errors.New("received ack with no sink configured")
			return
		}
		if code != uint32(server.CodePing) {
			result <- fmt.Errorf("code = %d, want ping", code)
			return
		}
		result <- nil
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p"}, testLogger())
	c.cfg.pingInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ping with no sink configured")
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

func TestIncomingPrivateMessageTruncatesOversizeBody(t *testing.T) {
	oversize := strings.Repeat("a", maxIncomingMessageBytes+100)
	srv := newFakeServer(t)
	result := make(chan error, 1)
	ackPayload := make(chan []byte, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		if _, err := conn.Write(messageUserFrame(t, 5, 1000, "alice", oversize, false)); err != nil {
			result <- err
			return
		}
		for {
			payload, err := readFramePayload(conn)
			if err != nil {
				result <- err
				return
			}
			if binary.LittleEndian.Uint32(payload[:4]) == uint32(server.CodeMessageAcked) {
				ackPayload <- payload
				result <- nil
				return
			}
		}
	})

	sink := newFakeSink()
	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", MessageSink: sink}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	msg := sink.waitForCall(t, 2*time.Second)
	if len(msg.Body) != maxIncomingMessageBytes {
		t.Fatalf("sink received body of %d bytes, want %d", len(msg.Body), maxIncomingMessageBytes)
	}
	if msg.Body != oversize[:maxIncomingMessageBytes] {
		t.Fatal("truncated body is not a prefix of the original")
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("server exchange: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ack of truncated message")
	}
	payload := <-ackPayload
	if gotID := binary.LittleEndian.Uint32(payload[4:8]); gotID != 5 {
		t.Fatalf("ack message id = %d, want 5", gotID)
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

// TestSlowSinkDoesNotBlockReadLoop is the regression guard for why
// runMessageWorker exists as a separate goroutine (see messages.go's package
// doc comment): if a future change moved the sink call back onto readLoop
// (e.g. inline in handleMessage's CodeMessageUser case), a slow sink would
// stall dispatch of every other server message, including the ping ticker's
// write path racing behind it - and this test would time out.
func TestSlowSinkDoesNotBlockReadLoop(t *testing.T) {
	srv := newFakeServer(t)
	result := make(chan error, 1)
	ackPayload := make(chan []byte, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		if _, err := conn.Write(messageUserFrame(t, 11, 1000, "alice", "hello", false)); err != nil {
			result <- err
			return
		}
		// While the sink is blocked, the read loop must still be dispatching:
		// the ping ticker keeps firing and its write must still arrive.
		code, err := readFrameCode(conn)
		if err != nil {
			result <- err
			return
		}
		if code != uint32(server.CodePing) {
			result <- fmt.Errorf("first frame after blocked sink: code=%d, want ping", code)
			return
		}
		result <- nil
		for {
			payload, err := readFramePayload(conn)
			if err != nil {
				return
			}
			if binary.LittleEndian.Uint32(payload[:4]) == uint32(server.CodeMessageAcked) {
				ackPayload <- payload
				return
			}
		}
	})

	sink := newFakeSink()
	sink.block = make(chan struct{})
	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", MessageSink: sink}, testLogger())
	c.cfg.pingInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	sink.waitForStart(t, 2*time.Second) // sink is now blocked inside HandlePrivateMessage

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("read loop did not keep dispatching while sink was blocked: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ping while sink was blocked")
	}

	close(sink.block)

	select {
	case payload := <-ackPayload:
		if gotID := binary.LittleEndian.Uint32(payload[4:8]); gotID != 11 {
			t.Fatalf("ack message id = %d, want 11", gotID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ack after releasing the sink")
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

// TestIncomingMessageDroppedWhenQueueFull holds the message worker blocked
// on its first call so the queue behind it fills, then verifies the surplus
// is dropped rather than blocking handleIncomingPrivateMessage (which would
// stall readLoop) or panicking.
func TestIncomingMessageDroppedWhenQueueFull(t *testing.T) {
	const totalSent = incomingMessageQueue + 10 // > worker's 1 in-flight + queue capacity
	const wantAccepted = incomingMessageQueue + 1

	srv := newFakeServer(t)
	result := make(chan error, 1)
	acks := make(chan uint32, wantAccepted)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		for id := uint32(1); id <= totalSent; id++ {
			if _, err := conn.Write(messageUserFrame(t, id, 1000, "alice", "hello", false)); err != nil {
				result <- fmt.Errorf("write message %d: %w", id, err)
				return
			}
		}
		// Sentinel: readLoop is a single serial reader on this connection, so
		// by the time it has processed this frame (observable client-side via
		// c.tree.uploadKnown, polled below), every messageUserFrame written
		// above has already been enqueued-or-dropped by
		// handleIncomingPrivateMessage.
		if _, err := conn.Write(getUserStatsResponseFrame(t, "me", 123)); err != nil {
			result <- fmt.Errorf("write sentinel: %w", err)
			return
		}
		result <- nil

		for {
			payload, err := readFramePayload(conn)
			if err != nil {
				return
			}
			if binary.LittleEndian.Uint32(payload[:4]) == uint32(server.CodeMessageAcked) {
				select {
				case acks <- binary.LittleEndian.Uint32(payload[4:8]):
				default:
				}
			}
		}
	})

	sink := newFakeSink()
	sink.block = make(chan struct{})
	c := New(Config{Address: srv.addr(), Username: "me", Password: "p", MessageSink: sink}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("server did not send all frames: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending frames")
	}

	sink.waitForStart(t, 2*time.Second) // worker holds message id=1 blocked

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.tree.mu.Lock()
		known := c.tree.uploadKnown
		c.tree.mu.Unlock()
		if known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for sentinel frame to be processed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Every send attempt beyond the worker's one in-flight slot plus the
	// bounded queue has, by now, either been enqueued or dropped: release the
	// worker and see exactly how many were accepted.
	close(sink.block)

	got := make(map[uint32]bool, wantAccepted)
	for len(got) < wantAccepted {
		select {
		case id := <-acks:
			got[id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out collecting acks: got %d, want %d", len(got), wantAccepted)
		}
	}
	if len(got) != wantAccepted {
		t.Fatalf("accepted %d messages, want %d (queue capacity %d + 1 in-flight)", len(got), wantAccepted, incomingMessageQueue)
	}
	for id := uint32(1); id <= wantAccepted; id++ {
		if !got[id] {
			t.Fatalf("message id %d was never acked, want it accepted (ids 1..%d)", id, wantAccepted)
		}
	}
	select {
	case id := <-acks:
		t.Fatalf("unexpected extra ack for id %d: only the first %d messages should have been accepted", id, wantAccepted)
	default:
	}
	msgs := sink.messages()
	if len(msgs) != wantAccepted {
		t.Fatalf("sink recorded %d messages, want %d", len(msgs), wantAccepted)
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

func TestSendPrivateMessageWritesToServer(t *testing.T) {
	srv := newFakeServer(t)
	result := make(chan error, 1)
	got := make(chan []byte, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		payload, err := readFramePayload(conn)
		if err != nil {
			result <- err
			return
		}
		got <- payload
		result <- nil
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p"}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	if err := c.SendPrivateMessage(context.Background(), "bob", "hello there"); err != nil {
		t.Fatalf("SendPrivateMessage: %v", err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MessageUser frame")
	}
	payload := <-got
	if code := binary.LittleEndian.Uint32(payload[:4]); code != uint32(server.CodeMessageUser) {
		t.Fatalf("code = %d, want CodeMessageUser", code)
	}
	reader := bytes.NewReader(payload[4:])
	username, err := readWireString(reader)
	if err != nil {
		t.Fatalf("read username: %v", err)
	}
	if username != "bob" {
		t.Fatalf("username = %q, want bob", username)
	}
	body, err := readWireString(reader)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if body != "hello there" {
		t.Fatalf("body = %q, want %q", body, "hello there")
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

func TestSendPrivateMessageRejectsInvalidInput(t *testing.T) {
	srv := newFakeServer(t)
	result := make(chan error, 1)
	got := make(chan []byte, 1)
	srv.serve(t, func(conn net.Conn) {
		defer conn.Close()
		if err := drainLoginAndSteadyState(t, conn); err != nil {
			result <- err
			return
		}
		// The first frame the server observes after startup must be the one
		// valid send issued below - proving none of the rejected calls wrote
		// anything to the wire.
		payload, err := readFramePayload(conn)
		if err != nil {
			result <- err
			return
		}
		got <- payload
		result <- nil
		_, _ = io.Copy(io.Discard, conn)
	})

	c := New(Config{Address: srv.addr(), Username: "me", Password: "p"}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	waitForState(t, c, StateConnected, 2*time.Second)

	overlong := strings.Repeat("a", maxPrivateMessageBytes+1)
	for _, tt := range []struct {
		name     string
		username string
		body     string
		wantErr  error
	}{
		{"empty username", "", "hello", ErrInvalidUsername},
		{"empty body", "bob", "", ErrEmptyMessage},
		{"whitespace-only body", "bob", "   \t\n", ErrEmptyMessage},
		{"body too long", "bob", overlong, ErrMessageTooLong},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := c.SendPrivateMessage(context.Background(), tt.username, tt.body)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SendPrivateMessage(%q, %.20q) = %v, want %v", tt.username, tt.body, err, tt.wantErr)
			}
		})
	}

	if err := c.SendPrivateMessage(context.Background(), "bob", "sentinel"); err != nil {
		t.Fatalf("SendPrivateMessage: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sentinel frame")
	}
	payload := <-got
	if code := binary.LittleEndian.Uint32(payload[:4]); code != uint32(server.CodeMessageUser) {
		t.Fatalf("code = %d, want CodeMessageUser", code)
	}
	reader := bytes.NewReader(payload[4:])
	username, err := readWireString(reader)
	if err != nil || username != "bob" {
		t.Fatalf("username = %q err=%v, want bob", username, err)
	}
	body, err := readWireString(reader)
	if err != nil || body != "sentinel" {
		t.Fatalf("body = %q err=%v, want sentinel", body, err)
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

// TestSendPrivateMessageWhenDisconnected mirrors
// TestSendToServerWhenDisconnectedErrors in peers_test.go: no server
// connection has ever been established, so the write must fail rather than
// panic.
func TestSendPrivateMessageWhenDisconnected(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	if err := c.SendPrivateMessage(context.Background(), "bob", "hello"); err == nil {
		t.Fatal("SendPrivateMessage: expected error when not connected to server, got nil")
	}
}
