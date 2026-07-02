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
)

// Terminal reports whether the state is an end state that needs no further work
// this cycle. COOLDOWN is not terminal: it is retried after a backoff. CANCELLED
// is terminal: the album left Lidarr's wanted list, so there is nothing to do.
func (s AlbumJobState) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
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
