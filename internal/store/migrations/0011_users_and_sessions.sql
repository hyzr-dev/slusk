-- Accounts and sessions for form-based login (issue #279). Before this,
-- observ.auth_token was the only credential and the dashboard showed a native
-- Basic-auth dialog instead of a login form. Now a browser session is
-- established by POSTing username/password, while auth_token remains valid
-- (and optional) for machine access.
--
-- Deliberately no foreign key FROM any other table TO users or user_sessions.
-- v1 has no password reset, so "delete the row and start over" is the
-- supported recovery path: `DELETE FROM users;` cascades only to
-- user_sessions and destroys no pipeline data. If some future feature wants
-- to attribute pipeline rows to a user, that FK is a deliberate later
-- decision, not an accident of this migration.
CREATE TABLE IF NOT EXISTS users (
  id            BIGSERIAL PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- token_hash is sha256(session token); the raw token only ever exists in the
-- browser's cookie and in memory during a request, so a dump of this table
-- yields no usable sessions.
CREATE TABLE IF NOT EXISTS user_sessions (
  token_hash BYTEA PRIMARY KEY,
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_sessions_expires ON user_sessions (expires_at);
