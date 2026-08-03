package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
)

func TestCantConnectToPeerDeserializeServerTokenOnly(t *testing.T) {
	var frame bytes.Buffer
	for _, value := range []uint32{8, uint32(CodeCantConnectToPeer), 42} {
		if err := binary.Write(&frame, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}

	var got CantConnectToPeer
	if err := got.Deserialize(&frame); err != nil {
		t.Fatalf("Deserialize token-only server response: %v", err)
	}
	if got.Token != soul.Token(42) || got.Username != "" {
		t.Fatalf("Deserialize() = token %d username %q, want token 42 and empty username", got.Token, got.Username)
	}
}

func TestCantConnectToPeerDeserializeClientFormAndRejectsTruncation(t *testing.T) {
	wire, err := (&CantConnectToPeer{}).Serialize(&CantConnectToPeer{Token: 7, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var got CantConnectToPeer
	if err := got.Deserialize(bytes.NewReader(wire)); err != nil {
		t.Fatalf("Deserialize client form: %v", err)
	}
	if got.Token != soul.Token(7) || got.Username != "alice" {
		t.Fatalf("Deserialize() = token %d username %q", got.Token, got.Username)
	}

	truncated := wire[:len(wire)-1]
	if err := (&CantConnectToPeer{}).Deserialize(bytes.NewReader(truncated)); err == nil || err == io.EOF {
		t.Fatalf("truncated username error = %v, want a hard truncation error", err)
	}
}
