package distributed

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/internal"
)

const CodeEmbeddedMessage Code = 93

// EmbeddedMessage code 93 is the deprecated wrapper older clients used when forwarding a
// server-provided distributed search. Current frames use one-byte outer and inner codes followed
// immediately by the raw inner payload. Older SoulseekQt frames used a four-byte outer code.
type EmbeddedMessage struct {
	Code    Code
	Message []byte
}

// Serialize accepts a code and message and returns a message packed as a byte slice.
func (e *EmbeddedMessage) Serialize(message *EmbeddedMessage) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint8(buf, uint8(CodeEmbeddedMessage))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint8(buf, uint8(message.Code))
	if err != nil {
		return nil, err
	}

	if _, err = buf.Write(message.Message); err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}

// Deserialize accepts a reader and deserializes the message into the EmbeddedMessage struct.
func (e *EmbeddedMessage) Deserialize(reader io.Reader) error {
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint8(reader) // code 93
	if err != nil {
		return err
	}

	if code != uint8(CodeEmbeddedMessage) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeEmbeddedMessage, code))
	}

	remainder, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	// MessageRead consumes the first byte of the outer code. Older SoulseekQt
	// encoded that distributed outer code as uint32, leaving three zero bytes
	// before the one-byte embedded code.
	if len(remainder) >= 3 && remainder[0] == 0 && remainder[1] == 0 && remainder[2] == 0 {
		remainder = remainder[3:]
	}
	if len(remainder) == 0 {
		return io.ErrUnexpectedEOF
	}

	e.Code = Code(remainder[0])
	e.Message = append(e.Message[:0], remainder[1:]...)
	return nil
}
