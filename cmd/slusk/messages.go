package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/soulseek"
)

// incomingMessageStore is the narrow slice of *store.Store messageSink needs,
// so tests can supply a fake rather than a real database.
type incomingMessageStore interface {
	RecordIncomingMessage(ctx context.Context, m core.PrivateMessage) (id int64, inserted bool, err error)
}

// messageSink persists incoming private messages so the soulseek client can ack them. It is
// the durability half of the persist-then-ack contract in soulseek.MessageSink: returning an
// error here deliberately leaves the message unacked, so the server redelivers it at the next
// login instead of it being lost.
type messageSink struct {
	store  incomingMessageStore // narrow interface, for tests
	logger *slog.Logger
}

// HandlePrivateMessage implements soulseek.MessageSink. inserted == false (a redelivered
// duplicate, see RecordIncomingMessage) is still a nil return: the message is already
// durably stored, so the client should ack it exactly as if this were the first delivery.
func (s *messageSink) HandlePrivateMessage(ctx context.Context, msg soulseek.PrivateMessage) error {
	m := core.PrivateMessage{
		Username:   msg.Username,
		Direction:  core.MessageIncoming,
		Body:       msg.Body,
		Admin:      msg.Admin,
		SentAt:     msg.SentAt.UTC(),
		ReceivedAt: time.Now().UTC(),
	}
	// ServerMessageID 0 means the server sent none (see soulseek.PrivateMessage), not a
	// real id — leaving it nil there keeps the store's NULL-means-outgoing-or-unknown
	// convention (see RecordIncomingMessage's ON CONFLICT) from being fooled by a
	// coincidental zero id.
	if msg.ServerMessageID != 0 {
		id := msg.ServerMessageID
		m.ServerMessageID = &id
	}

	_, inserted, err := s.store.RecordIncomingMessage(ctx, m)
	if err != nil {
		return err
	}
	if !inserted && s.logger != nil {
		s.logger.Debug("private message already persisted; acking redelivered duplicate",
			"from", msg.Username, "serverMessageID", msg.ServerMessageID)
	}
	return nil
}
