package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// SelectingStore is the slice of the store Selecting needs. It embeds
// BackoffStore since exhausting a job's candidate cache goes through the same
// failOrBackoff path as every other module's search-cycle failure.
type SelectingStore interface {
	BackoffStore
	// RunnableJobsInState is used with StateSelecting to pick this tick's
	// batch of jobs (see RunnableJobsInState's doc comment for ordering).
	RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
	// NextNewCandidate returns a job's best untried cached candidate.
	NextNewCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error)
	// ActivateCandidateWithTransfers atomically re-checks ownership, lifecycle
	// eligibility, and MaxActive, then creates the complete PENDING transfer set
	// before activating the candidate and advancing its job to DOWNLOADING.
	// deadline is when those transfers become overdue; topUpCandidate rewrites
	// it per file as each one is actually enqueued.
	ActivateCandidateWithTransfers(ctx context.Context, candidateID, jobID int64, maxActive int, deadline, now time.Time) (activated, capFull bool, err error)
	// DeferSelectingJob moves a candidate-specific skip behind FIFO peers so a
	// full batch of live-owner conflicts cannot starve later unrelated jobs.
	DeferSelectingJob(ctx context.Context, jobID int64, now time.Time) error
	// DownloadFoldersForJob and MarkDownloadFolderCleaned are the register
	// quarantineLeftovers reads once a job has terminally FAILED, to find every
	// folder the job downloaded into — across all of its search cycles, not just
	// the last one (issue #314).
	DownloadFoldersForJob(ctx context.Context, jobID int64) ([]string, error)
	MarkDownloadFolderCleaned(ctx context.Context, jobID int64, leaf string, now time.Time) error

	// The remaining methods are topUpDeps's store half (see topUpCandidate):
	// declared again here, rather than embedding topUpDeps directly, so this
	// interface's doc comment stays the single place documenting what
	// Selecting needs from the store.
	TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)
	RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, bool, error)
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error
	AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (bool, error)
	RegisterDownloadFolder(ctx context.Context, jobID int64, leaf string, now time.Time) (int64, bool, error)
	DeferCandidate(ctx context.Context, candidateID int64, now time.Time) (time.Time, bool, error)
	ClearCandidateDeferral(ctx context.Context, candidateID int64) error
	// FailCandidateAndAdvance is the ceiling on a deferred candidate's wait
	// (issue #471); Downloading uses the same method for transfer failures.
	FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error)
}

// SelectingParams configures a Selecting.
type SelectingParams struct {
	Store SelectingStore
	Peers PeerSearcher

	MaxActive  int
	MaxRetries int

	BackoffBase time.Duration
	BackoffCap  time.Duration

	// CandidateTTL bounds how long a cached NEW candidate may sit unactivated
	// before Selecting discards the whole cache and sends the job back to
	// WANTED for a fresh search: a search result this old (peer gone offline,
	// shared files renamed/removed) is no longer trustworthy enough to enqueue
	// blind.
	CandidateTTL time.Duration

	MaxInflightPerPeer int
	MaxTransferRetries int
	TransferDeadline   time.Duration

	Interval time.Duration

	// CompleteDir is the download root (paths.slskd_complete_dir). Selecting
	// only needs it when a job fails terminally, to move whatever the job left
	// behind into the quarantine subdirectory. Empty disables quarantining.
	CompleteDir string

	Logger *slog.Logger
}

// TransfersForCandidate, RecordEnqueueIntent, RetryTransfer,
// UpdateTransferProgress, AttachTransferID and Enqueue forward to Store and
// Peers so SelectingParams itself satisfies topUpDeps (see selecting.go's
// topUpDeps doc comment) - Downloading (task 9) does the same on its own
// params struct, keeping the two modules' wiring independent rather than
// sharing one deps struct.
func (p SelectingParams) TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error) {
	return p.Store.TransfersForCandidate(ctx, candidateID)
}

func (p SelectingParams) RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, bool, error) {
	return p.Store.RecordEnqueueIntent(ctx, candidateID, username, filename, deadline, now)
}

func (p SelectingParams) RetryTransfer(ctx context.Context, transferID int64, now time.Time) error {
	return p.Store.RetryTransfer(ctx, transferID, now)
}

func (p SelectingParams) UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error {
	return p.Store.UpdateTransferProgress(ctx, transferID, state, bytesDone, bytesTotal, now)
}

func (p SelectingParams) AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (bool, error) {
	return p.Store.AttachTransferID(ctx, transferID, remoteID, now)
}

func (p SelectingParams) RegisterDownloadFolder(ctx context.Context, jobID int64, leaf string, now time.Time) (int64, bool, error) {
	return p.Store.RegisterDownloadFolder(ctx, jobID, leaf, now)
}

func (p SelectingParams) DeferCandidate(ctx context.Context, candidateID int64, now time.Time) (time.Time, bool, error) {
	return p.Store.DeferCandidate(ctx, candidateID, now)
}

func (p SelectingParams) ClearCandidateDeferral(ctx context.Context, candidateID int64) error {
	return p.Store.ClearCandidateDeferral(ctx, candidateID)
}

func (p SelectingParams) FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error) {
	return p.Store.FailCandidateAndAdvance(ctx, candidateID, jobID, reason, from, to, now)
}

func (p SelectingParams) AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error {
	return p.Store.AddJobEvent(ctx, jobID, event, detail, now)
}

func (p SelectingParams) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	return p.Peers.Enqueue(ctx, username, filename, size)
}

func (p SelectingParams) Cancel(ctx context.Context, username, id string) error {
	return p.Peers.Cancel(ctx, username, id)
}

// Selecting activates one cached candidate per runnable SELECTING job per
// tick (bounded by MaxActive), or backs the job off when its candidate cache
// is exhausted or stale. It ports the back half of the legacy engine's
// startJob (candidate selection/enqueue; the front half - searching and
// caching - is Discovery's).
type Selecting struct {
	p SelectingParams
}

// NewSelecting constructs a Selecting.
func NewSelecting(p SelectingParams) *Selecting {
	if p.Logger != nil {
		p.Logger = p.Logger.With("module", "selecting")
	}
	return &Selecting{p: p}
}

// Name identifies this module in logs and Health().
func (s *Selecting) Name() string { return "selecting" }

// Interval is how often this module ticks.
func (s *Selecting) Interval() time.Duration { return s.p.Interval }

func (s *Selecting) log() *slog.Logger {
	if s.p.Logger != nil {
		return s.p.Logger
	}
	return slog.Default()
}

// recordEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it is
// logged at warn level and swallowed rather than propagated (same pattern as
// Discovery.recordEvent).
func (s *Selecting) recordEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) {
	if err := s.p.Store.AddJobEvent(ctx, jobID, event, detail, now); err != nil {
		s.log().Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
	}
}

// Tick processes every runnable SELECTING job, up to MaxActive of them (the
// MaxActive cap on concurrently DOWNLOADING+IMPORTING jobs means at most
// MaxActive activations could ever succeed in one tick anyway, so a bigger
// batch would only mean wasted NextNewCandidate/ActivateCandidate round trips
// once the cap fills - see selectJob's early-stop below).
func (s *Selecting) Tick(ctx context.Context, now time.Time) error {
	jobs, err := s.p.Store.RunnableJobsInState(ctx, core.StateSelecting, now, s.p.MaxActive)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		proceed, err := s.selectJob(ctx, job, now)
		if err != nil {
			return err
		}
		if !proceed {
			break
		}
	}
	return nil
}

// selectJob tries to activate one job's best cached candidate. The returned
// bool tells Tick whether to keep working the rest of this tick's batch: false
// only when ActivateCandidateWithTransfers reports the shared MaxActive cap is
// full, since every later job would hit the same cap. Candidate-specific skips
// are deferred behind FIFO peers and return true so unrelated work continues.
func (s *Selecting) selectJob(ctx context.Context, job core.AlbumJob, now time.Time) (bool, error) {
	cand, ok, err := s.p.Store.NextNewCandidate(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		// No NEW candidate left to try: every cached candidate from the last
		// search has already been activated and failed.
		//
		// A manual job (issue #155) never gets a re-search: the user picked one
		// specific peer, often for a reason the protocol carries nowhere (a FLAC
		// rip, a bitrate, a particular edition), and Discovery (issue #347)
		// refuses to search on a manual job's behalf anyway. So this goes
		// straight to MarkJobFailed instead of failOrBackoff, on the first failure -
		// retries is never consumed. A manual job can also arrive here via a
		// genuine IMPORTING -> SELECTING bounce (Importing.failCandidate,
		// e.g. Lidarr rejected the import) - that path is unrelated to #59
		// now: #59's own failure mode (AlbumStatus(0) erroring on every tick
		// because a manual job has no LidarrAlbumID) no longer happens -
		// Importing resolves AlbumMBID to a real Lidarr album before it ever
		// calls AlbumStatus, and a job with no identified album is routed to
		// the terminal core.StateNotImported by Downloading before it even
		// reaches IMPORTING.
		if job.Source == core.SourceManual {
			detail := "manual job candidate failed, not re-searching"
			s.log().Info(detail, "album_job", job.ID)
			if err := s.p.Store.MarkJobFailed(ctx, job.ID, now); err != nil {
				return false, err
			}
			s.recordEvent(ctx, job.ID, core.EventJobFailed, detail, now)
			s.quarantineLeftovers(ctx, job, now)
			return true, nil
		}
		// A Lidarr-sourced job's search-cycle failure goes through the same
		// failOrBackoff path as every other module, but with resetToWanted=true:
		// ResetJobToWanted wipes the (now-useless) candidate cache and its
		// transfers and sends the job back to WANTED for a fresh search, rather
		// than leaving it stuck in SELECTING with nothing left to try.
		detail := "candidates exhausted"
		s.log().Info(detail+", re-searching unless the retry budget is spent", "album_job", job.ID)
		// Recorded BEFORE failOrBackoff, and this ordering is load-bearing.
		// candidate_rejected and job_failed are both in the store's
		// failureExplainingEvents, and one pipeline pass shares a single now,
		// so LatestFailureDetails separates them on `id DESC` alone. Written
		// afterwards, this row would outrank the terminal job_failed and the
		// FAILED JOBS panel would show "candidates exhausted" for a job that
		// has actually given up - the exact substitution #318 exists to end.
		s.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
		// The reason handed to failOrBackoff is deliberately not the detail
		// above: on the terminal branch - the only branch that reads it - the
		// job is not re-searching, and it needs to explain itself to someone
		// who has not read the rest of the trail.
		failed, err := failOrBackoff(ctx, s.p.Store, s.log(), job, s.p.MaxRetries, s.p.BackoffBase, s.p.BackoffCap, true,
			"every candidate found for this album was tried and failed to download", now)
		if err != nil {
			return false, err
		}
		if failed {
			s.quarantineLeftovers(ctx, job, now)
		}
		return true, nil
	}

	// The Source guard exists for a manual job's retry (#347's RetryManualJob):
	// a fresh manual job is created ACTIVE, straight into DOWNLOADING, so it
	// never has a NEW candidate here - but a retried one does, and its
	// candidate's created_at is by definition old, so a retry a day later
	// (CandidateTTL defaults to 24h) would otherwise hit this branch. Staleness
	// only matters because a fresh search is a better option than a stale
	// candidate; a manual job has no fresh search to fall back to - the
	// candidate is the user's explicit choice and the only thing to try. If the
	// peer has gone offline, enqueuing it simply fails and the exhaustion
	// branch above ends the job properly.
	if job.Source != core.SourceManual && now.Sub(cand.CreatedAt) > s.p.CandidateTTL {
		// The cached candidate is too old to trust: the peer may have gone
		// offline or renamed/removed the shared files since the search ran.
		// Discard the whole cache and re-search from scratch, same as
		// exhaustion, but retries is left UNCHANGED - staleness is not a search
		// failure, and penalizing the retry budget for a job that simply sat in
		// SELECTING too long (behind other jobs at the MaxActive cap) would be
		// wrong.
		detail := "candidate cache expired, re-searching"
		s.log().Info(detail, "album_job", job.ID, "candidate", cand.ID)
		if err := s.p.Store.ResetJobToWanted(ctx, job.ID, core.StateSelecting, job.Retries, nil, now); err != nil {
			return false, err
		}
		s.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
		return true, nil
	}

	activated, capFull, err := s.p.Store.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, s.p.MaxActive, now.Add(s.p.TransferDeadline), now)
	if err != nil {
		return false, err
	}
	if !activated {
		// Only a full shared cap blocks the rest of the FIFO batch. Candidate-
		// specific skips (job left SELECTING, or another live candidate owns the
		// same remote file) are moved behind their FIFO peers so even a whole
		// batch of conflicts cannot starve unrelated jobs on later ticks.
		if !capFull {
			if err := s.p.Store.DeferSelectingJob(ctx, job.ID, now); err != nil {
				return false, err
			}
		}
		s.log().Info("candidate activation skipped",
			"album_job", job.ID, "candidate", cand.ID, "cap_full", capFull)
		return !capFull, nil
	}

	sent, err := topUpCandidate(ctx, s.p, job.ID, cand.ID, now, s.p.MaxInflightPerPeer, s.p.MaxTransferRetries, s.p.TransferDeadline, s.log())
	if err != nil {
		return false, err
	}

	selectedDetail := fmt.Sprintf("enqueued candidate %s, downloading (%d files, %d sent, %d deferred)",
		cand.Username, len(cand.Files), sent, len(cand.Files)-sent)
	s.log().Info(selectedDetail, "album_job", job.ID, "user", cand.Username,
		"files", len(cand.Files), "sent", sent, "deferred", len(cand.Files)-sent)
	s.recordEvent(ctx, job.ID, core.EventCandidateSelected, selectedDetail, now)
	return true, nil
}

// quarantineLeftovers moves whatever a terminally FAILED job left in the
// download root into the quarantine subdirectory, so a job nothing will ever
// pick up again does not strand its partial download where Lidarr and the
// next job both scan. Usually there is nothing left - cleanupFolder already
// removed each candidate's folder as it failed - so this is a no-op far more
// often than not.
//
// The folders come from the register (issue #314), which closes the two holes
// the old per-candidate derivation left: a job terminated from Discovery has no
// candidates or transfers to derive from at all, and ResetJobToWanted deletes
// both on every non-terminal retry, so only the last search cycle's folders
// were ever reachable. The register outlives both. It also removes the N+1
// per-candidate transfer queries this used to run.
//
// It returns nothing and swallows every error: the FAILED transition has
// already committed, and no filesystem or store problem here may be allowed to
// turn that into a pipeline error.
func (s *Selecting) quarantineLeftovers(ctx context.Context, job core.AlbumJob, now time.Time) {
	if s.p.CompleteDir == "" {
		return
	}
	leaves, err := registeredFolders(ctx, s.p.Store, s.log(), job.ID, "quarantine")
	if err != nil {
		return
	}
	var moved []string
	for _, leaf := range leaves {
		dst, ok := quarantineFolder(s.log(), job.ID, s.p.CompleteDir, leaf)
		if !ok {
			continue
		}
		moved = append(moved, dst)
		// The folder is no longer where it was registered, so nothing should go
		// looking for it there again.
		markCleaned(ctx, s.p.Store, s.log(), job.ID, leaf, now)
	}
	if len(moved) == 0 {
		return
	}
	s.recordEvent(ctx, job.ID, core.EventQuarantined,
		fmt.Sprintf("moved leftover files to %s", strings.Join(moved, ", ")), now)
}

// topUpDeps is the store+peer behavior topUpCandidate needs: exactly enough
// to hand a candidate's PENDING files to slskd and record the outcome. Kept
// as a small free-standing interface rather than a shared struct so
// Selecting and Downloading (task 9) can each own their store/peer wiring
// independently - both modules' param structs (SelectingParams, and
// Downloading's equivalent) implement it by forwarding to their own Store and
// Peers fields (see the forwarding methods above SelectingParams).
type topUpDeps interface {
	TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)
	RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, bool, error)
	RegisterDownloadFolder(ctx context.Context, jobID int64, leaf string, now time.Time) (int64, bool, error)
	DeferCandidate(ctx context.Context, candidateID int64, now time.Time) (time.Time, bool, error)
	ClearCandidateDeferral(ctx context.Context, candidateID int64) error
	FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error)
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error
	AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (bool, error)
	Enqueue(ctx context.Context, username, filename string, size int64) (string, error)
	Cancel(ctx context.Context, username, id string) error
}

// topUpCandidate hands a candidate's PENDING files to slskd until
// maxInflightPerPeer transfers are in flight, sending more only as earlier
// ones finish. Keeping the queued count bounded stops the peer's per-user
// queued-megabyte limit from rejecting a burst. PENDING files carry their
// size in bytes_total (set during atomic candidate activation) so they can be
// enqueued here without the original search result in hand. Files are sent
// in filename order for deterministic progress.
//
// maxInflightPerPeer, maxTransferRetries, transferDeadline and log are passed
// explicitly rather than folded into topUpDeps: they are plain configuration,
// not behavior, so there is nothing to gain from routing them through an
// interface method.
//
// Ported from the legacy engine's topUpAttempt (engine/discovery.go:370-418),
// s/attempt/candidate/. Selecting calls this once, right after activating a
// candidate; Downloading (task 9) calls it again on every tick to keep
// topping up a still-downloading candidate as earlier files finish.
//
// This is also where the job's local download folder is registered (issue
// #314). It is the single chokepoint every enqueue on either backend passes
// through, and — unlike the backends themselves, whose Enqueue takes only
// (username, filename, size) — it has the job id in hand. Registering here
// keeps internal/soulseek and internal/slskd free of any store dependency,
// which is why jobID is threaded down rather than the register being pushed
// into them.
func topUpCandidate(ctx context.Context, d topUpDeps, jobID, candidateID int64, now time.Time, maxInflightPerPeer, maxTransferRetries int, transferDeadline time.Duration, log *slog.Logger) (int, error) {
	transfers, err := d.TransfersForCandidate(ctx, candidateID)
	if err != nil {
		return 0, err
	}
	inflight := 0
	var pending []core.Transfer
	for _, tr := range transfers {
		switch tr.State {
		case core.TransferQueued, core.TransferInProgress, core.TransferStalled:
			inflight++
		case core.TransferPending:
			pending = append(pending, tr)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Filename < pending[j].Filename })

	// Claim the folders this tick's files would land in before promoting any
	// transfer out of PENDING. Doing it inside the loop below would leave a
	// transfer QUEUED with a deadline running against a file that was never
	// handed to a peer, and — worse — the first file would already be
	// downloading into a directory another job owns by the time the second one
	// discovered the collision.
	if room := maxInflightPerPeer - inflight; room > 0 && len(pending) > 0 {
		if room > len(pending) {
			room = len(pending)
		}
		ok, err := reserveDownloadFolders(ctx, d, jobID, candidateID, pending[:room], now, transferDeadline, log)
		if err != nil || !ok {
			return 0, err
		}
	}

	deadline := now.Add(transferDeadline)
	sent := 0
	for _, p := range pending {
		if inflight >= maxInflightPerPeer {
			break
		}
		// Promote PENDING -> QUEUED with a real deadline, then hand it to the peer.
		// A false result means a cancellation barrier made this stale snapshot
		// ineligible before any remote side effect occurred.
		tid, ok, err := d.RecordEnqueueIntent(ctx, candidateID, p.Username, p.Filename, deadline, now)
		if err != nil {
			return sent, err
		}
		if !ok {
			return sent, nil
		}
		remoteID, err := d.Enqueue(ctx, p.Username, p.Filename, p.BytesTotal)
		if err != nil {
			if p.Retries < maxTransferRetries {
				log.Info("enqueue failed, returning to pending", "user", p.Username, "file", p.Filename, "retries", p.Retries, "err", err)
				if uerr := d.RetryTransfer(ctx, tid, now); uerr != nil {
					log.Error("retry transfer failed", "transfer", tid, "err", uerr)
				}
			} else {
				log.Error("enqueue failed", "user", p.Username, "file", p.Filename, "err", err)
				if uerr := d.UpdateTransferProgress(ctx, tid, core.TransferErrored, 0, 0, now); uerr != nil {
					log.Error("mark transfer errored failed", "transfer", tid, "err", uerr)
				}
			}
			continue
		}
		attached, attachErr := d.AttachTransferID(ctx, tid, remoteID, now)
		if attachErr != nil || !attached {
			// Enqueue succeeded but cancellation/delete committed before the id
			// could be attached. Compensate immediately; remote cancellation stays
			// best-effort, matching manual lifecycle actions.
			if cancelErr := d.Cancel(ctx, p.Username, remoteID); cancelErr != nil {
				log.Warn("compensating remote cancel failed", "transfer", tid, "user", p.Username, "remote_id", remoteID, "err", cancelErr)
			}
			if attachErr != nil {
				return sent, attachErr
			}
			return sent, nil
		}
		inflight++
		sent++
	}
	return sent, nil
}

// reserveDownloadFolders claims every local folder files would be written into
// for jobID, and reports whether the caller may go on to enqueue them. false
// means another job owns one of those folders right now and this candidate has
// been deferred (or, at the ceiling, failed); the error is reserved for
// problems that should abort the tick.
//
// This is the register (issue #314) doing a second job. Registering has always
// been how a folder gets named for later cleanup; since #471 it is also how a
// job acquires the right to write there at all, because neither backend lets
// slusk pick the local directory — both derive it from the peer's share path,
// so two peers sharing a directory name is enough for two jobs to collide.
//
// A register *error* still never blocks a download, the same contract
// cleanupFolder holds: an unregisterable leaf is one this file was never going
// to be written into anyway, since the backends fall back to the download root.
// Only a live owner blocks.
//
// The leaf is derived per file rather than once over the whole set, which keeps
// the registered names byte-for-byte what they were before this guard existed —
// including the defensive case of a candidate whose files span two directories,
// where a set-wide commonLeaf would collapse to "" and register nothing,
// stranding those folders. matcher.Rank groups on (username, ReleaseDir) so a
// real candidate is always exactly one directory and the loop runs once; the
// wording of the all-or-nothing rule survives as intent, not as machinery.
func reserveDownloadFolders(ctx context.Context, d topUpDeps, jobID, candidateID int64, files []core.Transfer, now time.Time, transferDeadline time.Duration, log *slog.Logger) (bool, error) {
	seen := make(map[string]struct{}, 1)
	for _, f := range files {
		leaf := commonLeaf([]string{f.Filename})
		if leaf == "" {
			continue
		}
		if _, dup := seen[leaf]; dup {
			continue
		}
		seen[leaf] = struct{}{}

		owner, ok, err := d.RegisterDownloadFolder(ctx, jobID, leaf, now)
		if err != nil {
			log.Error("register download folder failed", "album_job", jobID, "folder", leaf, "err", err)
			continue
		}
		if ok {
			continue
		}
		return false, deferForFolder(ctx, d, jobID, candidateID, leaf, owner, now, transferDeadline, log)
	}
	// Nothing is in the way, so any earlier wait is over. Clearing it here
	// rather than on the first successful enqueue keeps the clock tied to the
	// obstruction and not to how the tick happened to end.
	if err := d.ClearCandidateDeferral(ctx, candidateID); err != nil {
		log.Error("clear candidate deferral failed", "album_job", jobID, "candidate", candidateID, "err", err)
	}
	return true, nil
}

// deferForFolder records that candidateID is waiting for owner to release leaf,
// and fails the candidate once the wait outlives transferDeadline.
//
// Waiting, not failing, is the point: a collision says "not right now", never
// "bad candidate". Routing it through FailCandidateAndAdvance immediately would
// burn a retry, and RejectCandidateAndAdvance would blacklist the peer
// permanently (#317) because a neighbour happened to download at the same time.
//
// The ceiling exists because nothing else can break this wait. TransfersPastDeadline
// and StallTimeout only look at transfers already QUEUED/IN_PROGRESS/STALLED,
// and a deferred candidate's transfers are all still PENDING with no deadline
// set. transferDeadline is reused rather than adding a config key: a required
// key must exist in production's config.toml before the merge deploys, and this
// is the same question — how long may one candidate hold a job up.
func deferForFolder(ctx context.Context, d topUpDeps, jobID, candidateID int64, leaf string, owner int64, now time.Time, transferDeadline time.Duration, log *slog.Logger) error {
	since, first, err := d.DeferCandidate(ctx, candidateID, now)
	if err != nil {
		return err
	}
	if first {
		// Written once per wait, not once per tick: `first` is true exactly when
		// deferred_since went NULL -> set. Silence here would leave the job
		// looking stuck on the one screen whose whole job is explaining stuck
		// jobs, and the owning job id is what makes the wait chaseable.
		detail := fmt.Sprintf("download folder %q is in use by job %d, waiting", leaf, owner)
		log.Info("candidate deferred", "album_job", jobID, "candidate", candidateID, "folder", leaf, "owner", owner)
		if err := d.AddJobEvent(ctx, jobID, core.EventCandidateDeferred, detail, now); err != nil {
			log.Warn("record candidate_deferred event failed", "album_job", jobID, "err", err)
		}
		return nil
	}
	if since.IsZero() || now.Sub(since) <= transferDeadline {
		return nil
	}
	reason := fmt.Sprintf("download folder %q still held by job %d after %s", leaf, owner, transferDeadline)
	log.Info("deferred candidate hit the ceiling", "album_job", jobID, "candidate", candidateID, "folder", leaf, "owner", owner)
	// from is DOWNLOADING for both callers: Selecting only reaches topUpCandidate
	// after ActivateCandidateWithTransfers has already advanced the job.
	if _, err := d.FailCandidateAndAdvance(ctx, candidateID, jobID, reason, core.StateDownloading, core.StateSelecting, now); err != nil {
		return err
	}
	return nil
}
