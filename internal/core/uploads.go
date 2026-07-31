package core

import "time"

// UploadStatus is how one upload to a peer ended. It lives here rather than in
// internal/soulseek because both the native client (which produces it) and the
// store (which persists it) need the same three values, and two independently
// maintained copies of the same enum drift.
//
// The three values are deliberately coarse: the specific reason belongs in
// UploadHistoryEntry.Detail as free text, so a new failure path in the client
// never requires a schema change.
type UploadStatus string

const (
	// UploadCompleted means the whole file reached the peer.
	UploadCompleted UploadStatus = "completed"
	// UploadAborted means streaming began and then died — a dropped
	// connection, a write error, or the peer reading below the minimum
	// throughput.
	UploadAborted UploadStatus = "aborted"
	// UploadRejected means not a byte was ever streamed: the file was gone or
	// unreadable, the peer session could not be established, negotiation timed
	// out, or the peer declined. Such a row has BytesSent and
	// AvgBytesPerSecond both 0, which is a true measurement rather than a
	// missing one — render it as "—", not as "0 B/s".
	UploadRejected UploadStatus = "rejected"
)

// UploadHistoryEntry is one persisted row of upload_history (issue #325): a
// single finished upload to a peer, written once its outcome is decided.
//
// Filename is the virtual share path, never the local one, and Detail is a
// short fixed reason string, never a raw error — both are served over the API
// and an upload error routinely wraps a local filesystem path (see the
// migration's comment).
//
// BytesSent counts only what this attempt put on the wire, so a resumed upload
// does not report the bytes an earlier attempt already delivered.
// AvgBytesPerSecond is BytesSent over the streaming phase's own duration and is
// therefore NOT derivable from FinishedAt-StartedAt, which also covers queueing
// and negotiation.
type UploadHistoryEntry struct {
	ID                int64
	Username          string
	Filename          string
	Size              uint64
	BytesSent         uint64
	AvgBytesPerSecond uint64
	Status            UploadStatus
	Detail            string
	StartedAt         time.Time
	FinishedAt        time.Time
}
