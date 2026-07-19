package internal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
)

// buildFrame builds a raw wire frame: a little-endian uint32 size followed by
// codeSize bytes of code (1 for init/distributed messages, 4 otherwise) and
// then payload. size is the declared size field, which callers may set to a
// value other than len(code)+len(payload) to exercise error paths.
func buildFrame(t *testing.T, declaredSize uint32, code any, payload []byte) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, declaredSize); err != nil {
		t.Fatalf("write size: %v", err)
	}

	if err := binary.Write(buf, binary.LittleEndian, code); err != nil {
		t.Fatalf("write code: %v", err)
	}

	buf.Write(payload)

	return buf.Bytes()
}

func TestMessageReadUint32CodeRoundTrip(t *testing.T) {
	payload := []byte("hello soulseek")
	// size covers the 4-byte code plus the payload.
	frame := buildFrame(t, uint32(4+len(payload)), uint32(42), payload)

	message, size, code, err := MessageRead(CodeServer(0), bytes.NewReader(frame), false)
	if err != nil {
		t.Fatalf("MessageRead: unexpected error: %v", err)
	}

	if code != CodeServer(42) {
		t.Errorf("code = %d, want 42", code)
	}

	if size != uint32(4+len(payload)) {
		t.Errorf("size = %d, want %d", size, 4+len(payload))
	}

	if !bytes.Equal(message.Bytes(), frame) {
		t.Errorf("message = %q, want %q", message.Bytes(), frame)
	}
}

func TestMessageReadUint8CodeRoundTrip(t *testing.T) {
	payload := []byte("init")
	// size covers the 1-byte code plus the payload.
	frame := buildFrame(t, uint32(1+len(payload)), uint8(7), payload)

	message, size, code, err := MessageRead(CodePeerInit(0), bytes.NewReader(frame), false)
	if err != nil {
		t.Fatalf("MessageRead: unexpected error: %v", err)
	}

	if code != CodePeerInit(7) {
		t.Errorf("code = %d, want 7", code)
	}

	if size != uint32(1+len(payload)) {
		t.Errorf("size = %d, want %d", size, 1+len(payload))
	}

	if !bytes.Equal(message.Bytes(), frame) {
		t.Errorf("message = %q, want %q", message.Bytes(), frame)
	}
}

func TestMessageReadOversizedDeclaredSize(t *testing.T) {
	// Only the size field is written: MessageRead must reject the declared
	// size before attempting to read the code or the message body, so no
	// large allocation ever happens.
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(MaxMessageSize+1)); err != nil {
		t.Fatalf("write size: %v", err)
	}

	_, _, _, err := MessageRead(CodeServer(0), buf, false)
	if !errors.Is(err, soul.ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestMessageReadTruncatedFrame(t *testing.T) {
	payload := []byte("short")
	// Declare a size larger than what is actually provided.
	frame := buildFrame(t, uint32(4+len(payload)+10), uint32(1), payload)

	_, _, _, err := MessageRead(CodeServer(0), bytes.NewReader(frame), false)
	if !errors.Is(err, soul.ErrDifferentPacketSize) {
		t.Fatalf("err = %v, want ErrDifferentPacketSize", err)
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadStringSizeCap(t *testing.T) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(MaxMessageSize+1)); err != nil {
		t.Fatalf("write size: %v", err)
	}

	_, err := ReadString(buf)
	if !errors.Is(err, soul.ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestReadBytesSizeCap(t *testing.T) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(MaxMessageSize+1)); err != nil {
		t.Fatalf("write size: %v", err)
	}

	_, err := ReadBytes(buf)
	if !errors.Is(err, soul.ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
}
