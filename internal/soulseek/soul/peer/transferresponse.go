package peer

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/internal"
)

const CodeTransferResponse Code = 41

// TransferResponse code 41 response to TransferRequest.
// We (or the other peer) either agrees, or tells the reason
// for rejecting the file upload.
type TransferResponse struct {
	Token   soul.Token
	Allowed bool
	Reason  error
}

// ErrNotAllowedWithNoReason is returned when a TransferResponse is not allowed and no reason is provided.
var ErrNotAllowedWithNoReason = errors.New("rejection reason is required when transfer is not allowed")

// Serialize accepts a TransferResponse and returns a message packed as a byte slice.
// If the transfer is not allowed, a reason must be provided. The possible errors are:
// ErrBanned, ErrCancelled, ErrComplete, ErrFileNotShared, ErrFileReadError, ErrPendingShutdown,
// ErrQueued, ErrTooManyFiles, ErrTooManyMegabytes, and ErrNotAllowedWithNoReason.
// All errors exist in the peer.TransferResponse package.
func (t *TransferResponse) Serialize(message *TransferResponse) ([]byte, error) {
	buf := new(bytes.Buffer)

	err := internal.WriteUint32(buf, uint32(CodeTransferResponse))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(buf, uint32(message.Token))
	if err != nil {
		return nil, err
	}

	err = internal.WriteBool(buf, message.Allowed)
	if err != nil {
		return nil, err
	}

	if !message.Allowed {
		if message.Reason == nil {
			return nil, ErrNotAllowedWithNoReason
		}

		err = internal.WriteString(buf, message.Reason.Error())
		if err != nil {
			return nil, err
		}
	}

	return internal.Pack(buf.Bytes())
}

// Deserialize populates a TransferResponse with the data in the provided reader.
func (t *TransferResponse) Deserialize(reader io.Reader) error {
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint32(reader) // code 41
	if err != nil {
		return err
	}

	if code != uint32(CodeTransferResponse) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeTransferResponse, code))
	}

	token, err := internal.ReadUint32(reader)
	if err != nil {
		return err
	}

	t.Token = soul.Token(token)

	t.Allowed, err = internal.ReadBool(reader)
	if err != nil {
		return err
	}

	if !t.Allowed {
		// The reason string is a protocol-documented optional trailing
		// field. Reading its length prefix and body as separate steps lets
		// us tell apart the two ways this can come up short: the field may
		// be completely absent (a clean io.EOF reading the length prefix
		// itself, with zero bytes remaining in the frame at all), which is
		// not an error - the peer simply omitted it - versus the length
		// prefix being present but declaring a body that is missing or
		// truncated, which is always a hard error even when that failure
		// also happens to surface as a clean io.EOF (e.g. a declared
		// nonzero-length body with zero bytes actually following it).
		size, err := internal.ReadStringLen(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		r, err := internal.ReadStringBody(reader, size)
		if err != nil {
			return err
		}

		t.Reason = Reason(r)
	}

	return nil
}
