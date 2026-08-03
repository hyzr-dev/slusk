# Browser sessions are database-backed opaque tokens, not signed ones

A browser session is a 256-bit random value from `crypto/rand`; only its SHA-256 hash is
stored, in `user_sessions`, and every request resolves the cookie by looking that hash up
(`app.createSession` / `app.SessionUser`). The server is therefore the sole authority on
whether a session is valid, which is why slusk has **no signing key and no JWT** — and
must not grow one. There is nothing to sign: an opaque token carries no claims, so a
`session_secret` in `config.toml` would be a key nothing reads.

## Consequences

- Each instance is already isolated without configuration: a session is only valid against
  the database that holds its row. Two instances share sessions if and only if they share a
  database, which is the behaviour we want in both directions.
- A database dump yields no usable sessions, only hashes.
- Revocation is immediate and real — `DELETE FROM users` cascades to `user_sessions`, which
  is what makes "delete the row and start over" a working recovery path (see
  `0011_users_and_sessions.sql`). A signed stateless token could not be revoked before it
  expired without reintroducing exactly the server-side lookup this design already has.
- The cost is a database round-trip per authenticated request. Accepted: slusk is a
  single-instance homelab service, and the same request already touches Postgres for its
  actual work.

## Considered options

**Signed stateless tokens (JWT).** Rejected. The one thing they buy — validating a session
without touching the database — is worthless at this scale, and they charge for it in key
management (a key to generate, store, and rotate) and in revocation, which stops being free.

Recorded because a reader who sees a login form will reasonably assume a signing key exists
somewhere and go looking for the bug of it being missing. It is not missing; it is not
needed.
