package server

import (
	"bytes"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/internal"
)

const CodeBranchLevel Code = 126

type BranchLevel struct {
	Level int
}

func (b *BranchLevel) Serialize(message *BranchLevel) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeBranchLevel))
	if err != nil {
		return nil, err
	}

	err = internal.WriteUint32(buf, uint32(message.Level))
	if err != nil {
		return nil, err
	}

	return internal.Pack(buf.Bytes())
}
