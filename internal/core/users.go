package core

import "time"

// User is one row of the users table (issue #279, form-based login).
//
// PasswordHash is bcrypt output and is populated only by store lookups the
// auth package needs for verification (UserByName); it must never leave
// internal/observ's auth code and must never be marshalled into an API
// response.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}
