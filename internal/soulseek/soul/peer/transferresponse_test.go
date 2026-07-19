package peer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
)

// buildTransferResponseFrame builds a raw TransferResponse payload (code,
// token, allowed, and optionally a reason string), without the leading
// 4-byte size prefix that MessageRead/Deserialize's first ReadUint32 call
// consumes and discards. reason == nil omits the trailing field entirely;
// reason != nil writes it (possibly truncated via truncateReasonAt).
func buildTransferResponseFrame(t *testing.T, token soul.Token, allowed bool, reason []byte) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(0)); err != nil { // size placeholder, unread by Deserialize
		t.Fatalf("write size placeholder: %v", err)
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(CodeTransferResponse)); err != nil {
		t.Fatalf("write code: %v", err)
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(token)); err != nil {
		t.Fatalf("write token: %v", err)
	}
	allowedByte := byte(0)
	if allowed {
		allowedByte = 1
	}
	buf.WriteByte(allowedByte)
	if reason != nil {
		buf.Write(reason)
	}

	return buf.Bytes()
}

// packedReason returns the wire encoding of a reason string: a 4-byte
// little-endian length prefix followed by the string bytes.
func packedReason(t *testing.T, s string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(s))); err != nil {
		t.Fatalf("write reason length: %v", err)
	}
	buf.WriteString(s)
	return buf.Bytes()
}

func TestTransferResponseDeserializeReasonPresent(t *testing.T) {
	frame := buildTransferResponseFrame(t, soul.Token(7), false, packedReason(t, "Queued"))

	var r TransferResponse
	if err := r.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: unexpected error: %v", err)
	}
	if r.Token != soul.Token(7) {
		t.Errorf("Token = %d, want 7", r.Token)
	}
	if r.Allowed {
		t.Errorf("Allowed = true, want false")
	}
	if !errors.Is(r.Reason, ErrQueued) {
		t.Errorf("Reason = %v, want ErrQueued", r.Reason)
	}
}

func TestTransferResponseDeserializeReasonAbsent(t *testing.T) {
	// No trailing reason bytes at all: reading the reason's length prefix
	// hits a clean io.EOF right at the field boundary.
	frame := buildTransferResponseFrame(t, soul.Token(9), false, nil)

	var r TransferResponse
	if err := r.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: unexpected error for absent trailing reason: %v", err)
	}
	if r.Token != soul.Token(9) {
		t.Errorf("Token = %d, want 9", r.Token)
	}
	if r.Allowed {
		t.Errorf("Allowed = true, want false")
	}
	if r.Reason != nil {
		t.Errorf("Reason = %v, want nil", r.Reason)
	}
}

func TestTransferResponseDeserializeReasonTruncatedMidString(t *testing.T) {
	// Declare a reason string longer than what is actually provided: the
	// length prefix reads fine, but the body read is a partial read followed
	// by EOF (io.ErrUnexpectedEOF via io.ReadFull), not a clean boundary EOF,
	// so this must still be a hard error.
	full := packedReason(t, "Queued")
	truncated := full[:len(full)-2] // drop the last 2 bytes of the string body
	frame := buildTransferResponseFrame(t, soul.Token(3), false, truncated)

	var r TransferResponse
	err := r.Deserialize(bytes.NewReader(frame))
	if err == nil {
		t.Fatal("Deserialize: expected error for a truncated reason string, got nil")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want NOT a clean io.EOF (should be io.ErrUnexpectedEOF or similar)", err)
	}
}

func TestTransferResponseDeserializeAllowedNoReasonField(t *testing.T) {
	// Allowed transfers never carry a reason field at all; nothing beyond
	// Allowed should be read.
	frame := buildTransferResponseFrame(t, soul.Token(1), true, nil)

	var r TransferResponse
	if err := r.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: unexpected error: %v", err)
	}
	if !r.Allowed {
		t.Errorf("Allowed = false, want true")
	}
	if r.Reason != nil {
		t.Errorf("Reason = %v, want nil", r.Reason)
	}
}
