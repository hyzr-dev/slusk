package core

import "time"

// MessageDirection is which side of a private-message conversation sent one
// message: IN for something a Soulseek peer sent us, OUT for something we
// sent them.
type MessageDirection string

const (
	MessageIncoming MessageDirection = "IN"
	MessageOutgoing MessageDirection = "OUT"
)

// PrivateMessage is one persisted Soulseek private message, in either
// direction. Username always names the remote peer, never our own account,
// so a conversation's history reads the same regardless of direction.
type PrivateMessage struct {
	ID        int64
	Username  string // the remote peer, in BOTH directions
	Direction MessageDirection
	Body      string
	// ServerMessageID is the server-assigned id echoed back in MessageAcked;
	// the dedup key for redelivered offline messages. nil for outgoing.
	ServerMessageID *int64
	Admin           bool
	SentAt          time.Time  // wire timestamp (incoming) / send time (outgoing), UTC
	ReceivedAt      time.Time  // when this row was written
	ReadAt          *time.Time // nil == unread; only ever set for incoming
}

// Conversation is one peer's private-message thread summary, as listed by
// GET /api/messages: who it is with, what was last said, and how much of it
// is unread.
type Conversation struct {
	Username      string
	LastMessage   string
	LastMessageAt time.Time
	LastDirection MessageDirection
	Unread        int
	Total         int
}
