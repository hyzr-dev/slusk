package server

import (
	"bytes"
	"testing"
)

func TestUnwatchUserSerializeExactFrame(t *testing.T) {
	got, err := (&UnwatchUser{}).Serialize(&UnwatchUser{Username: "alice"})
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeUnwatchUser))
	putWireString(&payload, "alice")
	want := packServerWire(&payload)
	if !bytes.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}
