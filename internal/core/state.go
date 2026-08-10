// Package core holds the domain types shared across slusk. It imports no
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
	// StateParked is an operator-visible holding state for a job set aside
	// because no candidate could satisfy it - distinct from failed: parked
	// jobs await a person's decision, failed jobs await a retry. Two paths
	// reach it today: a DOWNLOADING job whose active transfer exhausted its
	// retry budget after repeatedly vanishing from the backend, and an
	// IMPORTING job that has had N candidates rejected for the same content
	// fault (issue #472). PARKED is neither runnable nor terminal; a manual
	// retry or force-search returns it to WANTED, deletion removes it, and
	// WantedSync cancels it when the album stops being wanted.
	StateParked AlbumJobState = "PARKED"
	// StateOrphaned is the deprecated spelling of StateParked retained for
	// reading databases and accepting operations from before migration 0008.
	// New transitions must write StateParked instead.
	StateOrphaned AlbumJobState = "ORPHANED"
	// StateNotImported is the terminal outcome of a manual job (issue #59)
	// whose download completed but that never reached Lidarr: either the
	// user never identified it against a MusicBrainz release group
	// (AlbumMBID is empty), or the identified release group is not in
	// Lidarr's library (lidarr.Client.AlbumByForeignID reported not-found).
	// The downloaded files are left on disk exactly as they landed - no
	// cleanup runs, since they are the deliverable. This is NOT a failure:
	// it must never be retried, revived, or otherwise treated as an error
	// condition. Only a manual job can reach it; a Lidarr-sourced job always
	// has a real LidarrAlbumID by construction.
	StateNotImported AlbumJobState = "NOT_IMPORTED"
	// StateImportRefused is the terminal outcome of a job whose download was
	// complete and correct and which Lidarr permanently refused to accept
	// (issue #470). The files are kept, under CompleteDir/_import_rejected/,
	// and the job awaits a person.
	//
	// It is deliberately none of the other three terminals. FAILED means the
	// candidate cache was exhausted and is revived on a schedule; nothing was
	// exhausted here. NOT_IMPORTED is manual-job-only and explicitly must never
	// be retried, where this is resolvable - fix the tags, add the release in
	// Lidarr - so it must stay retryable. PARKED's "awaits a person's decision"
	// fits but its "no candidate could satisfy it" does not: the candidate was
	// fine and Lidarr said no.
	//
	// Named REFUSED rather than REJECTED on purpose. EventImportRejected
	// already exists and is written every time a candidate is rejected and the
	// job moves on to the next one - the opposite outcome. One job on the
	// canary carries 59 of them and was never terminal. See docs/adr.
	//
	// There is no automatic revival: SyncWantedJobs' revive acts only on
	// FAILED, and a timer that decides for the user contradicts what the state
	// means. ForceSearch is the escape hatch and already accepts it.
	StateImportRefused AlbumJobState = "IMPORT_REFUSED"
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
	return s == StateDone || s == StateCancelled || s == StateFailed ||
		s == StateNotImported || s == StateImportRefused
}

// TransferState mirrors slskd's transfer states, plus STALLED which slusk
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
	EventSearchExcluded    JobEventType = "search_excluded"
	EventCandidateSelected JobEventType = "candidate_selected"
	EventCandidateRejected JobEventType = "candidate_rejected"
	// EventCandidateDeferred records a candidate waiting because another job
	// owns the download folder its peer's files would land in (issue #471).
	// Deliberately not EventCandidateRejected: nothing is wrong with the
	// candidate, and the two must stay distinguishable because a rejection is
	// permanent (#317) while this clears as soon as the owner is done. The
	// detail names the owning job.
	EventCandidateDeferred JobEventType = "candidate_deferred"
	EventAttemptFailed     JobEventType = "attempt_failed"
	EventAttemptSucceeded  JobEventType = "attempt_succeeded"
	EventTransferStalled   JobEventType = "transfer_stalled"
	EventImportOK          JobEventType = "import_ok"
	EventImportRejected    JobEventType = "import_rejected"
	EventDedup             JobEventType = "dedup"
	EventJobFailed         JobEventType = "job_failed"
	EventQuarantined       JobEventType = "quarantined"
	// EventNotImported records a manual job's download completing with no
	// Lidarr album to import into (issue #59, StateNotImported) - either no
	// MusicBrainz release group was ever identified, or the identified one
	// is not in Lidarr's library. Deliberately distinct from
	// EventAttemptFailed/EventJobFailed: this is not a failure.
	EventNotImported JobEventType = "not_imported"
	// EventJobParked records a job entering StateParked, whichever path took
	// it there (issue #472). Parking is the most operator-relevant transition
	// in the system - it is where automation stops and waits for a person -
	// and until this event existed the timeline was silent about it.
	EventJobParked JobEventType = "job_parked"
	// EventImportRefused records a job reaching StateImportRefused: Lidarr
	// permanently refused the files and they were moved to the
	// _import_rejected quarantine (issue #470). Distinct from
	// EventImportRejected, which is written on the ordinary path where a
	// candidate is rejected and the job goes on to try the next one - the two
	// read alike and mean opposite things, which is why the state is spelled
	// REFUSED. The detail carries Lidarr's reason and where the files went.
	EventImportRefused JobEventType = "import_refused"
)

// RejectionReason is the machine-readable key a candidate rejection is counted
// under in candidate_rejections.reason. Its values are the exact strings the
// pipeline has always written there, so the counts already in the database
// keep their meaning and no migration is needed.
//
// Not an exhaustive enumeration of that column. It names only the reasons
// something counts, which today is the two content faults the repeated-
// rejection cap acts on (issue #472). Importing's confirm-timeout path writes
// "import not confirmed" to the same column as a bare literal and deliberately
// has no constant here - it was held back from the cap's scope, and giving it
// one would imply it is in. Anything that treats this type as the full set of
// recorded reasons is wrong about the data.
//
// This is deliberately not job_events.detail: that string is written for
// humans and embeds a folder path and per-candidate numbers, so re-wording it
// would silently disable anything keyed on it.
type RejectionReason string

const (
	// ReasonImportRejected is Lidarr refusing every file in the candidate's
	// folder.
	ReasonImportRejected RejectionReason = "import rejected"
	// ReasonIncompleteDownload is the coverage gate: the candidate's files
	// cannot complete even the smallest valid edition of the album.
	ReasonIncompleteDownload RejectionReason = "incomplete download"
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
