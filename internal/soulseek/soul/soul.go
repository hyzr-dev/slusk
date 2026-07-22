// Package soul holds the common types used by the rest of the sub-packages.
package soul

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
)

// ConnectionType represents the type of connection. Possible values are "P", "F" and "D".
type ConnectionType string

// ErrMismatchingCodes is returned when the code read from the stream does not match the expected of the consumer.
var ErrMismatchingCodes = errors.New("mismatching codes")

// ErrDifferentPacketSize is returned when the declared size of the package does not match the size of the actual read.
var ErrDifferentPacketSize = errors.New("the declared size of the package does not match the size of the actual read")

// ErrMessageTooLarge is returned when a message declares a size larger than internal.MaxMessageSize.
var ErrMessageTooLarge = errors.New("message declares a size larger than the maximum allowed message size")

// Token is a unique identifier of type uint32 that is used throughout the protocol.
type Token uint32

// NewToken returns a new cryptographically secure random token.
func NewToken() Token {
	var tokenBytes [4]byte
	rand.Read(tokenBytes[:])
	return Token(binary.LittleEndian.Uint32(tokenBytes[:]))
}
