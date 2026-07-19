package soulseek

import (
	"bytes"
	"errors"
	"testing"
	"time"

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
	if err := c.handleMessage(server.CodeGetPeerAddress, bytes.NewReader(frame)); err != nil {
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
	if err := c.handleMessage(server.CodeGetPeerAddress, bytes.NewReader(frame)); err != nil {
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
