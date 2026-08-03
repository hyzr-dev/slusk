// Package soulseek: messages.go implements Soulseek private messaging (issue
// #183): receiving CodeMessageUser from the server, handing it to a
// MessageSink off the connection read loop, acking it only once the sink
// confirms it is durably persisted, and sending outgoing messages via
// SendPrivateMessage. See handleIncomingPrivateMessage's use in client.go's
// handleMessage for the enqueue side, and runMessageWorker for the
// persist-then-ack side.
package soulseek

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
)

// maxPrivateMessageBytes caps an outgoing private message. Enforced before any wire
// contact so malformed input can never reach sendToServerGeneration, whose write path
// tears the server session down on failure. Shared with internal/observ's identical
// limit via core.MaxPrivateMessageBytes — see that constant's doc comment for why it
// lives in core rather than being declared independently in each package.
const maxPrivateMessageBytes = core.MaxPrivateMessageBytes

// maxIncomingMessageBytes caps a stored incoming body. The frame decoder only bounds a
// field at internal.MaxMessageSize (64 MiB), so without this an unfriendly peer could
// write arbitrarily large rows into our database. An oversize body is truncated, not
// dropped, and still acked — dropping it unacked would make the server redeliver it at
// every login forever.
const maxIncomingMessageBytes = 64 << 10

// maxIncomingUsernameBytes caps a peer's username as read off the wire. A Soulseek
// username is registered at a few dozen characters at most, but the frame decoder only
// bounds the field at internal.MaxMessageSize (64 MiB) — the same untrusted-frame
// concern as maxIncomingMessageBytes, and username is additionally used as the
// Conversations partition key and a URL path segment (GET/POST
// /api/messages/{username}), so an oversize value is worth rejecting cheaply rather
// than letting it flow further. 256 bytes is generous headroom over any real username
// while still bounding worst case.
const maxIncomingUsernameBytes = 256

// messageSinkTimeout bounds a single sink write. It runs on the message worker, not on
// readLoop, so exceeding it costs one unacked message (redelivered at the next login)
// rather than stalling server-message dispatch.
const messageSinkTimeout = 10 * time.Second

// incomingMessageQueue is how many received private messages may await persistence.
// handleIncomingPrivateMessage never blocks on it: a full queue drops the message
// WITHOUT acking, so the server keeps it and redelivers at the next login. Bounded
// because the peer, not us, controls the arrival rate.
const incomingMessageQueue = 256

var (
	ErrEmptyMessage    = errors.New("soulseek: message body is empty")
	ErrMessageTooLong  = errors.New("soulseek: message body too long")
	ErrInvalidUsername = errors.New("soulseek: username is empty")
)

// PrivateMessage is one incoming private message handed to a MessageSink.
type PrivateMessage struct {
	ServerMessageID int64 // echoed back in MessageAcked; 0 when the server sent none
	Username        string
	Body            string
	SentAt          time.Time
	Admin           bool
}

// MessageSink receives incoming private messages. An implementation MUST have durably
// persisted the message before returning nil: the client sends MessageAcked only on a
// nil return, and acking is what makes the server stop redelivering. A non-nil return
// leaves the message unacked, so it arrives again at the next login — implementations
// must therefore be idempotent on PrivateMessage.ServerMessageID.
//
// Implementations are called from the message worker, never from the connection read
// loop, so a slow write delays acking but never blocks server-message dispatch.
type MessageSink interface {
	HandlePrivateMessage(ctx context.Context, msg PrivateMessage) error
}

// incomingMessage pairs a received message with the server generation it arrived on, so
// the worker can skip the ack if the connection has since been replaced — the message id
// is only meaningful within the session that issued it, and skipping simply means the
// server redelivers.
type incomingMessage struct {
	msg        PrivateMessage
	generation uint64
}

// SendPrivateMessage sends body to username after validating both. The Soulseek protocol
// offers no delivery confirmation, and the server stores messages for offline recipients
// and delivers them at their next login — so a nil return means "written to the server",
// never "delivered" or "read". Callers must not present it as either.
func (c *Client) SendPrivateMessage(ctx context.Context, username, body string) error {
	if strings.TrimSpace(username) == "" {
		return ErrInvalidUsername
	}
	if strings.TrimSpace(body) == "" {
		return ErrEmptyMessage
	}
	if len(body) > maxPrivateMessageBytes {
		return ErrMessageTooLong
	}
	return sendToServerGeneration(c, 0, &server.MessageUser{Username: username, Message: body})
}

// handleIncomingPrivateMessage handles server.CodeMessageUser. It must be non-blocking
// end to end: no DB work, no ack — those happen later, off readLoop, in
// runMessageWorker. readLoop is the serial dispatcher for ALL server messages, including
// ConnectToPeer and search results, so a slow or failing sink call here would stall
// downloads, the application's actual purpose. It never returns a non-nil error: readLoop
// treats any error as fatal and tears down the whole Soulseek session, and a message we
// failed to enqueue is simply left unacked for redelivery at the next login.
func (c *Client) handleIncomingPrivateMessage(reader io.Reader) error {
	msg := &server.MessageUser{}
	if err := msg.Deserialize(reader); err != nil {
		return fmt.Errorf("deserialize message user: %w", err)
	}

	pm := PrivateMessage{
		// server.MessageUser.UserID is misnamed: on the wire, code 22 (Message
		// user) is `uint32 id, uint32 timestamp, string username, string
		// message, bool is_admin`, and the first uint32 is the MESSAGE id
		// echoed back in MessageAcked.MessageID, not a user id. Likewise
		// msg.New actually carries is_admin, not any notion of "new". Get
		// either wrong and either the server never stops redelivering
		// (MessageID) or the admin flag is silently swapped (New).
		ServerMessageID: int64(msg.UserID),
		// Both Username and Message are peer-controlled fields off the same frame,
		// bounded on the wire only by internal.MaxMessageSize (64 MiB) — truncate both
		// to a sane limit, and to a valid UTF-8 boundary, so neither can grow a database
		// row unbounded or leave the body ending mid-rune (see maxIncomingMessageBytes
		// and truncateValidUTF8's doc comments for why an invalid tail is not merely
		// cosmetic: Postgres TEXT in a UTF8 database rejects it outright, which would
		// fail the insert, leave the message unacked, and redeliver it at every login
		// forever).
		Username: truncateValidUTF8(msg.Username, maxIncomingUsernameBytes),
		Body:     truncateValidUTF8(msg.Message, maxIncomingMessageBytes),
		SentAt:   time.Unix(int64(msg.Timestamp), 0).UTC(),
		Admin:    msg.New,
	}

	if c.cfg.MessageSink == nil {
		if c.logger != nil {
			c.logger.Debug("dropping incoming private message: no sink configured; leaving unacked for redelivery at next login",
				"from", pm.Username)
		}
		return nil
	}

	// generation 0 means "no active server connection" (see currentServerGeneration),
	// which sendToServerGeneration otherwise treats as "any generation, don't check" —
	// exactly the check an ack for this message must NOT skip. Enqueuing it anyway would
	// let a later, unrelated session ack a message it never received. There is no
	// reachable path to this branch today (handleIncomingPrivateMessage only runs while
	// a server message is being dispatched, which implies a live connection), but the
	// guard is cheap and the alternative failure mode is silent.
	generation := c.currentServerGeneration()
	if generation == 0 {
		if c.logger != nil {
			c.logger.Warn("private message arrived with no active server generation; dropping unacked", "username", pm.Username)
		}
		return nil
	}

	select {
	case c.incoming <- incomingMessage{msg: pm, generation: generation}:
	default:
		if c.logger != nil {
			c.logger.Warn("private message queue full; dropping unacked", "username", pm.Username)
		}
	}
	return nil
}

// truncateValidUTF8 truncates s to at most maxBytes bytes, then drops any trailing
// invalid UTF-8 introduced by cutting mid-rune. len() and slicing count bytes, not
// runes, so truncating a multibyte body or username at a byte boundary can leave an
// incomplete sequence at the end; strings.ToValidUTF8 with an empty replacement removes
// exactly that incomplete tail rather than the whole final rune's worth of valid data.
func truncateValidUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBytes], "")
}

// runMessageWorker persists received private messages and acks them. It is deliberately
// off readLoop: acking is what stops the server redelivering, so the ack must follow a
// durable write — but nothing requires that write to happen inside the read loop, and
// doing it there would let a slow database stall ConnectToPeer and search dispatch.
func (c *Client) runMessageWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-c.incoming:
			// WithoutCancel so a reconnect racing this write does not abort a
			// persist we could otherwise have completed and acked. Deliberate
			// trade-off: it also means ctx.Done() (process shutdown) does not
			// abort an in-flight persist either, so a hung database can delay
			// this worker's exit by up to messageSinkTimeout even when the
			// caller wants to shut down immediately.
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), messageSinkTimeout)
			err := c.cfg.MessageSink.HandlePrivateMessage(persistCtx, entry.msg)
			cancel()
			if err != nil {
				if c.logger != nil {
					c.logger.Error("private message sink failed; leaving unacked for redelivery at next login",
						"from", entry.msg.Username, "err", err)
				}
				continue
			}

			// entry.generation, not the current generation: a stale ack is
			// refused by sendToServerGeneration's existing generation check
			// rather than written to a new session. Expected after a
			// reconnect, so debug rather than error — it self-corrects via
			// redelivery.
			if err := sendToServerGeneration(c, entry.generation, &server.MessageAcked{MessageID: int(entry.msg.ServerMessageID)}); err != nil {
				if c.logger != nil {
					c.logger.Debug("ack private message failed; will redeliver at next login",
						"from", entry.msg.Username, "err", err)
				}
			}
		}
	}
}
