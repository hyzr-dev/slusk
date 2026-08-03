package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

const privateMessageSelect = `SELECT id, username, direction, body, server_message_id, is_admin, sent_at, received_at, read_at FROM private_messages`

// maxConversations caps how many peers Conversations returns. Without it a peer with an
// unbounded number of distinct correspondents (or a hostile one opportunistically
// messaging from many usernames) would make GET /api/messages return the entire table,
// each row carrying a full message body. Chosen to comfortably exceed any real dashboard
// use while still bounding worst-case response size, consistent with Thread's cap.
const maxConversations = 500

func scanPrivateMessage(r rowScanner) (core.PrivateMessage, error) {
	var m core.PrivateMessage
	var direction string
	var isAdmin int64
	var serverMessageID sql.NullInt64
	var readAt sql.NullTime
	if err := r.Scan(&m.ID, &m.Username, &direction, &m.Body, &serverMessageID, &isAdmin, &m.SentAt, &m.ReceivedAt, &readAt); err != nil {
		return core.PrivateMessage{}, err
	}
	m.Direction = core.MessageDirection(direction)
	m.Admin = isAdmin != 0
	if serverMessageID.Valid {
		id := serverMessageID.Int64
		m.ServerMessageID = &id
	}
	if readAt.Valid {
		t := readAt.Time
		m.ReadAt = &t
	}
	return m, nil
}

func scanPrivateMessages(rows *sql.Rows) ([]core.PrivateMessage, error) {
	var out []core.PrivateMessage
	for rows.Next() {
		m, err := scanPrivateMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// RecordIncomingMessage persists one received private message. inserted is false when
// the same server_message_id is already stored, which is the normal outcome for a
// message the server redelivered because a crash or a failed ack write landed between
// the persist and the ack. Callers must treat inserted == false as success and ack
// anyway — that is what stops the redelivery loop.
func (s *Store) RecordIncomingMessage(ctx context.Context, m core.PrivateMessage) (id int64, inserted bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO private_messages (username, direction, body, server_message_id, is_admin, sent_at, received_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (server_message_id) WHERE server_message_id IS NOT NULL DO NOTHING
		 RETURNING id`,
		m.Username, string(core.MessageIncoming), m.Body, m.ServerMessageID, boolToInt(m.Admin), m.SentAt, m.ReceivedAt,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("record incoming message: %w", err)
	}
	return id, true, nil
}

// RecordOutgoingMessage persists one private message this client sent, always
// marked already-read (ReadAt tracking only ever applies to incoming
// messages, and a message we sent is read by definition). It is
// send-then-persist — see cmd/slusk's sendMessageFn for why that
// ordering, not the reverse, is correct here.
func (s *Store) RecordOutgoingMessage(ctx context.Context, username, body string, now time.Time) (core.PrivateMessage, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO private_messages (username, direction, body, server_message_id, is_admin, sent_at, received_at, read_at)
		 VALUES ($1, $2, $3, NULL, 0, $4, $4, $4)
		 RETURNING id`,
		username, string(core.MessageOutgoing), body, now,
	).Scan(&id)
	if err != nil {
		return core.PrivateMessage{}, fmt.Errorf("record outgoing message: %w", err)
	}
	return core.PrivateMessage{
		ID:         id,
		Username:   username,
		Direction:  core.MessageOutgoing,
		Body:       body,
		SentAt:     now,
		ReceivedAt: now,
		ReadAt:     &now,
	}, nil
}

// Conversations lists every peer with at least one private message, newest
// activity first, with per-peer unread and total counts for the dashboard's
// conversation list (GET /api/messages).
func (s *Store) Conversations(ctx context.Context) ([]core.Conversation, error) {
	// Ranked by id, not sent_at, for the same reason as Thread's ORDER BY: sent_at is
	// peer-supplied wire input and can be out of order with arrival, so using it here
	// could pick a stale message as "last" or misorder the conversation list itself.
	rows, err := s.db.QueryContext(ctx, `
		WITH ranked AS (
		  SELECT id, username, body, direction, sent_at,
		         ROW_NUMBER() OVER (PARTITION BY username ORDER BY id DESC) AS rn
		  FROM private_messages
		), agg AS (
		  SELECT username, COUNT(*) AS total,
		         SUM(CASE WHEN direction = 'IN' AND read_at IS NULL THEN 1 ELSE 0 END) AS unread
		  FROM private_messages GROUP BY username
		)
		SELECT a.username, r.body, r.direction, r.sent_at, a.unread, a.total
		FROM agg a JOIN ranked r ON r.username = a.username AND r.rn = 1
		ORDER BY r.id DESC, a.username
		LIMIT $1`, maxConversations)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []core.Conversation
	for rows.Next() {
		var c core.Conversation
		var direction string
		if err := rows.Scan(&c.Username, &c.LastMessage, &direction, &c.LastMessageAt, &c.Unread, &c.Total); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		c.LastDirection = core.MessageDirection(direction)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Thread returns one peer's messages newest-first, capped at limit. beforeID > 0 pages
// backwards (keyset, not OFFSET, so a concurrent insert cannot shift a page).
func (s *Store) Thread(ctx context.Context, username string, limit int, beforeID int64) ([]core.PrivateMessage, error) {
	query := privateMessageSelect + ` WHERE username = $1`
	args := []any{username}
	if beforeID > 0 {
		query += ` AND id < $2`
		args = append(args, beforeID)
	}
	// id DESC alone, not sent_at: sent_at is the peer-supplied wire timestamp (see
	// idx_private_messages_thread's comment in the migration), so ordering or paging by
	// it can skip or duplicate rows across pages. id is assigned on insert and therefore
	// monotonic with arrival order, which is what beforeID's keyset pagination requires.
	query += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query thread: %w", err)
	}
	defer rows.Close()
	return scanPrivateMessages(rows)
}

// MarkConversationRead stamps read_at on every unread incoming message from
// username, returning how many rows were touched. Outgoing messages and
// other peers' messages are never affected.
func (s *Store) MarkConversationRead(ctx context.Context, username string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE private_messages SET read_at = $1 WHERE username = $2 AND direction = $3 AND read_at IS NULL`,
		now, username, string(core.MessageIncoming))
	if err != nil {
		return 0, fmt.Errorf("mark conversation read: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mark conversation read rows affected: %w", err)
	}
	return n, nil
}

// UnreadMessageCount returns the total number of unread incoming messages
// across every conversation.
func (s *Store) UnreadMessageCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM private_messages WHERE direction = $1 AND read_at IS NULL`,
		string(core.MessageIncoming)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unread messages: %w", err)
	}
	return n, nil
}
