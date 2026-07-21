package server

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/internal"
)

const CodePossibleParents Code = 102

// maxPossibleParents bounds the parent list a (possibly compromised) central
// server can make us allocate. The protocol documents a max of 10; 64 is a
// safe ceiling well above that. Without it the wire-supplied count drives an
// unbounded append loop, letting a dense 64MB frame force a few hundred MB of
// Parent structs.
const maxPossibleParents = 64

// PossibleParents code 102, the server send us a list of max 10 possible distributed
// parents to connect to. Messages of this type are sent to us at regular intervals,
// until we tell the server we don’t need more possible parents with a HaveNoParent message.
// The received list always contains users whose upload speed is higher than our own.
// If we have the highest upload speed on the server, we become a branch root, and start
// receiving SearchRequest messages directly from the server.
type PossibleParents struct {
	Parents []Parent
}

type Parent struct {
	Username string
	IP       net.IP
	Port     int
}

func (p *PossibleParents) Deserialize(reader io.Reader) error {
	_, err := internal.ReadUint32(reader) // size
	if err != nil {
		return err
	}

	code, err := internal.ReadUint32(reader) // code 102
	if err != nil {
		return err
	}

	if code != uint32(CodePossibleParents) {
		return errors.Join(soul.ErrMismatchingCodes,
			fmt.Errorf("expected code %d, got %d", CodePossibleParents, code))
	}

	parents, err := internal.ReadUint32(reader)
	if err != nil {
		return err
	}
	if parents > maxPossibleParents {
		return fmt.Errorf("possible parents count %d exceeds max %d", parents, maxPossibleParents)
	}

	for range int(parents) {
		var parent Parent

		parent.Username, err = internal.ReadString(reader)
		if err != nil {
			return err
		}

		ip, err := internal.ReadUint32(reader)
		if err != nil {
			return err
		}

		parent.IP = internal.ReadIP(ip)

		parent.Port, err = internal.ReadUint32ToInt(reader)
		if err != nil {
			return err
		}

		p.Parents = append(p.Parents, parent)
	}

	return err
}
