// Package store: reliability.go holds the known_users / artist_user_reliability
// read and write paths. See the schema comment above those tables for why they
// are written incrementally rather than derived from candidate_attempts.
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// RecordAttemptOutcome upserts a peer's global (known_users) reliability row,
// and its artist-specific (artist_user_reliability) row when artistID is
// known, after a candidate attempt reaches a terminal state. This is the ONLY
// place peer history is written, and must be called at attempt completion
// (success or fail) rather than derived from candidate_attempts, since
// ResetJobForRetry deletes that history on every retry cycle. artistID <= 0
// (not yet backfilled onto the job) skips the artist-specific row rather than
// writing a bogus artist_id=0 bucket; the global row is still recorded.
func (s *Store) RecordAttemptOutcome(ctx context.Context, artistID int64, username string, success bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	successInc, failInc := 0, 0
	var successAt, failAt any
	if success {
		successInc, successAt = 1, now
	} else {
		failInc, failAt = 1, now
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO known_users (username, success_count, fail_count, last_success_at, last_fail_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(username) DO UPDATE SET
		   success_count = success_count + excluded.success_count,
		   fail_count = fail_count + excluded.fail_count,
		   last_success_at = COALESCE(excluded.last_success_at, last_success_at),
		   last_fail_at = COALESCE(excluded.last_fail_at, last_fail_at),
		   updated_at = excluded.updated_at`,
		username, successInc, failInc, successAt, failAt, now); err != nil {
		return fmt.Errorf("upsert known_users: %w", err)
	}

	if artistID <= 0 {
		return tx.Commit()
	}

	var userID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM known_users WHERE username = ?`, username).Scan(&userID); err != nil {
		return fmt.Errorf("read known_users id: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artist_user_reliability (artist_id, user_id, success_count, fail_count, last_success_at, last_fail_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(artist_id, user_id) DO UPDATE SET
		   success_count = success_count + excluded.success_count,
		   fail_count = fail_count + excluded.fail_count,
		   last_success_at = COALESCE(excluded.last_success_at, last_success_at),
		   last_fail_at = COALESCE(excluded.last_fail_at, last_fail_at),
		   updated_at = excluded.updated_at`,
		artistID, userID, successInc, failInc, successAt, failAt, now); err != nil {
		return fmt.Errorf("upsert artist_user_reliability: %w", err)
	}

	return tx.Commit()
}

// ReliabilityFor batch-looks-up reliability history for a set of usernames
// against one artist: the global (known_users) row for every username that
// has one, plus the artist-specific (artist_user_reliability) row when
// artistID > 0. One query per scope regardless of how many usernames are
// asked for (not one query per user), since this runs once per startJob call
// against every candidate the search returned. Usernames with no recorded
// history at either scope are simply absent from the returned map; callers
// look up with the map's zero value (an empty PeerReliability), which is the
// correct "no history" signal.
func (s *Store) ReliabilityFor(ctx context.Context, artistID int64, usernames []string) (map[string]core.PeerReliability, error) {
	out := make(map[string]core.PeerReliability, len(usernames))
	if len(usernames) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(usernames))
	args := make([]any, len(usernames))
	for i, u := range usernames {
		placeholders[i] = "?"
		args[i] = u
	}
	inClause := strings.Join(placeholders, ",")

	globalRows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT username, success_count, fail_count, last_success_at, last_fail_at
		 FROM known_users WHERE username IN (%s)`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("query known_users: %w", err)
	}
	defer globalRows.Close()
	for globalRows.Next() {
		var username string
		var c core.ReliabilityCounters
		if err := globalRows.Scan(&username, &c.SuccessCount, &c.FailCount, &c.LastSuccessAt, &c.LastFailAt); err != nil {
			return nil, fmt.Errorf("scan known_users: %w", err)
		}
		out[username] = core.PeerReliability{Global: c}
	}
	if err := globalRows.Err(); err != nil {
		return nil, err
	}

	if artistID <= 0 {
		return out, nil
	}

	artistArgs := append([]any{artistID}, args...)
	artistRows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT ku.username, aur.success_count, aur.fail_count, aur.last_success_at, aur.last_fail_at
		 FROM artist_user_reliability aur
		 JOIN known_users ku ON ku.id = aur.user_id
		 WHERE aur.artist_id = ? AND ku.username IN (%s)`, inClause), artistArgs...)
	if err != nil {
		return nil, fmt.Errorf("query artist_user_reliability: %w", err)
	}
	defer artistRows.Close()
	for artistRows.Next() {
		var username string
		var c core.ReliabilityCounters
		if err := artistRows.Scan(&username, &c.SuccessCount, &c.FailCount, &c.LastSuccessAt, &c.LastFailAt); err != nil {
			return nil, fmt.Errorf("scan artist_user_reliability: %w", err)
		}
		pr := out[username]
		pr.Artist = c
		out[username] = pr
	}
	return out, artistRows.Err()
}
