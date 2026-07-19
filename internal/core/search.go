package core

// SearchResult is one search result file offered by a peer, enriched with the
// peer's upload-availability signals. Provider-neutral: the pipeline and
// matcher consume this rather than a wire-specific type, so they have no
// dependency on any particular Soulseek client library. Filename preserves
// the provider's own path syntax (slskd uses "\" separators).
type SearchResult struct {
	Username          string
	Filename          string
	Size              int64
	BitRate           int
	HasFreeUploadSlot bool
	QueueLength       int
	UploadSpeed       int
}

// RankedCandidate is one user offering a group of files, with an aggregate
// score assigned by the matcher.
type RankedCandidate struct {
	Username string
	Files    []SearchResult
	Score    float64
}
