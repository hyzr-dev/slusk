package soulseek

import "time"

// ConnState is the coarse connection state of a Client's connection to the
// Soulseek server.
type ConnState string

const (
	// StateDisconnected means no connection attempt is currently in progress
	// or established; the client is between attempts (e.g. backing off).
	StateDisconnected ConnState = "disconnected"
	// StateConnecting means a dial and login are in progress.
	StateConnecting ConnState = "connecting"
	// StateConnected means login succeeded and the client is exchanging
	// messages with the server.
	StateConnected ConnState = "connected"
	// StateFailed means a terminal error occurred (e.g. invalid credentials
	// or an outdated protocol version) and the client has given up
	// reconnecting.
	StateFailed ConnState = "failed"
)

// Status is an immutable snapshot of a Client's connection state, safe to
// read concurrently with the Client's Run loop (see Client.Status).
type Status struct {
	State               ConnState
	LastAttempt         time.Time
	LastConnectedAt     time.Time
	LastError           string
	LastErrorAt         time.Time
	ConsecutiveFailures int
}
