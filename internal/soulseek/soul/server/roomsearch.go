package server

import (
	"bytes"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/internal"
)

const CodeRoomSearch Code = 120

type RoomSearch struct {
	Room        string
	Token       soul.Token
	SearchQuery string
}

func (r *RoomSearch) Serialize(message *RoomSearch) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeRoomSearch))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, message.Room)
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(buf, uint32(message.Token))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, message.SearchQuery)
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}
