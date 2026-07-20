package server

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func putWireUint32(buf *bytes.Buffer, value uint32) {
	_ = binary.Write(buf, binary.LittleEndian, value)
}

func putWireString(buf *bytes.Buffer, value string) {
	putWireUint32(buf, uint32(len(value)))
	_, _ = buf.WriteString(value)
}

func packServerWire(payload *bytes.Buffer) []byte {
	var frame bytes.Buffer
	putWireUint32(&frame, uint32(payload.Len()))
	_, _ = frame.Write(payload.Bytes())
	return frame.Bytes()
}

func TestGetUserStatsDeserializeRawWireIncludesUnknownField(t *testing.T) {
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeGetUserStats))
	putWireString(&payload, "alice")
	for _, value := range []uint32{1200, 7, 0x11223344, 91, 13} {
		putWireUint32(&payload, value)
	}

	var message GetUserStats
	if err := message.Deserialize(bytes.NewReader(packServerWire(&payload))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if message.Username != "alice" || message.Speed != 1200 || message.Uploads != 7 || message.Unknown != 0x11223344 || message.Files != 91 || message.Directories != 13 {
		t.Fatalf("decoded stats = %+v", message)
	}
}

func TestWatchUserDeserializeRawWireIncludesUnknownField(t *testing.T) {
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeWatchUser))
	putWireString(&payload, "alice")
	_ = payload.WriteByte(1)
	for _, value := range []uint32{uint32(StatusOnline), 2400, 8, 0xaabbccdd, 101, 17} {
		putWireUint32(&payload, value)
	}
	putWireString(&payload, "SE")

	var message WatchUser
	if err := message.Deserialize(bytes.NewReader(packServerWire(&payload))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if message.Username != "alice" || !message.Exists || message.Status != StatusOnline || message.AverageSpeed != 2400 || message.UploadNumber != 8 || message.Unknown != 0xaabbccdd || message.Files != 101 || message.Directories != 17 || message.CountryCode != "SE" {
		t.Fatalf("decoded watch user = %+v", message)
	}
}
