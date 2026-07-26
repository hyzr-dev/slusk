package server

import (
	"bytes"
	"testing"
)

func TestGetUserStatusSerializeExactFrame(t *testing.T) {
	got, err := (&GetUserStatus{}).Serialize(&GetUserStatus{Username: "alice"})
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeGetUserStatus))
	putWireString(&payload, "alice")
	want := packServerWire(&payload)
	if !bytes.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}

func TestGetUserStatusDeserialize(t *testing.T) {
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeGetUserStatus))
	putWireString(&payload, "alice")
	putWireUint32(&payload, uint32(StatusAway))
	_ = payload.WriteByte(1)

	var got GetUserStatus
	if err := got.Deserialize(bytes.NewReader(packServerWire(&payload))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got.Username != "alice" || got.Status != StatusAway || !got.Privileged {
		t.Fatalf("decoded = %+v", got)
	}
}
