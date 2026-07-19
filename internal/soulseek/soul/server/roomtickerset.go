package server

import (
	"bytes"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/internal"
)

const CodeRoomTickerSet Code = 116

type RoomTickerSet struct{}

func (r RoomTickerSet) Serialize(room, ticker string) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeRoomTickerSet))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, room)
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, ticker)
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}
