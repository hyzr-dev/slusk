package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// fakeLidarrLibraryClient is a LidarrLibraryClient test double. runningCommands
// defaults to reporting idle (nil, nil) when unset, since most tests here are
// not about the refresh-command race - only the ones that are set it
// explicitly.
type fakeLidarrLibraryClient struct {
	artistByMBID     func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error)
	rootFolders      func(ctx context.Context) ([]core.LidarrRootFolder, error)
	qualityProfiles  func(ctx context.Context) ([]core.LidarrProfile, error)
	metadataProfiles func(ctx context.Context) ([]core.LidarrProfile, error)
	addArtist        func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error)
	setMonitored     func(ctx context.Context, artistID int64, monitored bool) error
	monitorAlbums    func(ctx context.Context, albumIDs []int64, monitored bool) error
	albumsByArtist   func(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error)
	albumByForeignID func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
	runningCommands  func(ctx context.Context) ([]core.LidarrCommand, error)
}

func (f *fakeLidarrLibraryClient) ArtistByMBID(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
	return f.artistByMBID(ctx, artistMBID)
}

// RootFolders defaults to a single accessible "/music/library" folder when
// rootFolders is unset - matching validAddArtistParams' RootFolderPath - so
// tests that aren't about root-folder validation don't each need to stub it.
func (f *fakeLidarrLibraryClient) RootFolders(ctx context.Context) ([]core.LidarrRootFolder, error) {
	if f.rootFolders == nil {
		return []core.LidarrRootFolder{{ID: 1, Path: "/music/library", Accessible: true}}, nil
	}
	return f.rootFolders(ctx)
}
func (f *fakeLidarrLibraryClient) QualityProfiles(ctx context.Context) ([]core.LidarrProfile, error) {
	return f.qualityProfiles(ctx)
}
func (f *fakeLidarrLibraryClient) MetadataProfiles(ctx context.Context) ([]core.LidarrProfile, error) {
	return f.metadataProfiles(ctx)
}
func (f *fakeLidarrLibraryClient) AddArtist(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
	return f.addArtist(ctx, req)
}
func (f *fakeLidarrLibraryClient) SetArtistMonitored(ctx context.Context, artistID int64, monitored bool) error {
	return f.setMonitored(ctx, artistID, monitored)
}
func (f *fakeLidarrLibraryClient) MonitorAlbums(ctx context.Context, albumIDs []int64, monitored bool) error {
	return f.monitorAlbums(ctx, albumIDs, monitored)
}
func (f *fakeLidarrLibraryClient) AlbumsByArtist(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error) {
	return f.albumsByArtist(ctx, artistID)
}
func (f *fakeLidarrLibraryClient) AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
	return f.albumByForeignID(ctx, foreignAlbumID)
}
func (f *fakeLidarrLibraryClient) RunningCommands(ctx context.Context) ([]core.LidarrCommand, error) {
	if f.runningCommands == nil {
		return nil, nil
	}
	return f.runningCommands(ctx)
}

// newlyCreatedThenMonitoredArtist returns an artistByMBID stub for the
// common "artist does not exist yet" shape: ensureArtist's first lookup
// reports absent (so AddArtist gets called), and every later call - the
// verification AddArtistAndMonitor runs after applying monitoring - reports
// the artist present and monitored, matching what Lidarr's own refresh
// should have left in place absent a revert.
func newlyCreatedThenMonitoredArtist(artistID int64) func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
	calls := 0
	return func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
		calls++
		if calls == 1 {
			return core.LidarrArtist{}, false, nil
		}
		return core.LidarrArtist{ID: artistID, Monitored: true}, true, nil
	}
}

// monitoredAlbums returns an albumsByArtist stub reporting every given id as
// monitored - used alongside newlyCreatedThenMonitoredArtist so the
// post-apply verification in ensureMonitoringApplied confirms success
// without needing a re-apply.
func monitoredAlbums(ids ...int64) func(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error) {
	return func(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error) {
		albums := make([]core.LidarrAlbum, len(ids))
		for i, id := range ids {
			albums[i] = core.LidarrAlbum{ID: id, Monitored: true}
		}
		return albums, nil
	}
}

func TestLidarrLibraryArtistStatusValidatesID(t *testing.T) {
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: &fakeLidarrLibraryClient{}})
	if _, err := l.ArtistStatus(context.Background(), "  "); !errors.Is(err, ErrLidarrLibraryQueryInvalid) {
		t.Fatalf("ArtistStatus(blank) = %v, want ErrLidarrLibraryQueryInvalid", err)
	}
}

func TestLidarrLibraryArtistStatusInLibrary(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{ID: 9, Name: "Aphex Twin"}, true, nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.ArtistStatus(context.Background(), "artist-1")
	if err != nil {
		t.Fatalf("ArtistStatus: %v", err)
	}
	if !got.Known || !got.InLibrary || got.ArtistID != 9 || got.Name != "Aphex Twin" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestLidarrLibraryArtistStatusNotInLibrary(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{}, false, nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.ArtistStatus(context.Background(), "artist-1")
	if err != nil {
		t.Fatalf("ArtistStatus: %v", err)
	}
	if !got.Known || got.InLibrary {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestLidarrLibraryArtistStatusUnreachableIsUnknownNotError covers issue
// #331's design, mirroring app.Identify.AlbumLidarrStatus: a Lidarr
// transport failure must surface as Known=false, not as a request error.
func TestLidarrLibraryArtistStatusUnreachableIsUnknownNotError(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{}, false, errors.New("connection refused")
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.ArtistStatus(context.Background(), "artist-1")
	if err != nil {
		t.Fatalf("ArtistStatus should not return an error for an unreachable Lidarr, got %v", err)
	}
	if got.Known {
		t.Fatalf("expected Known = false, got %+v", got)
	}
}

func TestLidarrLibraryAddOptions(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		rootFolders: func(ctx context.Context) ([]core.LidarrRootFolder, error) {
			return []core.LidarrRootFolder{{ID: 1, Path: "/music/library"}}, nil
		},
		qualityProfiles: func(ctx context.Context) ([]core.LidarrProfile, error) {
			return []core.LidarrProfile{{ID: 1, Name: "Any"}}, nil
		},
		metadataProfiles: func(ctx context.Context) ([]core.LidarrProfile, error) {
			return []core.LidarrProfile{{ID: 1, Name: "Standard"}}, nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.AddOptions(context.Background())
	if err != nil {
		t.Fatalf("AddOptions: %v", err)
	}
	if len(got.RootFolders) != 1 || len(got.QualityProfiles) != 1 || len(got.MetadataProfiles) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func validAddArtistParams() AddArtistParams {
	return AddArtistParams{
		ArtistMBID: "artist-1", ArtistName: "Aphex Twin", AlbumMBID: "rg-1",
		RootFolderPath: "/music/library", QualityProfileID: 2, MetadataProfileID: 1,
		Monitor: MonitorThisAlbum,
	}
}

func TestAddArtistAndMonitorValidatesRequiredFields(t *testing.T) {
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: &fakeLidarrLibraryClient{}})
	cases := []struct {
		name   string
		mutate func(p AddArtistParams) AddArtistParams
	}{
		{"blank artist mbid", func(p AddArtistParams) AddArtistParams { p.ArtistMBID = " "; return p }},
		{"blank album mbid", func(p AddArtistParams) AddArtistParams { p.AlbumMBID = " "; return p }},
		{"blank root folder", func(p AddArtistParams) AddArtistParams { p.RootFolderPath = " "; return p }},
		{"zero quality profile", func(p AddArtistParams) AddArtistParams { p.QualityProfileID = 0; return p }},
		{"zero metadata profile", func(p AddArtistParams) AddArtistParams { p.MetadataProfileID = 0; return p }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.AddArtistAndMonitor(context.Background(), c.mutate(validAddArtistParams())); !errors.Is(err, ErrLidarrLibraryQueryInvalid) {
				t.Fatalf("AddArtistAndMonitor(%s) = %v, want ErrLidarrLibraryQueryInvalid", c.name, err)
			}
		})
	}
}

func TestAddArtistAndMonitorRejectsUnknownMonitorChoice(t *testing.T) {
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: &fakeLidarrLibraryClient{}})
	params := validAddArtistParams()
	params.Monitor = 0
	if _, err := l.AddArtistAndMonitor(context.Background(), params); !errors.Is(err, ErrLidarrLibraryInvalidMonitorChoice) {
		t.Fatalf("AddArtistAndMonitor(invalid monitor) = %v, want ErrLidarrLibraryInvalidMonitorChoice", err)
	}
}

// TestAddArtistAndMonitorAlreadyInLibrary covers issue #331's guard: an
// existing artist must not be re-created, and its monitored flag must be
// left untouched - only the album-monitoring step should run.
func TestAddArtistAndMonitorAlreadyInLibrary(t *testing.T) {
	var setMonitoredCalled, addArtistCalled bool
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{ID: 9}, true, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			addArtistCalled = true
			return core.LidarrArtist{}, errors.New("must not be called")
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error {
			setMonitoredCalled = true
			return nil
		},
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error {
			if len(albumIDs) != 1 || albumIDs[0] != 42 || !monitored {
				t.Fatalf("unexpected monitorAlbums call: ids=%v monitored=%v", albumIDs, monitored)
			}
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if addArtistCalled {
		t.Fatal("AddArtist must not be called for an artist already in the library")
	}
	if setMonitoredCalled {
		t.Fatal("SetArtistMonitored must not be called for an artist already in the library")
	}
	if !got.AlreadyInLibrary || got.ArtistID != 9 || got.AlbumMonitorState != AlbumMonitorStateMonitored {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestAddArtistAndMonitorRetriesUntilAlbumAppears covers the verified race
// (.lidarr-endpoints-verified.md): Lidarr may not have refreshed the newly
// added artist's metadata yet, so an empty poll must be retried, not treated
// as absence.
func TestAddArtistAndMonitorRetriesUntilAlbumAppears(t *testing.T) {
	attempts := 0
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			attempts++
			if attempts < 3 {
				return core.LidarrAlbum{}, false, nil
			}
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		monitorAlbums:  func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
		albumsByArtist: monitoredAlbums(42),
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 6, AlbumPollInterval: time.Millisecond})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if got.AlbumMonitorState != AlbumMonitorStateMonitored || got.AlreadyInLibrary {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestAddArtistAndMonitorRetryExhaustionIsNotAnError covers issue #331's
// explicit requirement: running out of poll budget must report "artist
// created, album not monitored" rather than a failure - the artist genuinely
// exists.
func TestAddArtistAndMonitorRetryExhaustionIsNotAnError(t *testing.T) {
	var monitorCalled bool
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{}, false, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{}, false, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error {
			monitorCalled = true
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 3, AlbumPollInterval: time.Millisecond})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor should not error on retry exhaustion, got %v", err)
	}
	if got.AlbumMonitorState != AlbumMonitorStateNotVisibleYet {
		t.Fatalf("AlbumMonitorState = %q, want %q when the poll budget ran out", got.AlbumMonitorState, AlbumMonitorStateNotVisibleYet)
	}
	if got.ArtistID != 9 {
		t.Fatalf("ArtistID = %d, want 9 (the artist was still created)", got.ArtistID)
	}
	if monitorCalled {
		t.Fatal("MonitorAlbums must not be called when the album was never found")
	}
}

// TestAddArtistAndMonitorAllAlbums covers the whole-discography choice:
// every album id from AlbumsByArtist must be monitored, not just the one
// that prompted the add.
func TestAddArtistAndMonitorAllAlbums(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		// Every id in this discography is reported monitored: this doubles
		// as the source AddArtistAndMonitor builds monitorIDs from and as
		// the post-apply verification's read-back, which is fine for a
		// stateless mock - the revert/re-apply path has its own dedicated
		// tests below.
		albumsByArtist: monitoredAlbums(1, 2, 42),
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error {
			if len(albumIDs) != 3 || !monitored {
				t.Fatalf("unexpected monitorAlbums call: ids=%v monitored=%v", albumIDs, monitored)
			}
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	params := validAddArtistParams()
	params.Monitor = MonitorAllAlbums
	got, err := l.AddArtistAndMonitor(context.Background(), params)
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if got.AlbumMonitorState != AlbumMonitorStateMonitored {
		t.Fatalf("AlbumMonitorState = %q, want %q", got.AlbumMonitorState, AlbumMonitorStateMonitored)
	}
}

// TestAddArtistAndMonitorCreatesUnmonitoredThenSetsMonitored covers the
// verified sequence: a newly created artist must be explicitly marked
// monitored afterward, since AddArtist itself always creates it unmonitored.
func TestAddArtistAndMonitorCreatesThenSetsMonitored(t *testing.T) {
	var setMonitoredArtistID int64
	var setMonitoredValue bool
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error {
			// Only capture the first call - the initial explicit mark.
			// Re-apply calls (none expected here, since verification should
			// pass first time) would otherwise overwrite these.
			if setMonitoredArtistID == 0 {
				setMonitoredArtistID = artistID
				setMonitoredValue = monitored
			}
			return nil
		},
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		albumsByArtist: monitoredAlbums(42),
		monitorAlbums:  func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	if _, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams()); err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if setMonitoredArtistID != 9 || !setMonitoredValue {
		t.Fatalf("SetArtistMonitored called with artistID=%d monitored=%v, want 9/true", setMonitoredArtistID, setMonitoredValue)
	}
}

// TestAddArtistAndMonitorWaitsForIdleBeforeMonitoring covers issue #331's
// second bug (testenv/seed_lidarr.py's wait_for_idle): SetArtistMonitored
// must not run while Lidarr still reports a queued/started
// RefreshArtist/RefreshAlbum/RescanFolders command. RunningCommands reports
// busy for the first two polls and idle on the third; SetArtistMonitored
// must only ever be called after the command list has gone idle.
func TestAddArtistAndMonitorWaitsForIdleBeforeMonitoring(t *testing.T) {
	var order []string
	commandCalls := 0
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		runningCommands: func(ctx context.Context) ([]core.LidarrCommand, error) {
			commandCalls++
			order = append(order, "commands")
			if commandCalls < 3 {
				return []core.LidarrCommand{{Name: "RefreshArtist", Status: "started"}}, nil
			}
			return []core.LidarrCommand{{Name: "RefreshArtist", Status: "completed"}}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error {
			order = append(order, "setMonitored")
			return nil
		},
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		albumsByArtist: monitoredAlbums(42),
		monitorAlbums:  func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{
		Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond,
		RefreshPollAttempts: 5, RefreshPollInterval: time.Millisecond,
	})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if got.AlbumMonitorState != AlbumMonitorStateMonitored {
		t.Fatalf("AlbumMonitorState = %q, want %q", got.AlbumMonitorState, AlbumMonitorStateMonitored)
	}
	if commandCalls != 3 {
		t.Fatalf("commandCalls = %d, want 3 (busy, busy, idle)", commandCalls)
	}
	setMonitoredAt := -1
	for i, step := range order {
		if step == "setMonitored" {
			setMonitoredAt = i
			break
		}
	}
	if setMonitoredAt == -1 {
		t.Fatal("SetArtistMonitored was never called")
	}
	for i, step := range order[:setMonitoredAt] {
		if step != "commands" {
			t.Fatalf("order[%d] = %q, want every step before setMonitored to be a commands poll: %v", i, step, order)
		}
	}
	if setMonitoredAt != 3 {
		t.Fatalf("SetArtistMonitored ran after %d commands polls, want exactly 3: %v", setMonitoredAt, order)
	}
}

// TestAddArtistAndMonitorReappliesMonitoringAfterRefreshReverts covers issue
// #331's second bug end to end: even after waitForIdle gives the all-clear,
// a refresh landing in the gap can still revert monitoring. The first
// verification must see it reverted, trigger exactly one re-apply, and the
// second verification must confirm it stuck.
func TestAddArtistAndMonitorReappliesMonitoringAfterRefreshReverts(t *testing.T) {
	artistCalls := 0
	verifyCalls := 0
	var setMonitoredCalls, monitorAlbumsCalls int
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			artistCalls++
			if artistCalls == 1 {
				return core.LidarrArtist{}, false, nil // ensureArtist: not created yet
			}
			verifyCalls++
			if verifyCalls == 1 {
				return core.LidarrArtist{ID: 9, Monitored: false}, true, nil // reverted by the refresh
			}
			return core.LidarrArtist{ID: 9, Monitored: true}, true, nil // re-apply took
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error {
			setMonitoredCalls++
			return nil
		},
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		albumsByArtist: func(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error) {
			// Mirrors the artist's monitored state: only "monitored" once the
			// re-apply has run (verifyCalls reached 2).
			return []core.LidarrAlbum{{ID: 42, Monitored: verifyCalls >= 2}}, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error {
			monitorAlbumsCalls++
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if got.AlbumMonitorState != AlbumMonitorStateMonitored {
		t.Fatalf("AlbumMonitorState = %q, want %q once the single re-apply confirmed", got.AlbumMonitorState, AlbumMonitorStateMonitored)
	}
	if setMonitoredCalls != 2 {
		t.Fatalf("setMonitoredCalls = %d, want 2 (initial + one re-apply)", setMonitoredCalls)
	}
	if monitorAlbumsCalls != 2 {
		t.Fatalf("monitorAlbumsCalls = %d, want 2 (initial + one re-apply)", monitorAlbumsCalls)
	}
}

// TestAddArtistAndMonitorHonestlyReportsWhenReapplyDoesNotStick covers the
// give-up path: if monitoring is still reverted even after the single
// re-apply, AddArtistAndMonitor must not report success it cannot confirm -
// AlbumMonitored must be false, with no error (the artist genuinely exists).
func TestAddArtistAndMonitorHonestlyReportsWhenReapplyDoesNotStick(t *testing.T) {
	created := false
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			if !created {
				return core.LidarrArtist{}, false, nil
			}
			// Every call after creation - both verification and
			// re-verification - still reports unmonitored.
			return core.LidarrArtist{ID: 9, Monitored: false}, true, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			created = true
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor should not error just because monitoring could not be confirmed, got %v", err)
	}
	if got.AlbumMonitorState != AlbumMonitorStateReverted {
		t.Fatalf("AlbumMonitorState = %q, want %q when a refresh keeps reverting monitoring even after one re-apply", got.AlbumMonitorState, AlbumMonitorStateReverted)
	}
	if got.ArtistID != 9 {
		t.Fatalf("ArtistID = %d, want 9 - the artist genuinely exists even though monitoring could not be confirmed", got.ArtistID)
	}
	if got.ArtistMonitored {
		t.Fatal("ArtistMonitored must be false when verification never once saw it as monitored")
	}
}

// --- issue #331 backend review coverage -----------------------------------

// TestAddArtistAndMonitorRejectsUnknownRootFolder covers backend review #7:
// RootFolderPath is the one user-controlled string in this flow that reaches
// an upstream write unchecked, so it must be validated against Lidarr's own
// configured set before anything else runs.
func TestAddArtistAndMonitorRejectsUnknownRootFolder(t *testing.T) {
	var addArtistCalled bool
	client := &fakeLidarrLibraryClient{
		rootFolders: func(ctx context.Context) ([]core.LidarrRootFolder, error) {
			return []core.LidarrRootFolder{{ID: 1, Path: "/music/other", Accessible: true}}, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			addArtistCalled = true
			return core.LidarrArtist{}, errors.New("must not be called")
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	_, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if !errors.Is(err, ErrLidarrLibraryInvalidRootFolder) {
		t.Fatalf("AddArtistAndMonitor(unknown root folder) = %v, want ErrLidarrLibraryInvalidRootFolder", err)
	}
	if addArtistCalled {
		t.Fatal("AddArtist must not be called once root folder validation fails")
	}
}

// TestAddArtistAndMonitorRejectsInaccessibleRootFolder covers the other half
// of #7: a path that matches a configured folder but isn't accessible must
// still be rejected.
func TestAddArtistAndMonitorRejectsInaccessibleRootFolder(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		rootFolders: func(ctx context.Context) ([]core.LidarrRootFolder, error) {
			return []core.LidarrRootFolder{{ID: 1, Path: "/music/library", Accessible: false}}, nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	_, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if !errors.Is(err, ErrLidarrLibraryInvalidRootFolder) {
		t.Fatalf("AddArtistAndMonitor(inaccessible root folder) = %v, want ErrLidarrLibraryInvalidRootFolder", err)
	}
}

// TestAddArtistAndMonitorAllAlbumsUnionsRatherThanOverwrites covers backend
// review #5: AlbumsByArtist can come back without the just-resolved target
// album if Lidarr's refresh is still mid-flight - monitorIDs must union it
// in, not be overwritten wholesale.
func TestAddArtistAndMonitorAllAlbumsUnionsRatherThanOverwrites(t *testing.T) {
	var monitored []int64
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		// Deliberately omits album 42 - the target album - simulating a
		// refresh that hasn't caught up with the one AlbumByForeignID
		// already resolved.
		albumsByArtist: monitoredAlbums(1, 2),
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitoredArg bool) error {
			monitored = albumIDs
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	params := validAddArtistParams()
	params.Monitor = MonitorAllAlbums
	if _, err := l.AddArtistAndMonitor(context.Background(), params); err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	found42 := false
	for _, id := range monitored {
		if id == 42 {
			found42 = true
		}
	}
	if !found42 {
		t.Fatalf("monitorAlbums call = %v, want it to include the target album 42 even though AlbumsByArtist omitted it", monitored)
	}
	if len(monitored) != 3 {
		t.Fatalf("monitorAlbums call = %v, want exactly 3 ids (1, 2, 42)", monitored)
	}
}

// TestAddArtistAndMonitorAllAlbumsEmptyAlbumsByArtistStillMonitorsTarget
// covers the sharper edge of backend review #5: if AlbumsByArtist comes back
// completely empty (a fully mid-flight refresh), the target album must still
// be monitored rather than MonitorAlbums being skipped or called with an
// empty set that silently "succeeds" having monitored nothing.
func TestAddArtistAndMonitorAllAlbumsEmptyAlbumsByArtistStillMonitorsTarget(t *testing.T) {
	var monitorAlbumsCalled bool
	var monitored []int64
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		albumsByArtist: func(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error) {
			return nil, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitoredArg bool) error {
			monitorAlbumsCalled = true
			monitored = albumIDs
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	params := validAddArtistParams()
	params.Monitor = MonitorAllAlbums
	got, err := l.AddArtistAndMonitor(context.Background(), params)
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if !monitorAlbumsCalled || len(monitored) != 1 || monitored[0] != 42 {
		t.Fatalf("monitorAlbums call = %v (called=%v), want exactly [42]", monitored, monitorAlbumsCalled)
	}
	if got.AlbumMonitorState == AlbumMonitorStateUnknown {
		t.Fatal("AlbumMonitorState must not be unknown - the target album was still monitored")
	}
}

// TestAddArtistAndMonitorAlreadyInLibraryReportsExistingMonitoredFlag covers
// backend review #3: retrying after a falsely-reported failure must not
// claim success on an artist that exists but was never actually monitored -
// ArtistMonitored must reflect Lidarr's real Monitored flag, not be assumed
// true just because the artist already exists.
func TestAddArtistAndMonitorAlreadyInLibraryReportsExistingMonitoredFlag(t *testing.T) {
	var setMonitoredCalled bool
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{ID: 9, Monitored: false}, true, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error {
			setMonitoredCalled = true
			return nil
		},
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if setMonitoredCalled {
		t.Fatal("SetArtistMonitored must never be called for an artist already in the library")
	}
	if got.ArtistMonitored {
		t.Fatal("ArtistMonitored must be false - the existing artist genuinely isn't monitored, and AddArtistAndMonitor must not silently claim otherwise")
	}
	if got.AlbumMonitorState != AlbumMonitorStateMonitored {
		t.Fatalf("AlbumMonitorState = %q, want %q - the album write itself still succeeded", got.AlbumMonitorState, AlbumMonitorStateMonitored)
	}
}

// TestAddArtistAndMonitorAlbumBelongsToWrongArtist covers backend review #8's
// smaller item: the resolved album's ArtistID must be checked against the
// artist just ensured before monitoring it, since the endpoint is
// independently callable with a mismatched artist/album pair.
func TestAddArtistAndMonitorAlbumBelongsToWrongArtist(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 999}, true, nil // belongs to a different artist
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error {
			t.Fatal("MonitorAlbums must not be called when the album belongs to a different artist")
			return nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	if _, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams()); err == nil {
		t.Fatal("expected an error when the resolved album belongs to a different artist")
	}
}

// TestAddArtistAndMonitorReportsUnknownOnVerificationTransportError covers
// backend review #6: a transport error during verification must not be
// reported as "reverted" - that would claim monitoring failed when it may
// well be fine, just unconfirmed.
func TestAddArtistAndMonitorReportsUnknownOnVerificationTransportError(t *testing.T) {
	calls := 0
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			calls++
			if calls == 1 {
				return core.LidarrArtist{}, false, nil // ensureArtist: not created yet
			}
			// Every call from here on is verification - always a transport error.
			return core.LidarrArtist{}, false, errors.New("connection refused")
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		monitorAlbums: func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond})
	got, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("AddArtistAndMonitor should not error just because verification transport-failed, got %v", err)
	}
	if got.AlbumMonitorState != AlbumMonitorStateUnknown {
		t.Fatalf("AlbumMonitorState = %q, want %q", got.AlbumMonitorState, AlbumMonitorStateUnknown)
	}
	if !got.ArtistMonitored {
		t.Fatal("ArtistMonitored must not be reported false on a verification transport error - the explicit SetArtistMonitored(true) call itself succeeded")
	}
}

// TestWaitForIdleIgnoresUnrelatedArtistRefresh covers backend review #2:
// GET /command is instance-wide, so a running command scoped to a different
// artist must not hold up monitoring our own add.
func TestWaitForIdleIgnoresUnrelatedArtistRefresh(t *testing.T) {
	commandCalls := 0
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		runningCommands: func(ctx context.Context) ([]core.LidarrCommand, error) {
			commandCalls++
			return []core.LidarrCommand{{Name: "RefreshArtist", Status: "started", ArtistIDs: []int64{999}}}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		albumsByArtist: monitoredAlbums(42),
		monitorAlbums:  func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{
		Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond,
		RefreshPollAttempts: 5, RefreshPollInterval: time.Millisecond,
	})
	if _, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams()); err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if commandCalls != 1 {
		t.Fatalf("commandCalls = %d, want 1 - a command scoped to a different artist must not be treated as busy", commandCalls)
	}
}

// TestWaitForIdleBlocksOnCommandScopedToOurArtist is the positive
// counterpart: a command explicitly scoped to our artist id must still
// block, exactly like the pre-scoping behaviour did for every command.
func TestWaitForIdleBlocksOnCommandScopedToOurArtist(t *testing.T) {
	commandCalls := 0
	client := &fakeLidarrLibraryClient{
		artistByMBID: newlyCreatedThenMonitoredArtist(9),
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{ID: 9}, nil
		},
		runningCommands: func(ctx context.Context) ([]core.LidarrCommand, error) {
			commandCalls++
			if commandCalls < 3 {
				return []core.LidarrCommand{{Name: "RefreshArtist", Status: "started", ArtistIDs: []int64{9}}}, nil
			}
			return []core.LidarrCommand{{Name: "RefreshArtist", Status: "completed", ArtistIDs: []int64{9}}}, nil
		},
		setMonitored: func(ctx context.Context, artistID int64, monitored bool) error { return nil },
		albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
			return core.LidarrAlbum{ID: 42, ArtistID: 9}, true, nil
		},
		albumsByArtist: monitoredAlbums(42),
		monitorAlbums:  func(ctx context.Context, albumIDs []int64, monitored bool) error { return nil },
	}
	l := NewLidarrLibrary(LidarrLibraryParams{
		Lidarr: client, AlbumPollAttempts: 1, AlbumPollInterval: time.Millisecond,
		RefreshPollAttempts: 5, RefreshPollInterval: time.Millisecond,
	})
	if _, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams()); err != nil {
		t.Fatalf("AddArtistAndMonitor: %v", err)
	}
	if commandCalls != 3 {
		t.Fatalf("commandCalls = %d, want 3 (busy, busy, idle) - a command scoped to our own artist must block", commandCalls)
	}
}

// TestAddArtistAndMonitorAddUncertainPropagates covers backend review's wire
// contract: ErrAddArtistUncertain from the underlying client (surfaced here
// as core.ErrLidarrAddArtistUncertain, aliased as ErrLidarrLibraryAddUncertain)
// must survive AddArtistAndMonitor's error wrapping intact, since
// internal/observ maps it to a distinct 502 rather than the generic 500.
func TestAddArtistAndMonitorAddUncertainPropagates(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{}, false, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{}, fmt.Errorf("%w: create failed and re-check failed too", core.ErrLidarrAddArtistUncertain)
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	_, err := l.AddArtistAndMonitor(context.Background(), validAddArtistParams())
	if !errors.Is(err, ErrLidarrLibraryAddUncertain) {
		t.Fatalf("AddArtistAndMonitor = %v, want an error wrapping ErrLidarrLibraryAddUncertain", err)
	}
}
