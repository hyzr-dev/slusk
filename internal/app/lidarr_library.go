// Package app: lidarr_library.go is the transport-neutral service backing
// issue #331's "add to Lidarr" flow - checking whether an artist/album a
// user is about to manually download is already in their Lidarr library,
// and if not, adding the artist and monitoring the album (or its whole
// discography) on their behalf.
//
// LidarrLibrary declares its own narrow interface onto internal/lidarr,
// exactly like Identify declares LidarrLibraryLookup: this package owns the
// interface it consumes, not the package it wraps.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// ErrLidarrLibraryQueryInvalid is returned when a required argument is
// blank or non-positive; internal/observ maps this to 422.
var ErrLidarrLibraryQueryInvalid = errors.New("lidarr library request is missing a required field")

// ErrLidarrLibraryInvalidMonitorChoice is returned when AddArtistParams.Monitor
// is not one of the defined MonitorChoice values; internal/observ maps this
// to 422. In practice internal/observ's own request validation rejects an
// unrecognised monitor string before this is ever reached - this guard exists
// so the service is not silently wrong if called directly (e.g. from tests
// or a future caller) with a zero-value or invalid choice.
var ErrLidarrLibraryInvalidMonitorChoice = errors.New("invalid monitor choice")

// ErrLidarrLibraryInvalidRootFolder is returned when
// AddArtistParams.RootFolderPath is not one of Lidarr's currently configured,
// accessible root folders (issue #331 backend review); internal/observ maps
// this to 422 with a rootFolderPath field error. RootFolderPath is the one
// user-controlled string in this flow that reaches an upstream write
// unchecked, and the allowed set is one GET /rootfolder away.
var ErrLidarrLibraryInvalidRootFolder = errors.New("root folder path is not one of Lidarr's configured, accessible root folders")

// ErrLidarrLibraryAddUncertain is an alias of core.ErrLidarrAddArtistUncertain
// (issue #331 backend review): the artist-create request failed at the
// transport level and Lidarr's own re-check could not establish whether the
// write actually landed. internal/observ maps this to its own distinct
// status (502 with code "addUncertain") rather than the generic 500 every
// other failure gets, since a blind client retry here risks creating a
// duplicate artist. Exposed under this name so callers outside internal/core
// don't need to know the sentinel originates from Lidarr's own client.
var ErrLidarrLibraryAddUncertain = core.ErrLidarrAddArtistUncertain

// defaultAlbumPollAttempts/defaultAlbumPollInterval bound how long
// AddArtistAndMonitor waits for the target album to become resolvable via
// AlbumByForeignID after waitForIdle has already waited out
// Lidarr's async refresh commands (issue #331; see
// .lidarr-endpoints-verified.md's "race in step 3" - an empty result here
// means Lidarr has not refreshed yet, not that the album is absent). Since
// waitForIdle now absorbs the bulk of that wait before this poll starts,
// this budget only needs to cover the residual lag between a refresh command
// reporting done and Lidarr's own read model reflecting it - ~27.5s (double
// the previous ~13.75s, which a live probe against Lidarr 3.1.0.4875 showed
// could still be too tight on a cold add) is a defensible margin without
// resurrecting a second long wait.
//
// defaultRefreshPollAttempts/defaultRefreshPollInterval bound waitForIdle
// itself: the live probe measured POST /artist taking over 30s on a cold
// metadata fetch, and the RefreshArtist command that follows does comparable
// (or more) work, so ~1m55s (well short of testenv/seed_lidarr.py's
// 15-minute worst-case budget, which this mirrors) is generous without being
// unbounded.
//
// Both budgets' actual worst-case wait is (attempts-1)*interval, since there
// is no sleep after the final attempt - these comments are the only spec for
// the numbers below, so they state the true figure rather than a
// nicer-sounding round one.
const (
	defaultAlbumPollAttempts   = 12
	defaultAlbumPollInterval   = 2500 * time.Millisecond
	defaultRefreshPollAttempts = 24
	defaultRefreshPollInterval = 5 * time.Second
)

// watchedLidarrCommands are the asynchronous commands issue #331's race is
// about (see testenv/seed_lidarr.py's wait_for_idle): POST /artist returns
// before Lidarr's RefreshArtist command - which can itself queue
// RefreshAlbum and RescanFolders - has run, and that command re-applies the
// artist's monitor policy, undoing whatever was just set. Monitoring must
// not be applied while any of these is queued or started.
var watchedLidarrCommands = map[string]bool{
	"RefreshArtist": true,
	"RefreshAlbum":  true,
	"RescanFolders": true,
}

// MonitorChoice is the user's choice of what to monitor after an artist is
// added (issue #331). The zero value is deliberately invalid - see
// MonitorChoice.Valid - so a caller that forgets to set it is rejected
// rather than silently monitoring nothing or everything.
type MonitorChoice int

const (
	// MonitorThisAlbum monitors only the album that prompted the add.
	MonitorThisAlbum MonitorChoice = iota + 1
	// MonitorAllAlbums monitors the artist's entire known discography.
	MonitorAllAlbums
)

// Valid reports whether c is one of the defined MonitorChoice values.
func (c MonitorChoice) Valid() bool {
	return c == MonitorThisAlbum || c == MonitorAllAlbums
}

// LidarrArtistStatus is the read-only Lidarr library status for one
// MusicBrainz artist (issue #331), mirroring app.LidarrAlbumStatus's
// three-way semantics exactly: Known is false when Lidarr could not be
// reached or answered with an error - that must be surfaced to the user as
// "unknown", never silently treated as absent.
type LidarrArtistStatus struct {
	Known     bool
	InLibrary bool
	// ArtistID and Name are meaningful only when InLibrary.
	ArtistID int64
	Name     string
}

// LidarrAddOptions carries everything the "add artist" form needs to
// populate its selectors (issue #331): every configured root folder (with
// its default profile ids) plus every quality and metadata profile. No
// config keys back this - Lidarr's own GET /rootfolder response already
// carries the per-folder defaults the UI prefills from.
type LidarrAddOptions struct {
	RootFolders      []core.LidarrRootFolder
	QualityProfiles  []core.LidarrProfile
	MetadataProfiles []core.LidarrProfile
}

// AddArtistParams configures AddArtistAndMonitor.
type AddArtistParams struct {
	// ArtistMBID is the MusicBrainz artist id to add (Lidarr's foreignArtistId).
	ArtistMBID string
	// ArtistName is passed through to Lidarr's create call for display; it
	// is not used to look anything up.
	ArtistName string
	// AlbumMBID is the MusicBrainz release-group id of the album that
	// prompted the add - the one AddArtistAndMonitor polls for and, per
	// Monitor, monitors either alone or as part of the whole discography.
	AlbumMBID         string
	RootFolderPath    string
	QualityProfileID  int64
	MetadataProfileID int64
	Monitor           MonitorChoice
}

// AlbumMonitorState reports the outcome of trying to monitor the target
// album (issue #331 backend review): a plain bool cannot honestly
// distinguish "not visible yet", "reverted and the re-apply did not stick"
// and "unknown because verification itself failed" - conflating any of
// those into a flat "false" would misreport what actually happened. The
// underlying string values are exactly the wire representation used by
// internal/observ's DTO.
type AlbumMonitorState string

const (
	// AlbumMonitorStateMonitored means monitoring was applied and confirmed
	// (or, for an artist already in the library, the write simply succeeded -
	// there is no post-add refresh race to verify against in that case).
	AlbumMonitorStateMonitored AlbumMonitorState = "monitored"
	// AlbumMonitorStateNotVisibleYet means the poll budget for the target
	// album ran out before Lidarr's refresh made it resolvable - the artist
	// genuinely exists, monitoring was simply never attempted.
	AlbumMonitorStateNotVisibleYet AlbumMonitorState = "notVisibleYet"
	// AlbumMonitorStateReverted means Lidarr's async refresh reverted
	// monitoring after it was applied, and the single re-apply attempt did
	// not stick (or itself failed).
	AlbumMonitorStateReverted AlbumMonitorState = "reverted"
	// AlbumMonitorStateUnknown means a transport error during verification
	// left the true state unconfirmed - monitoring may well be fine, this is
	// not a claim that it is not.
	AlbumMonitorStateUnknown AlbumMonitorState = "unknown"
)

// AddArtistResult reports what AddArtistAndMonitor actually did (issue
// #331; fields ArtistMonitored/AlbumMonitorState reshaped by the backend
// review - see AlbumMonitorState's doc comment for why a single boolean
// was not honest enough). AlreadyInLibrary being true means no artist was
// created - the existing one was reused, and its monitored flag was never
// touched (ArtistMonitored reports whatever it already was). AlbumMonitorState
// other than "monitored" is not necessarily an error: the artist genuinely
// exists (created or reused) even when the target album could not yet be
// resolved or monitoring could not be confirmed.
type AddArtistResult struct {
	ArtistID          int64
	AlreadyInLibrary  bool
	ArtistMonitored   bool
	AlbumMonitorState AlbumMonitorState
}

// LidarrLibraryClient is the slice of internal/lidarr.Client LidarrLibrary
// needs for issue #331's "add to Lidarr" flow.
type LidarrLibraryClient interface {
	ArtistByMBID(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error)
	RootFolders(ctx context.Context) ([]core.LidarrRootFolder, error)
	QualityProfiles(ctx context.Context) ([]core.LidarrProfile, error)
	MetadataProfiles(ctx context.Context) ([]core.LidarrProfile, error)
	AddArtist(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error)
	SetArtistMonitored(ctx context.Context, artistID int64, monitored bool) error
	MonitorAlbums(ctx context.Context, albumIDs []int64, monitored bool) error
	AlbumsByArtist(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error)
	AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
	RunningCommands(ctx context.Context) ([]core.LidarrCommand, error)
}

// LidarrLibraryParams configures NewLidarrLibrary.
type LidarrLibraryParams struct {
	Lidarr LidarrLibraryClient
	Logger *slog.Logger
	// AlbumPollAttempts/AlbumPollInterval override the bounded retry
	// AddArtistAndMonitor uses to wait for the target album to become
	// resolvable once Lidarr's refresh commands have gone idle. Zero values
	// default to defaultAlbumPollAttempts/defaultAlbumPollInterval; tests
	// override both to avoid a real sleep.
	AlbumPollAttempts int
	AlbumPollInterval time.Duration
	// RefreshPollAttempts/RefreshPollInterval override waitForIdle's bounded
	// wait for Lidarr's asynchronous RefreshArtist/RefreshAlbum/RescanFolders
	// commands to finish after a fresh AddArtist, before monitoring is
	// applied. Zero values default to
	// defaultRefreshPollAttempts/defaultRefreshPollInterval; tests override
	// both to avoid a real sleep.
	RefreshPollAttempts int
	RefreshPollInterval time.Duration
}

// LidarrLibrary is the transport-neutral service backing issue #331's "add
// to Lidarr" flow.
type LidarrLibrary struct {
	lidarr              LidarrLibraryClient
	logger              *slog.Logger
	pollAttempts        int
	pollInterval        time.Duration
	refreshPollAttempts int
	refreshPollInterval time.Duration
}

// NewLidarrLibrary constructs a LidarrLibrary service.
func NewLidarrLibrary(p LidarrLibraryParams) *LidarrLibrary {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attempts := p.AlbumPollAttempts
	if attempts <= 0 {
		attempts = defaultAlbumPollAttempts
	}
	interval := p.AlbumPollInterval
	if interval <= 0 {
		interval = defaultAlbumPollInterval
	}
	refreshAttempts := p.RefreshPollAttempts
	if refreshAttempts <= 0 {
		refreshAttempts = defaultRefreshPollAttempts
	}
	refreshInterval := p.RefreshPollInterval
	if refreshInterval <= 0 {
		refreshInterval = defaultRefreshPollInterval
	}
	return &LidarrLibrary{
		lidarr: p.Lidarr, logger: logger,
		pollAttempts: attempts, pollInterval: interval,
		refreshPollAttempts: refreshAttempts, refreshPollInterval: refreshInterval,
	}
}

// ArtistStatus reports whether a MusicBrainz artist is already in the
// user's Lidarr library, for GET /api/lidarr/artists/{mbid}. A Lidarr
// transport failure is not mapped to an error - like
// Identify.AlbumLidarrStatus, "Lidarr unreachable" is a normal, displayable
// outcome (Known = false), not a request failure.
func (l *LidarrLibrary) ArtistStatus(ctx context.Context, artistMBID string) (LidarrArtistStatus, error) {
	artistMBID = strings.TrimSpace(artistMBID)
	if artistMBID == "" {
		return LidarrArtistStatus{}, ErrLidarrLibraryQueryInvalid
	}
	artist, found, err := l.lidarr.ArtistByMBID(ctx, artistMBID)
	if err != nil {
		l.logger.Warn("lidarr artist lookup failed", "artistMBID", artistMBID, "err", err)
		return LidarrArtistStatus{Known: false}, nil
	}
	if !found {
		return LidarrArtistStatus{Known: true, InLibrary: false}, nil
	}
	return LidarrArtistStatus{Known: true, InLibrary: true, ArtistID: artist.ID, Name: artist.Name}, nil
}

// AddOptions gathers everything the "add artist" form needs (issue #331),
// for GET /api/lidarr/add-options.
func (l *LidarrLibrary) AddOptions(ctx context.Context) (LidarrAddOptions, error) {
	roots, err := l.lidarr.RootFolders(ctx)
	if err != nil {
		return LidarrAddOptions{}, fmt.Errorf("lidarr root folders: %w", err)
	}
	quality, err := l.lidarr.QualityProfiles(ctx)
	if err != nil {
		return LidarrAddOptions{}, fmt.Errorf("lidarr quality profiles: %w", err)
	}
	metadata, err := l.lidarr.MetadataProfiles(ctx)
	if err != nil {
		return LidarrAddOptions{}, fmt.Errorf("lidarr metadata profiles: %w", err)
	}
	return LidarrAddOptions{RootFolders: roots, QualityProfiles: quality, MetadataProfiles: metadata}, nil
}

// ensureArtist returns the library artist id for params.ArtistMBID, creating
// it if it does not already exist. alreadyInLibrary is true when an
// existing artist was reused - AddArtistAndMonitor must not re-create it,
// and must not flip its monitored flag on the caller's behalf either, since
// the user did not ask for an existing artist's monitoring to change.
//
// artistMonitored carries the artist's Monitored flag as Lidarr reports it
// right now (issue #331 backend review #3): for an existing artist that is
// existing.Monitored - never assumed true - so a caller who retries after a
// falsely-reported failure learns honestly that the artist that already
// exists is not actually monitored, instead of AddArtistAndMonitor claiming
// success on an artist Lidarr will never act on. For a freshly created
// artist it is AddArtist's own report, which is always false in practice -
// see AddArtist's doc comment - but is carried through rather than assumed,
// in case that ever stops being true.
//
// A transport error from AddArtist that wraps core.ErrLidarrAddArtistUncertain
// is returned unwrapped further, so errors.Is(err, ErrLidarrLibraryAddUncertain)
// keeps working all the way out to internal/observ.
func (l *LidarrLibrary) ensureArtist(ctx context.Context, params AddArtistParams) (artistID int64, alreadyInLibrary bool, artistMonitored bool, err error) {
	existing, found, err := l.lidarr.ArtistByMBID(ctx, params.ArtistMBID)
	if err != nil {
		return 0, false, false, fmt.Errorf("lidarr artist lookup: %w", err)
	}
	if found {
		return existing.ID, true, existing.Monitored, nil
	}
	created, err := l.lidarr.AddArtist(ctx, core.AddArtistRequest{
		ForeignArtistID:   params.ArtistMBID,
		ArtistName:        params.ArtistName,
		QualityProfileID:  params.QualityProfileID,
		MetadataProfileID: params.MetadataProfileID,
		RootFolderPath:    params.RootFolderPath,
	})
	if err != nil {
		if errors.Is(err, ErrLidarrLibraryAddUncertain) {
			return 0, false, false, err
		}
		return 0, false, false, fmt.Errorf("lidarr add artist: %w", err)
	}
	return created.ID, false, created.Monitored, nil
}

// pollForAlbum retries AlbumByForeignID up to l.pollAttempts times, spaced
// l.pollInterval apart, to give Lidarr time to finish refreshing a freshly
// added artist's metadata (see the package doc comment's race in step 3).
// found is false with a nil error when every attempt came back empty -
// "not visible yet", not "absent" - callers must not treat that as a hard
// failure.
func (l *LidarrLibrary) pollForAlbum(ctx context.Context, albumMBID string) (album core.LidarrAlbum, found bool, err error) {
	for attempt := 0; attempt < l.pollAttempts; attempt++ {
		album, found, err = l.lidarr.AlbumByForeignID(ctx, albumMBID)
		if err != nil {
			return core.LidarrAlbum{}, false, err
		}
		if found {
			return album, true, nil
		}
		if attempt < l.pollAttempts-1 {
			select {
			case <-ctx.Done():
				return core.LidarrAlbum{}, false, ctx.Err()
			case <-time.After(l.pollInterval):
			}
		}
	}
	return core.LidarrAlbum{}, false, nil
}

// commandBlocksArtist reports whether c should hold up waitForIdle for
// artistID (issue #331 backend review #2): GET /command is instance-wide, so
// an unrelated refresh must not burn the whole wait budget. A command scoped
// to other artists only (c.ArtistIDs non-empty and artistID absent from it)
// is not blocking; a command with no ArtistIDs at all is treated as
// unscoped - a library-wide refresh can still touch our artist - and blocks
// regardless, matching the pre-scoping behaviour for that case.
func commandBlocksArtist(c core.LidarrCommand, artistID int64) bool {
	if !watchedLidarrCommands[c.Name] || (c.Status != "queued" && c.Status != "started") {
		return false
	}
	if len(c.ArtistIDs) == 0 {
		return true
	}
	for _, id := range c.ArtistIDs {
		if id == artistID {
			return true
		}
	}
	return false
}

// waitForIdle blocks until Lidarr reports no queued or started
// RefreshArtist/RefreshAlbum/RescanFolders command that could affect
// artistID (see commandBlocksArtist), bounded by
// l.refreshPollAttempts/l.refreshPollInterval (issue #331; mirrors
// testenv/seed_lidarr.py's wait_for_idle). Running out of budget is not
// treated as a hard failure - it is logged and AddArtistAndMonitor proceeds
// anyway, relying on the later verify-and-reapply step to catch a refresh
// that reverted monitoring after all. Only a transport error is returned,
// since that leaves Lidarr's state genuinely unknown.
func (l *LidarrLibrary) waitForIdle(ctx context.Context, artistID int64) error {
	for attempt := 0; attempt < l.refreshPollAttempts; attempt++ {
		commands, err := l.lidarr.RunningCommands(ctx)
		if err != nil {
			return err
		}
		busy := false
		for _, c := range commands {
			if commandBlocksArtist(c, artistID) {
				busy = true
				break
			}
		}
		if !busy {
			return nil
		}
		if attempt < l.refreshPollAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(l.refreshPollInterval):
			}
		}
	}
	l.logger.Warn("lidarr refresh commands did not go idle within the poll budget")
	return nil
}

// monitoringInPlace reports the artist's current Monitored flag
// (artistMonitored) and whether the artist and every album in albumIDs
// currently read back as monitored (allInPlace) - used both to decide
// whether a refresh reverted monitoring and to confirm a re-apply took
// (issue #331). err non-nil means the answer is unknown - a transport
// failure, not a clean "not in place" - and callers must not treat
// artistMonitored/allInPlace as meaningful in that case.
func (l *LidarrLibrary) monitoringInPlace(ctx context.Context, artistMBID string, artistID int64, albumIDs []int64) (artistMonitored, allInPlace bool, err error) {
	artist, found, err := l.lidarr.ArtistByMBID(ctx, artistMBID)
	if err != nil {
		return false, false, err
	}
	if !found || !artist.Monitored {
		return false, false, nil
	}
	albums, err := l.lidarr.AlbumsByArtist(ctx, artistID)
	if err != nil {
		return true, false, err
	}
	monitored := make(map[int64]bool, len(albums))
	for _, a := range albums {
		monitored[a.ID] = a.Monitored
	}
	for _, id := range albumIDs {
		if !monitored[id] {
			return true, false, nil
		}
	}
	return true, true, nil
}

// ensureMonitoringApplied verifies that monitoring set moments ago is still
// in place, and re-applies it once if Lidarr's async refresh reverted it in
// the meantime (issue #331; the exact race testenv/seed_lidarr.py's
// wait_for_idle documents). It reports the resulting AlbumMonitorState and
// the artist's best-known Monitored flag; see AlbumMonitorState's doc
// comment for what each state means, and monitoringInPlace's for why a
// transport error during verification must not be reported as "reverted" -
// the last successful write may well have stuck, this call just could not
// confirm it.
func (l *LidarrLibrary) ensureMonitoringApplied(ctx context.Context, artistMBID string, artistID int64, albumIDs []int64) (AlbumMonitorState, bool) {
	artistMonitored, inPlace, err := l.monitoringInPlace(ctx, artistMBID, artistID, albumIDs)
	if err != nil {
		l.logger.Warn("lidarr monitoring verification failed", "artistId", artistID, "err", err)
		// The explicit SetArtistMonitored(true) just before this call
		// succeeded (AddArtistAndMonitor would have returned already
		// otherwise) - verification failing to confirm it is not evidence
		// it was reverted.
		return AlbumMonitorStateUnknown, true
	}
	if inPlace {
		return AlbumMonitorStateMonitored, true
	}
	l.logger.Warn("lidarr's refresh reverted monitoring, re-applying once", "artistId", artistID)
	if err := l.lidarr.SetArtistMonitored(ctx, artistID, true); err != nil {
		l.logger.Warn("lidarr re-apply SetArtistMonitored failed", "artistId", artistID, "err", err)
		return AlbumMonitorStateReverted, artistMonitored
	}
	if err := l.lidarr.MonitorAlbums(ctx, albumIDs, true); err != nil {
		l.logger.Warn("lidarr re-apply MonitorAlbums failed", "artistId", artistID, "err", err)
		return AlbumMonitorStateReverted, true // the artist-level re-apply above did succeed
	}
	artistMonitored, inPlace, err = l.monitoringInPlace(ctx, artistMBID, artistID, albumIDs)
	if err != nil {
		l.logger.Warn("lidarr monitoring re-verification failed", "artistId", artistID, "err", err)
		return AlbumMonitorStateUnknown, true
	}
	if inPlace {
		return AlbumMonitorStateMonitored, true
	}
	return AlbumMonitorStateReverted, artistMonitored
}

// rootFolderAllowed reports whether path matches one of Lidarr's currently
// configured, accessible root folders (issue #331 backend review #7) -
// RootFolderPath is the one user-controlled string in this flow that reaches
// an upstream write unchecked.
func rootFolderAllowed(path string, roots []core.LidarrRootFolder) bool {
	for _, r := range roots {
		if r.Path == path && r.Accessible {
			return true
		}
	}
	return false
}

// AddArtistAndMonitor runs issue #331's verified add sequence (see
// .lidarr-endpoints-verified.md's "reliable sequence" and
// testenv/seed_lidarr.py's wait_for_idle): ensure the artist exists (creating
// it, unmonitored, if not), wait for Lidarr's asynchronous post-add refresh
// to go idle, explicitly mark the artist monitored, then poll for the target
// album and monitor either just that album or the artist's whole
// discography, per params.Monitor. Monitoring is never expressed through
// AddArtist's own request body - see AddArtist's doc comment for why that
// does not work reliably on Lidarr 3.1.0.
//
// For a freshly created artist, monitoring is re-verified after it is
// applied and re-applied once if the refresh reverted it in the window
// between waitForIdle giving up and the refresh actually finishing -
// waitForIdle's own budget is best-effort, not a guarantee. See
// AlbumMonitorState's doc comment for what the result's AlbumMonitorState
// values mean; only AlbumMonitorStateMonitored means that verification
// actually confirmed it.
//
// A non-"monitored" AlbumMonitorState in the result is not necessarily an
// error: the artist genuinely exists (created or reused) even when the poll
// budget ran out before the album became visible, or when monitoring could
// not be confirmed.
func (l *LidarrLibrary) AddArtistAndMonitor(ctx context.Context, params AddArtistParams) (AddArtistResult, error) {
	if strings.TrimSpace(params.ArtistMBID) == "" ||
		strings.TrimSpace(params.AlbumMBID) == "" ||
		strings.TrimSpace(params.RootFolderPath) == "" ||
		params.QualityProfileID <= 0 ||
		params.MetadataProfileID <= 0 {
		return AddArtistResult{}, ErrLidarrLibraryQueryInvalid
	}
	if !params.Monitor.Valid() {
		return AddArtistResult{}, ErrLidarrLibraryInvalidMonitorChoice
	}

	roots, err := l.lidarr.RootFolders(ctx)
	if err != nil {
		return AddArtistResult{}, fmt.Errorf("lidarr root folders: %w", err)
	}
	if !rootFolderAllowed(params.RootFolderPath, roots) {
		return AddArtistResult{}, ErrLidarrLibraryInvalidRootFolder
	}

	artistID, alreadyInLibrary, artistMonitored, err := l.ensureArtist(ctx, params)
	if err != nil {
		return AddArtistResult{}, err
	}
	if !alreadyInLibrary {
		if err := l.waitForIdle(ctx, artistID); err != nil {
			return AddArtistResult{}, fmt.Errorf("lidarr wait for idle: %w", err)
		}
		if err := l.lidarr.SetArtistMonitored(ctx, artistID, true); err != nil {
			return AddArtistResult{}, fmt.Errorf("lidarr set artist monitored: %w", err)
		}
		artistMonitored = true
	}

	album, found, err := l.pollForAlbum(ctx, params.AlbumMBID)
	if err != nil {
		return AddArtistResult{}, fmt.Errorf("lidarr album lookup: %w", err)
	}
	if !found {
		l.logger.Warn("lidarr had not refreshed the target album by the time the poll budget ran out",
			"artistId", artistID, "albumMBID", params.AlbumMBID)
		return AddArtistResult{
			ArtistID: artistID, AlreadyInLibrary: alreadyInLibrary,
			ArtistMonitored: artistMonitored, AlbumMonitorState: AlbumMonitorStateNotVisibleYet,
		}, nil
	}
	if album.ArtistID != artistID {
		return AddArtistResult{}, fmt.Errorf("lidarr album %d resolved to artist %d, not the artist just ensured (%d)", album.ID, album.ArtistID, artistID)
	}

	// Union, never overwrite: album.ID must always be in the set being
	// monitored, even if AlbumsByArtist comes back partial or empty because
	// Lidarr's refresh is still mid-flight (issue #331 backend review #5).
	monitorIDs := []int64{album.ID}
	if params.Monitor == MonitorAllAlbums {
		albums, err := l.lidarr.AlbumsByArtist(ctx, artistID)
		if err != nil {
			return AddArtistResult{}, fmt.Errorf("lidarr albums by artist: %w", err)
		}
		seen := map[int64]bool{album.ID: true}
		for _, a := range albums {
			if !seen[a.ID] {
				seen[a.ID] = true
				monitorIDs = append(monitorIDs, a.ID)
			}
		}
	}
	if len(monitorIDs) == 0 {
		// Defensive: album.ID is always seeded above, so this should be
		// unreachable, but MonitorAlbums(nil, true) would otherwise succeed
		// having monitored nothing while looking like success.
		l.logger.Warn("lidarr album monitor set was empty, not calling MonitorAlbums", "artistId", artistID)
		return AddArtistResult{
			ArtistID: artistID, AlreadyInLibrary: alreadyInLibrary,
			ArtistMonitored: artistMonitored, AlbumMonitorState: AlbumMonitorStateUnknown,
		}, nil
	}
	if err := l.lidarr.MonitorAlbums(ctx, monitorIDs, true); err != nil {
		return AddArtistResult{}, fmt.Errorf("lidarr monitor albums: %w", err)
	}

	state := AlbumMonitorStateMonitored
	if !alreadyInLibrary {
		state, artistMonitored = l.ensureMonitoringApplied(ctx, params.ArtistMBID, artistID, monitorIDs)
	}
	return AddArtistResult{
		ArtistID: artistID, AlreadyInLibrary: alreadyInLibrary,
		ArtistMonitored: artistMonitored, AlbumMonitorState: state,
	}, nil
}
