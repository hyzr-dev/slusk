package server

import (
	"bytes"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/internal"
)

const CodeWishlistSearch Code = 103

type WishlistSearch struct {
	Token       soul.Token
	SearchQuery string
}

func (w *WishlistSearch) Serialize(message *WishlistSearch) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := internal.WriteUint32(buf, uint32(CodeWishlistSearch))
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
