package core

import "time"

// AlbumJob is the unit of user-visible work: one wanted album from Lidarr.
type AlbumJob struct {
	ID              int64
	LidarrAlbumID   int64
	State           AlbumJobState
	CandidatesTried int
	NextAttemptAt   *time.Time // set while in COOLDOWN
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Title           string // cached from Lidarr at discovery time, for display only
	ArtistName      string // cached from Lidarr at discovery time, for display only
}

// CandidateAttempt is one ranked Soulseek user tried for an album.
type CandidateAttempt struct {
	ID           int64
	AlbumJobID   int64
	Username     string
	Score        float64
	State        string // PENDING/ACTIVE/SUCCEEDED/FAILED
	FailReason   string // timeout/errored/cancelled/incomplete
	BackoffUntil *time.Time
	CreatedAt    time.Time
}

// Transfer is one slskd file download. SlskdID is empty until slskd accepts the
// enqueue; (Username, Filename) is the fallback correlation key until then.
type Transfer struct {
	ID             int64
	AttemptID      int64
	SlskdID        string
	Username       string
	Filename       string
	State          TransferState
	BytesDone      int64
	BytesTotal     int64
	Deadline       time.Time
	LastProgressAt *time.Time
	UpdatedAt      time.Time
}
