// Package core holds the domain types shared across slskdarr. It imports no
// other internal package so every other package may depend on it freely.
package core

// AlbumJobState is the lifecycle state of one wanted album.
type AlbumJobState string

const (
	StateDiscovered  AlbumJobState = "DISCOVERED"
	StateSearching   AlbumJobState = "SEARCHING"
	StateSelecting   AlbumJobState = "SELECTING"
	StateDownloading AlbumJobState = "DOWNLOADING"
	StateVerifying   AlbumJobState = "VERIFYING"
	StateImporting   AlbumJobState = "IMPORTING"
	StateCompleted   AlbumJobState = "COMPLETED"
	StateCooldown    AlbumJobState = "COOLDOWN"
	StateFailed      AlbumJobState = "FAILED"
	StateCancelled   AlbumJobState = "CANCELLED"
	// StateWanted is the pipeline rewrite's entry state: synced from Lidarr's
	// wanted list, awaiting a Discovery search. Replaces DISCOVERED/SEARCHING.
	StateWanted AlbumJobState = "WANTED"
	// StateDone replaces COMPLETED in the pipeline rewrite.
	StateDone AlbumJobState = "DONE"
	// StateParked is an operator-visible holding state for a DOWNLOADING job
	// whose active transfer exhausted its retry budget after repeatedly
	// vanishing from the backend. PARKED is neither runnable nor terminal; a
	// manual retry or force-search returns it to WANTED, deletion removes it,
	// and WantedSync cancels it when the album stops being wanted.
	StateParked AlbumJobState = "PARKED"
	// StateOrphaned is the deprecated spelling of StateParked retained for
	// reading databases and accepting operations from before migration 0008.
	// New transitions must write StateParked instead.
	StateOrphaned AlbumJobState = "ORPHANED"
)

// Terminal reports whether the state is an end state that needs no further work
// this cycle. COOLDOWN is not terminal: it is retried after a backoff. CANCELLED
// is terminal: the album left Lidarr's wanted list, so there is nothing to do.
func (s AlbumJobState) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// PipelineTerminal reports whether the state is an end state in the pipeline
// state machine (spec 2026-07-06). FAILED is included: it is only ever left
// via WantedSync's revival or a manual dashboard retry, never by a module's
// normal advance. Distinct from Terminal() (the legacy engine's notion) until
// the engine is deleted.
func (s AlbumJobState) PipelineTerminal() bool {
	return s == StateDone || s == StateCancelled || s == StateFailed
}

// TransferState mirrors slskd's transfer states, plus STALLED which slskdarr
// derives itself from lack of byte progress.
type TransferState string

const (
	// TransferPending is recorded before a file is sent to slskd: the intent is
	// persisted so it survives a restart, but slskd has not been asked to
	// download it yet. The engine promotes PENDING files to QUEUED a few at a
	// time (see MaxInflightPerPeer) so a burst never trips a peer's per-user
	// queued-megabyte limit. Reconciliation ignores PENDING transfers.
	TransferPending    TransferState = "PENDING"
	TransferQueued     TransferState = "QUEUED"
	TransferInProgress TransferState = "IN_PROGRESS"
	TransferCompleted  TransferState = "COMPLETED"
	TransferErrored    TransferState = "ERRORED"
	TransferCancelled  TransferState = "CANCELLED"
	TransferStalled    TransferState = "STALLED"
)

// JobEventType identifies the kind of pipeline decision recorded in a job's
// audit trail (see store.AddJobEvent), surfaced by the dashboard's per-job
// detail panel and global event timeline.
type JobEventType string

const (
	EventSearch            JobEventType = "search"
	EventSearchFallback    JobEventType = "search_fallback"
	EventCandidateSelected JobEventType = "candidate_selected"
	EventCandidateRejected JobEventType = "candidate_rejected"
	EventAttemptFailed     JobEventType = "attempt_failed"
	EventAttemptSucceeded  JobEventType = "attempt_succeeded"
	EventTransferStalled   JobEventType = "transfer_stalled"
	EventImportOK          JobEventType = "import_ok"
	EventImportRejected    JobEventType = "import_rejected"
	EventDedup             JobEventType = "dedup"
	EventJobFailed         JobEventType = "job_failed"
)

// CandidateState is the lifecycle of one cached candidate (see core.Candidate).
type CandidateState string

const (
	CandidateNew       CandidateState = "NEW"
	CandidateActive    CandidateState = "ACTIVE"
	CandidateSucceeded CandidateState = "SUCCEEDED"
	CandidateFailed    CandidateState = "FAILED"
)

// JobSource identifies where an AlbumJob came from: the Lidarr wanted-sync
// (the default pipeline) or a manual POST /api/jobs request (issue #155).
type JobSource string

const (
	SourceLidarr JobSource = "lidarr"
	SourceManual JobSource = "manual"
)
