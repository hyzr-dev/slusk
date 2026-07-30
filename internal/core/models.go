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
	ReleaseDate     string // cached from Lidarr at discovery time, for display/ordering only
	// ArtistID is Lidarr's artist id, cached alongside the display metadata. It
	// keys per-artist peer reliability (artist_user_reliability) when an attempt
	// completes, so the job needs it long after the wanted-list entry that
	// supplied it is gone. 0 when unknown (e.g. an older job not yet backfilled).
	ArtistID int64
	// Retries counts search-cycle failures (empty search, candidates
	// exhausted). Per-candidate failures do not touch it. At max_retries the
	// job goes FAILED. Reset to 0 when a search yields candidates.
	Retries int
	// NotBefore hides the job from every pipeline module until it passes.
	// Backoff lives here as data — there is no COOLDOWN state.
	NotBefore *time.Time
	// FailedAt is set when the job enters FAILED; WantedSync revives the job
	// once it is older than failed_revive_after and the album is still wanted.
	FailedAt *time.Time
	// MinTrackCount/MaxTrackCount is the album's valid track-count band across
	// all Lidarr releases (editions), cached by Discovery at search time.
	// Importing's coverage gate accepts any candidate covering at least
	// MinTrackCount tracks. (0,0) means unknown — the gate then falls back to
	// the live AlbumStatus total.
	MinTrackCount int
	MaxTrackCount int
	// Source distinguishes a Lidarr wanted-sync job from a manually created one
	// (POST /api/jobs, issue #155). Manual jobs have no LidarrAlbumID and are
	// invisible to WantedSync's cancel/revive/refresh logic.
	Source JobSource
	// Year, Tracks and Format are candidate metadata captured at the
	// SELECTING -> DOWNLOADING transition (ActivateCandidateWithTransfers),
	// for display only. All three are nil until that transition runs — and
	// stay nil for manual jobs, which are created directly in DOWNLOADING and
	// never pass through SELECTING. Year is additionally nil when the job's
	// release_date is unparseable. See issue #156.
	Year   *int
	Tracks *int
	Format *string
}

// CandidateFile is one file of a cached search result, persisted as JSONB on
// the candidate so Selecting can enqueue it long after the search happened.
type CandidateFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// Candidate is one ranked Soulseek user cached for an album: a candidate is
// its own attempt, NEW (cached, untried) → ACTIVE (picked by Selecting) →
// SUCCEEDED | FAILED.
type Candidate struct {
	ID                int64
	AlbumJobID        int64
	Username          string
	Score             float64
	Files             []CandidateFile
	State             CandidateState
	FailReason        string
	ImportSubmittedAt *time.Time // set by Importing after ExecuteManualImport; gates verify vs confirm phase
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Transfer is one slskd file download. SlskdID is empty until slskd accepts the
// enqueue; (Username, Filename) is the fallback correlation key until then.
type Transfer struct {
	ID             int64
	CandidateID    int64
	SlskdID        string
	Username       string
	Filename       string
	State          TransferState
	BytesDone      int64
	BytesTotal     int64
	Retries        int
	Deadline       time.Time
	LastProgressAt *time.Time
	UpdatedAt      time.Time
}

// ReliabilityCounters is one peer's raw success/fail history at a single scope
// (either global across all artists, or for one specific artist). The counters
// and timestamps are stored as-is; decay/recency weighting is computed in Go at
// ranking time (see matcher.ReliabilityHistoryScore) rather than stored as a
// pre-aged score, so no background job is needed to "age" the numbers. The zero
// value (both counts 0, both timestamps nil) means no recorded history.
type ReliabilityCounters struct {
	SuccessCount  int
	FailCount     int
	LastSuccessAt *time.Time
	LastFailAt    *time.Time
}

// PeerReliability bundles a Soulseek peer's history for one artist lookup: the
// artist-specific record (the strong signal, preferred) and the global record
// aggregated over every artist (a weak fallback used when there is no
// artist-specific history). Either side may be a zero value when that peer has
// no outcome recorded at that scope.
//
// These two records are the ONLY peer history that survives a failed-album
// retry: ResetJobToWanted DELETEs candidates, so reliability must be written
// incrementally at candidate completion, never recomputed from candidates.
type PeerReliability struct {
	Artist ReliabilityCounters
	Global ReliabilityCounters
}

// JobView is a read-only projection joining an AlbumJob with its current
// candidate's aggregated transfer progress, for display purposes only (e.g.
// the dashboard). It is never written back to the store.
type JobView struct {
	Job AlbumJob
	// Peer is the current candidate's username (store.jobViewFrom's a.username)
	// — the album's peer, unambiguous since a job has exactly one current
	// candidate (see store.currentCandidateOrder). "" if Attempt is nil.
	Peer string
	// Status is the dashboard's coarse display status (queued/active/
	// stalled/importing/failed/parked/done), computed once by the store's
	// dashboardJobStatusSQL so that a job's rendered status and every
	// filter/facet/sort built around it can never disagree (issue #269).
	Status  string
	Attempt *Candidate // nil if the job has no candidate yet
	// AlbumBytesDone and AlbumBytesTotal are summed over every transfer of
	// the job's current candidate (Attempt) — i.e. every file of the album,
	// since candidate activation (ActivateCandidateWithTransfers) and manual
	// job creation (CreateManualJob) both write-ahead a PENDING transfers row
	// per file with bytes_total set from the file size. Unlike Transfer,
	// which reflects only the single most recently updated file, these
	// describe album-wide progress and are what the dashboard should render
	// as the job's progress bar (issue #174).
	//
	// AlbumBytesRemaining is not the same sum: it is
	// SUM(GREATEST(bytes_total-bytes_done, 0)) per transfer, filtered to
	// non-terminal states (excludes COMPLETED, ERRORED, CANCELLED) — the
	// GREATEST clamp keeps a single over-reported transfer from making the
	// album-wide remaining go negative.
	//
	// All three are zero when the job has no candidate.
	AlbumBytesDone      int64
	AlbumBytesTotal     int64
	AlbumBytesRemaining int64
}

// AttemptDetail bundles one candidate with every transfer (one per file) it
// produced, for the dashboard's per-job detail panel.
type AttemptDetail struct {
	Attempt   Candidate
	Transfers []Transfer
}

// JobDetail is the full history of one job: the job itself plus every
// candidate made for it (newest first) and each candidate's per-file
// transfers. Used by the dashboard's job detail panel (GET
// /api/jobs/{id}/detail); never written back to the store.
type JobDetail struct {
	Job      AlbumJob
	Attempts []AttemptDetail
}

// JobEvent is one row of a job's audit trail (see store.AddJobEvent),
// surfaced by the dashboard's per-job detail panel and global event timeline.
type JobEvent struct {
	ID         int64
	AlbumJobID int64
	Event      JobEventType
	Detail     string
	CreatedAt  time.Time
}

// SearchPass records one completed Discovery search cycle (a "pass"):
// Searched counts wanted albums examined, Matched counts albums that found
// viable candidates and advanced to SELECTING. Backs the Overview charts
// (GET /api/charts, issue #88).
type SearchPass struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt time.Time
	Searched   int
	Matched    int
}

// HourCount is one hour bucket of an event-count aggregation, e.g. completed
// downloads per hour for the Overview charts (GET /api/charts).
type HourCount struct {
	Hour  time.Time
	Count int
}

// PeerRow is one Soulseek peer's global reliability plus every artist-specific
// row recorded for them, read by the dashboard's Peers view (GET /api/peers).
// Score computation is left to the caller (see matcher.ReliabilityHistoryScore)
// so the dashboard reuses the exact formula the ranker uses rather than
// duplicating it.
type PeerRow struct {
	Username string
	Global   ReliabilityCounters
	Artists  map[int64]ReliabilityCounters // keyed by artist_id
}
