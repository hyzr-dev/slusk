package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// fakeLidarrLibraryClient is a LidarrLibraryClient test double.
type fakeLidarrLibraryClient struct {
	artistByMBID     func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error)
	rootFolders      func(ctx context.Context) ([]core.LidarrRootFolder, error)
	qualityProfiles  func(ctx context.Context) ([]core.LidarrProfile, error)
	metadataProfiles func(ctx context.Context) ([]core.LidarrProfile, error)
	addArtist        func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error)
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

// The fake implements no monitoring method at all, and this assertion is what
// keeps it that way: the add flow monitors nothing (see the package doc
// comment), so a SetArtistMonitored/MonitorAlbums re-added to
// LidarrLibraryClient would fail to compile here rather than quietly gaining a
// caller.
var _ LidarrLibraryClient = &fakeLidarrLibraryClient{}

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
		ArtistMBID: "artist-1", ArtistName: "Aphex Twin",
		RootFolderPath: "/music/library", QualityProfileID: 2, MetadataProfileID: 1,
	}
}

func TestEnsureArtistValidatesRequiredFields(t *testing.T) {
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: &fakeLidarrLibraryClient{}})
	cases := []struct {
		name   string
		mutate func(p AddArtistParams) AddArtistParams
	}{
		{"blank artist mbid", func(p AddArtistParams) AddArtistParams { p.ArtistMBID = " "; return p }},
		{"blank root folder", func(p AddArtistParams) AddArtistParams { p.RootFolderPath = " "; return p }},
		{"zero quality profile", func(p AddArtistParams) AddArtistParams { p.QualityProfileID = 0; return p }},
		{"zero metadata profile", func(p AddArtistParams) AddArtistParams { p.MetadataProfileID = 0; return p }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.EnsureArtist(context.Background(), c.mutate(validAddArtistParams())); !errors.Is(err, ErrLidarrLibraryQueryInvalid) {
				t.Fatalf("EnsureArtist(%s) = %v, want ErrLidarrLibraryQueryInvalid", c.name, err)
			}
		})
	}
}

// TestEnsureArtistAlreadyInLibrary covers issue #331's guard: an existing
// artist must not be re-created, and nothing about it may be touched.
func TestEnsureArtistAlreadyInLibrary(t *testing.T) {
	var addArtistCalled bool
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{ID: 9}, true, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			addArtistCalled = true
			return core.LidarrArtist{}, errors.New("must not be called")
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	got, err := l.EnsureArtist(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("EnsureArtist: %v", err)
	}
	if addArtistCalled {
		t.Fatal("AddArtist must not be called for an artist already in the library")
	}
	if !got.AlreadyInLibrary || got.ArtistID != 9 {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestEnsureArtistCreatesMissingArtist covers the create path: the request
// carries exactly the form's fields through to Lidarr, and the result reports
// the new id as not-already-in-library.
func TestEnsureArtistCreatesMissingArtist(t *testing.T) {
	var got core.AddArtistRequest
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{}, false, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			got = req
			return core.LidarrArtist{ID: 9}, nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	result, err := l.EnsureArtist(context.Background(), validAddArtistParams())
	if err != nil {
		t.Fatalf("EnsureArtist: %v", err)
	}
	want := core.AddArtistRequest{
		ForeignArtistID: "artist-1", ArtistName: "Aphex Twin",
		QualityProfileID: 2, MetadataProfileID: 1, RootFolderPath: "/music/library",
	}
	if got != want {
		t.Fatalf("AddArtist request = %+v, want %+v", got, want)
	}
	if result.ArtistID != 9 || result.AlreadyInLibrary {
		t.Fatalf("unexpected: %+v", result)
	}
}

// --- issue #331 backend review coverage -----------------------------------

// TestEnsureArtistRejectsUnknownRootFolder covers backend review #7:
// RootFolderPath is the one user-controlled string in this flow that reaches
// an upstream write unchecked, so it must be validated against Lidarr's own
// configured set before anything else runs.
func TestEnsureArtistRejectsUnknownRootFolder(t *testing.T) {
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
	_, err := l.EnsureArtist(context.Background(), validAddArtistParams())
	if !errors.Is(err, ErrLidarrLibraryInvalidRootFolder) {
		t.Fatalf("EnsureArtist(unknown root folder) = %v, want ErrLidarrLibraryInvalidRootFolder", err)
	}
	if addArtistCalled {
		t.Fatal("AddArtist must not be called once root folder validation fails")
	}
}

// TestEnsureArtistRejectsInaccessibleRootFolder covers the other half of #7: a
// path that matches a configured folder but isn't accessible must still be
// rejected.
func TestEnsureArtistRejectsInaccessibleRootFolder(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		rootFolders: func(ctx context.Context) ([]core.LidarrRootFolder, error) {
			return []core.LidarrRootFolder{{ID: 1, Path: "/music/library", Accessible: false}}, nil
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	_, err := l.EnsureArtist(context.Background(), validAddArtistParams())
	if !errors.Is(err, ErrLidarrLibraryInvalidRootFolder) {
		t.Fatalf("EnsureArtist(inaccessible root folder) = %v, want ErrLidarrLibraryInvalidRootFolder", err)
	}
}

// TestEnsureArtistAddUncertainPropagates covers backend review's wire
// contract: ErrAddArtistUncertain from the underlying client (surfaced here
// as core.ErrLidarrAddArtistUncertain, aliased as ErrLidarrLibraryAddUncertain)
// must survive EnsureArtist's error wrapping intact, since internal/observ
// maps it to a distinct 502 rather than the generic 500.
func TestEnsureArtistAddUncertainPropagates(t *testing.T) {
	client := &fakeLidarrLibraryClient{
		artistByMBID: func(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
			return core.LidarrArtist{}, false, nil
		},
		addArtist: func(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
			return core.LidarrArtist{}, fmt.Errorf("%w: create failed and re-check failed too", core.ErrLidarrAddArtistUncertain)
		},
	}
	l := NewLidarrLibrary(LidarrLibraryParams{Lidarr: client})
	_, err := l.EnsureArtist(context.Background(), validAddArtistParams())
	if !errors.Is(err, ErrLidarrLibraryAddUncertain) {
		t.Fatalf("EnsureArtist = %v, want an error wrapping ErrLidarrLibraryAddUncertain", err)
	}
}
