package server

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/distributed"
)

func TestEmbeddedMessageDeserializeRawRemainingPayload(t *testing.T) {
	rawSearchBody := []byte{49, 0, 0, 0, 5, 0, 0, 0, 'a', 'l', 'i', 'c', 'e', 7, 0, 0, 0, 3, 0, 0, 0, 'r', 'a', 'w'}
	frame := make([]byte, 4+4+1+len(rawSearchBody))
	binary.LittleEndian.PutUint32(frame[:4], uint32(4+1+len(rawSearchBody)))
	binary.LittleEndian.PutUint32(frame[4:8], uint32(CodeEmbeddedMessage))
	frame[8] = byte(distributed.CodeSearch)
	copy(frame[9:], rawSearchBody)

	var got EmbeddedMessage
	if err := got.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize raw server frame: %v", err)
	}
	if got.Code != distributed.CodeSearch {
		t.Fatalf("embedded code = %d, want %d", got.Code, distributed.CodeSearch)
	}
	if !bytes.Equal(got.Message, rawSearchBody) {
		t.Fatalf("embedded message = %x, want raw remainder %x", got.Message, rawSearchBody)
	}
}
