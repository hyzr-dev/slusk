package internal

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

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

// --- deobfuscate hardening ---

// buildObfuscatedFrame builds a raw obfuscated wire frame by hand: a random
// 4-byte key followed by the frame's plaintext (declaredSize, code, payload)
// XORed against the key stream produced by rotating it 4 bytes at a time,
// exactly as obfuscate does. declaredSize may be set to a value other than
// len(code)+len(payload) to exercise error paths, and payload may be shorter
// than what declaredSize promises to exercise truncation.
func buildObfuscatedFrame(t *testing.T, declaredSize uint32, codeBytes []byte, payload []byte) []byte {
	t.Helper()

	plain := new(bytes.Buffer)
	if err := binary.Write(plain, binary.LittleEndian, declaredSize); err != nil {
		t.Fatalf("write declared size: %v", err)
	}
	plain.Write(codeBytes)
	plain.Write(payload)

	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("rand key: %v", err)
	}

	out := new(bytes.Buffer)
	out.Write(key[:])

	rotated := rotateKeyBytes(key)
	for i, b := range plain.Bytes() {
		out.WriteByte(rotated[i%4] ^ b)
		if i%4 == 3 {
			rotated = rotateKeyBytes(rotated)
		}
	}

	return out.Bytes()
}

// rotateKeyBytes performs the same 31-bit rotate as deobfuscate/obfuscate,
// operating on a raw 4-byte little-endian key.
func rotateKeyBytes(key [4]byte) [4]byte {
	v := binary.LittleEndian.Uint32(key[:])
	v = (v >> 31) | (v << 1)
	var out [4]byte
	binary.LittleEndian.PutUint32(out[:], v)
	return out
}

// runDeobfuscateWithTimeout runs deobfuscate(connection, false) with a
// deadline, so a regression that reintroduces unbounded buffering fails the
// test instead of hanging the suite.
func runDeobfuscateWithTimeout(t *testing.T, connection io.Reader) (*bytes.Buffer, uint32, CodePeer, error) {
	t.Helper()

	type result struct {
		message *bytes.Buffer
		size    uint32
		code    CodePeer
		err     error
	}
	done := make(chan result, 1)
	go func() {
		message, size, code, err := deobfuscate(connection, false)
		done <- result{message, size, code, err}
	}()

	select {
	case r := <-done:
		return r.message, r.size, r.code, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("deobfuscate did not return within 2s; likely unbounded buffering")
		return nil, 0, 0, nil
	}
}

func TestDeobfuscateZeroSizeRejected(t *testing.T) {
	frame := buildObfuscatedFrame(t, 0, nil, nil)

	_, _, _, err := runDeobfuscateWithTimeout(t, bytes.NewReader(frame))
	if !errors.Is(err, soul.ErrDifferentPacketSize) {
		t.Fatalf("err = %v, want ErrDifferentPacketSize", err)
	}
}

func TestDeobfuscateShortSizesRejected(t *testing.T) {
	for _, size := range []uint32{1, 2, 3} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			// Declare a size smaller than the 4-byte code that always
			// follows, with no further bytes on the wire: the fix must
			// reject this via the readSoFar/size accounting rather than
			// blocking for bytes that will never arrive.
			frame := buildObfuscatedFrame(t, size, []byte{1, 0, 0, 0}, nil)

			_, _, _, err := runDeobfuscateWithTimeout(t, bytes.NewReader(frame))
			if !errors.Is(err, soul.ErrDifferentPacketSize) {
				t.Fatalf("err = %v, want ErrDifferentPacketSize", err)
			}
		})
	}
}

func TestDeobfuscateOvershootingSizeRejected(t *testing.T) {
	// Declare a size that promises an 8-byte body (code + 4 bytes payload)
	// but only provide 2 of those payload bytes before the connection ends.
	// A regression to the pre-fix unsigned-underflow arithmetic would
	// instead keep requesting 4-byte chunks from the connection forever.
	frame := buildObfuscatedFrame(t, 8, []byte{1, 0, 0, 0}, []byte{9, 9})
	// Truncate the frame after the key + declared size + code, leaving only
	// the 2 payload bytes actually written above (drop nothing further; the
	// frame is already short of what declaredSize=8 promises).

	_, _, _, err := runDeobfuscateWithTimeout(t, bytes.NewReader(frame))
	if !errors.Is(err, soul.ErrDifferentPacketSize) {
		t.Fatalf("err = %v, want ErrDifferentPacketSize", err)
	}
}

func TestDeobfuscateRoundTrip(t *testing.T) {
	payload := []byte("hello obfuscated soulseek")
	// Build the plaintext frame the same way MessageRead expects: size
	// covers the 4-byte code plus payload.
	plain := new(bytes.Buffer)
	if err := binary.Write(plain, binary.LittleEndian, uint32(4+len(payload))); err != nil {
		t.Fatalf("write size: %v", err)
	}
	if err := binary.Write(plain, binary.LittleEndian, uint32(42)); err != nil {
		t.Fatalf("write code: %v", err)
	}
	plain.Write(payload)

	obfuscated, err := obfuscate(plain.Bytes())
	if err != nil {
		t.Fatalf("obfuscate: %v", err)
	}

	message, size, code, err := MessageRead(CodeServer(0), bytes.NewReader(obfuscated), true)
	if err != nil {
		t.Fatalf("MessageRead: unexpected error: %v", err)
	}
	if code != CodeServer(42) {
		t.Errorf("code = %d, want 42", code)
	}
	if size != uint32(4+len(payload)) {
		t.Errorf("size = %d, want %d", size, 4+len(payload))
	}
	if !bytes.Equal(message.Bytes(), plain.Bytes()) {
		t.Errorf("message = %q, want %q", message.Bytes(), plain.Bytes())
	}
}
