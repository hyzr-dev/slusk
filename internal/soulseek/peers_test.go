package soulseek

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

// buildGetPeerAddressFrame builds a raw server.GetPeerAddress payload (code,
// username, IP, port, obfuscated port), without the leading 4-byte size
// prefix that Deserialize's first ReadUint32 call consumes and discards.
func buildGetPeerAddressFrame(t *testing.T, username string, ip [4]byte, port, obfuscatedPort int) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	mustWrite(t, writeUint32(buf, 0)) // size placeholder, unread by Deserialize
	mustWrite(t, writeUint32(buf, uint32(server.CodeGetPeerAddress)))
	mustWrite(t, writeString(buf, username))
	buf.Write(ip[:])
	mustWrite(t, writeUint32(buf, uint32(port)))
	mustWrite(t, writeUint32(buf, uint32(obfuscatedPort)))

	return buf.Bytes()
}

func TestSendToServerWhenDisconnectedErrors(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())

	if err := sendToServer(c, &server.Ping{}); err == nil {
		t.Fatal("sendToServer: expected error when not connected to server, got nil")
	}
}

func TestClientGetPeerAddressDeliversToMultipleWaiters(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())

	waiter1 := c.registerAddrWaiter("alice")
	waiter2 := c.registerAddrWaiter("alice")

	frame := buildGetPeerAddressFrame(t, "alice", [4]byte{1, 2, 3, 4}, 2234, 0)
	if err := c.handleMessage(context.Background(), server.CodeGetPeerAddress, bytes.NewReader(frame)); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	for i, ch := range []chan addrResult{waiter1, waiter2} {
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("waiter %d: err = %v, want nil", i, res.err)
			}
			if res.msg.Username != "alice" {
				t.Fatalf("waiter %d: Username = %q, want alice", i, res.msg.Username)
			}
			if res.msg.Port != 2234 {
				t.Fatalf("waiter %d: Port = %d, want 2234", i, res.msg.Port)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d: did not receive a delivery", i)
		}
	}
}

func TestClientGetPeerAddressWithNoWaiterIsDropped(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())

	frame := buildGetPeerAddressFrame(t, "bob", [4]byte{1, 2, 3, 4}, 2234, 0)
	if err := c.handleMessage(context.Background(), server.CodeGetPeerAddress, bytes.NewReader(frame)); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
}

func TestFailAllAddrWaiters(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())

	waiter := c.registerAddrWaiter("alice")
	wantErr := errors.New("boom")
	c.failAllAddrWaiters(wantErr)

	select {
	case res := <-waiter:
		if !errors.Is(res.err, wantErr) {
			t.Fatalf("err = %v, want %v", res.err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not receive a failure delivery")
	}
}

// TestCompletePendingDialThenDeregisterDoesNotLeak is a regression test for
// the TOCTOU race fixed by making completePendingDial's map deletion and
// channel delivery atomic under pendingMu (see its doc comment). It
// simulates, deterministically, the interleaving that used to leak a
// connection: a dial attempt's ctx expires and its deferred
// deregisterPendingAttempt call runs strictly after a completePendingDial
// call (standing in for a concurrent, real PierceFirewall arrival) has
// already fully finished - deleted the token from c.pending and delivered
// into attempt.done.
//
// Before the fix, completePendingDial released pendingMu between the
// deletion and the send, so deregisterPendingAttempt's non-blocking drain
// could run in that window, see an empty channel, and give up; the delayed
// send then landed in a channel nobody would ever read from again, leaking
// the socket. Actually exercising that exact timing window is inherently
// racy, so this test instead directly drives the two calls in the
// problematic order - which is deterministic and exactly what the atomic
// delete-then-deliver protocol must make safe regardless of timing.
//
// completePendingDial delivers the raw socket (plus any inbound lease), not a
// wrapped, counted PeerConn - the receiver (ConnectPeer / connectPeerSession)
// wraps and counts it. So PeerConns stays 0 here throughout; what this test
// guards is that the drain closes the delivered socket rather than stranding
// it.
func TestCompletePendingDialThenDeregisterDoesNotLeak(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())

	token, attempt := c.registerPendingAttempt("friend", peer.ConnectionType)

	connA, connB := net.Pipe()
	defer connB.Close()

	if !c.completePendingDial(token, connA) {
		t.Fatal("completePendingDial: expected the token to match the pending attempt")
	}

	// Simulates the dial attempt's ctx already having expired and its deferred
	// cleanup running after the delivery above; it must drain and close the
	// connection rather than leave it stranded in attempt.done.
	c.deregisterPendingAttempt(token, attempt)

	if got := c.Status().PeerConns; got != 0 {
		t.Fatalf("PeerConns = %d, want 0 - the raw delivery is never counted until wrapped", got)
	}

	// The delivered connection must actually have been closed, not merely
	// uncounted: a read on the other end of the pipe should now fail
	// promptly instead of hanging.
	buf := make([]byte, 1)
	_ = connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := connB.Read(buf); err == nil {
		t.Fatal("expected the delivered connection to have been closed")
	}
}
