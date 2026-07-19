package server

import (
	"bytes"
	"testing"
)

func TestPingSerializeGoldenBytes(t *testing.T) {
	p := &Ping{}
	got, err := p.Serialize(&Ping{})
	if err != nil {
		t.Fatalf("Serialize: unexpected error: %v", err)
	}

	// size(4)=4 | code(4)=32 (CodePing), 8 bytes total.
	want := []byte{
		0x4, 0x0, 0x0, 0x0,
		0x20, 0x0, 0x0, 0x0,
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("Serialize() = %#v, want %#v", got, want)
	}
}
