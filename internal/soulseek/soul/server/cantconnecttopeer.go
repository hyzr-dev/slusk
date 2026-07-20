package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/internal"
)

const CodeCantConnectToPeer Code = 1001

type CantConnectToPeer struct {
	Token    soul.Token
	Username string
}

func (c *CantConnectToPeer) Serialize(message *CantConnectToPeer) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeCantConnectToPeer))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(buf, uint32(message.Token))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, message.Username)
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}

func (c *CantConnectToPeer) Deserialize(reader io.Reader) error {
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint32(reader) // code 1001
	if err != nil {
		return err
	}

	if code != uint32(CodeCantConnectToPeer) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodeCantConnectToPeer, code))
	}

	c.Token, err = internal.ReadUint32ToToken(reader)
	if err != nil {
		return err
	}

	// This message is asymmetric: clients send token+username, but the server
	// replies with the token only when a peer could not complete our indirect
	// connection request. A completely absent trailing username is therefore
	// valid on receive; a present but truncated field remains malformed.
	size, err := internal.ReadStringLen(reader)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	c.Username, err = internal.ReadStringBody(reader, size)
	return err
}
