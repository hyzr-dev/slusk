package peer

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/internal"
)

const (
	CodePeerInit CodeInit = 1
	// MaxPeerInitUsernameSize bounds the identity retained for a peer session.
	MaxPeerInitUsernameSize       uint32 = 1024
	maxPeerInitConnectionTypeSize uint32 = 1
)

// PeerInit code 1 message is sent to initiate a direct connection to another peer.
type PeerInit struct {
	// Username is the username of the peer that wants to connect to us.
	Username       string
	ConnectionType soul.ConnectionType
}

// Serialize accepts a PeerInit and returns a message packed as a byte slice.
func (p *PeerInit) Serialize(message *PeerInit) ([]byte, error) {
	if message.Username == "" {
		return nil, errors.New("peer init username must be nonempty")
	}
	if len(message.Username) > int(MaxPeerInitUsernameSize) {
		return nil, fmt.Errorf("%w: peer init username exceeds %d bytes", soul.ErrMessageTooLarge, MaxPeerInitUsernameSize)
	}
	if len(message.ConnectionType) != int(maxPeerInitConnectionTypeSize) {
		return nil, errors.New("peer init connection type must be one byte")
	}

	buf := new(bytes.Buffer)
	err := internal.WriteUint8(buf, uint8(CodePeerInit))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, message.Username)
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, string(message.ConnectionType))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(buf, uint32(0)) // unknown
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}

// Deserialize populates a PeerInit with the data in the provided reader.
func (p *PeerInit) Deserialize(reader io.Reader) error {
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint8(reader) // code 1
	if err != nil {
		return err
	}

	if code != uint8(CodePeerInit) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodePeerInit, code))
	}

	usernameSize, err := internal.ReadStringLen(reader)
	if err != nil {
		return err
	}
	if usernameSize == 0 {
		return errors.New("peer init username must be nonempty")
	}
	if usernameSize > MaxPeerInitUsernameSize {
		return fmt.Errorf("%w: peer init username exceeds %d bytes", soul.ErrMessageTooLarge, MaxPeerInitUsernameSize)
	}
	p.Username, err = internal.ReadStringBody(reader, usernameSize)
	if err != nil {
		return err
	}

	connectionTypeSize, err := internal.ReadStringLen(reader)
	if err != nil {
		return err
	}
	if connectionTypeSize != maxPeerInitConnectionTypeSize {
		return errors.New("peer init connection type must be one byte")
	}
	connType, err := internal.ReadStringBody(reader, connectionTypeSize)
	if err != nil {
		return err
	}

	p.ConnectionType = soul.ConnectionType(connType)

	_, err = internal.ReadUint32(reader) // unknown
	if err != nil {
		return err
	}

	return nil
}
