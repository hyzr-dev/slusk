package server

import (
	"bytes"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul/internal"
)

const CodeUnwatchUser Code = 6

type UnwatchUser struct {
	Username string
}

func (u *UnwatchUser) Serialize(message *UnwatchUser) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeUnwatchUser))
	if err != nil {
		return nil, err
	}

	err = internal.WriteString(buf, message.Username)
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}
