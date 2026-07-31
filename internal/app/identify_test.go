package app

import (
	"context"
	"errors"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// fakeMusicBrainz is a MusicBrainzSearcher test double.
type fakeMusicBrainz struct {
	searchArtists func(ctx context.Context, query string) ([]core.MBArtist, error)
	releaseGroups func(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, error)
	releases      func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, error)
}

func (f *fakeMusicBrainz) SearchArtists(ctx context.Context, query string) ([]core.MBArtist, error) {
	return f.searchArtists(ctx, query)
}

func (f *fakeMusicBrainz) ReleaseGroups(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, error) {
	return f.releaseGroups(ctx, artistMBID)
}

func (f *fakeMusicBrainz) Releases(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, error) {
	return f.releases(ctx, releaseGroupMBID)
}

// fakeLidarrLookup is a LidarrLibraryLookup test double.
type fakeLidarrLookup struct {
	albumByForeignID func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
}

func (f *fakeLidarrLookup) AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
	return f.albumByForeignID(ctx, foreignAlbumID)
}

func TestIdentifySearchArtistsValidatesQuery(t *testing.T) {
	id := NewIdentify(IdentifyParams{MusicBrainz: &fakeMusicBrainz{}})
	if _, err := id.SearchArtists(context.Background(), "   "); !errors.Is(err, ErrIdentifyQueryInvalid) {
		t.Fatalf("SearchArtists(blank) = %v, want ErrIdentifyQueryInvalid", err)
	}
}

func TestIdentifySearchArtistsMapsBackendErrorToUnavailable(t *testing.T) {
	mb := &fakeMusicBrainz{searchArtists: func(ctx context.Context, query string) ([]core.MBArtist, error) {
		return nil, errors.New("boom")
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	if _, err := id.SearchArtists(context.Background(), "metallica"); !errors.Is(err, ErrIdentifyUnavailable) {
		t.Fatalf("SearchArtists = %v, want ErrIdentifyUnavailable", err)
	}
}

func TestIdentifySearchArtistsSuccess(t *testing.T) {
	want := []core.MBArtist{{ID: "a1", Name: "Metallica"}}
	mb := &fakeMusicBrainz{searchArtists: func(ctx context.Context, query string) ([]core.MBArtist, error) {
		if query != "metallica" {
			t.Errorf("query = %q", query)
		}
		return want, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	got, err := id.SearchArtists(context.Background(), "metallica")
	if err != nil {
		t.Fatalf("SearchArtists: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestIdentifyArtistAlbumsValidatesID(t *testing.T) {
	id := NewIdentify(IdentifyParams{MusicBrainz: &fakeMusicBrainz{}})
	if _, err := id.ArtistAlbums(context.Background(), ""); !errors.Is(err, ErrIdentifyQueryInvalid) {
		t.Fatalf("ArtistAlbums(blank) = %v, want ErrIdentifyQueryInvalid", err)
	}
}

func TestIdentifyArtistAlbumsMapsBackendErrorToUnavailable(t *testing.T) {
	mb := &fakeMusicBrainz{releaseGroups: func(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, error) {
		return nil, errors.New("boom")
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	if _, err := id.ArtistAlbums(context.Background(), "a1"); !errors.Is(err, ErrIdentifyUnavailable) {
		t.Fatalf("ArtistAlbums = %v, want ErrIdentifyUnavailable", err)
	}
}

func TestIdentifyAlbumEditionsValidatesID(t *testing.T) {
	id := NewIdentify(IdentifyParams{MusicBrainz: &fakeMusicBrainz{}})
	if _, err := id.AlbumEditions(context.Background(), ""); !errors.Is(err, ErrIdentifyQueryInvalid) {
		t.Fatalf("AlbumEditions(blank) = %v, want ErrIdentifyQueryInvalid", err)
	}
}

func TestIdentifyAlbumEditionsMapsBackendErrorToUnavailable(t *testing.T) {
	mb := &fakeMusicBrainz{releases: func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, error) {
		return nil, errors.New("boom")
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	if _, err := id.AlbumEditions(context.Background(), "rg1"); !errors.Is(err, ErrIdentifyUnavailable) {
		t.Fatalf("AlbumEditions = %v, want ErrIdentifyUnavailable", err)
	}
}

func TestIdentifyAlbumEditionsSuccess(t *testing.T) {
	want := []core.MBRelease{{ID: "r1", TrackCount: 8, TrackCountKnown: true}}
	mb := &fakeMusicBrainz{releases: func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, error) {
		return want, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	got, err := id.AlbumEditions(context.Background(), "rg1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if len(got) != 1 || got[0].TrackCount != 8 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestIdentifyAlbumLidarrStatusValidatesID(t *testing.T) {
	id := NewIdentify(IdentifyParams{Lidarr: &fakeLidarrLookup{}})
	if _, err := id.AlbumLidarrStatus(context.Background(), ""); !errors.Is(err, ErrIdentifyQueryInvalid) {
		t.Fatalf("AlbumLidarrStatus(blank) = %v, want ErrIdentifyQueryInvalid", err)
	}
}

func TestIdentifyAlbumLidarrStatusInLibrary(t *testing.T) {
	lidarr := &fakeLidarrLookup{albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
		return core.LidarrAlbum{ID: 42}, true, nil
	}}
	id := NewIdentify(IdentifyParams{Lidarr: lidarr})
	got, err := id.AlbumLidarrStatus(context.Background(), "rg1")
	if err != nil {
		t.Fatalf("AlbumLidarrStatus: %v", err)
	}
	if !got.Known || !got.InLibrary || got.AlbumID != 42 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestIdentifyAlbumLidarrStatusNotInLibrary(t *testing.T) {
	lidarr := &fakeLidarrLookup{albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
		return core.LidarrAlbum{}, false, nil
	}}
	id := NewIdentify(IdentifyParams{Lidarr: lidarr})
	got, err := id.AlbumLidarrStatus(context.Background(), "rg1")
	if err != nil {
		t.Fatalf("AlbumLidarrStatus: %v", err)
	}
	if !got.Known || got.InLibrary {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestIdentifyAlbumLidarrStatusUnreachableIsUnknownNotError covers issue
// #321's design: a Lidarr transport failure must surface as Known=false, not
// as a request error - "unknown" is a normal, displayable outcome here.
func TestIdentifyAlbumLidarrStatusUnreachableIsUnknownNotError(t *testing.T) {
	lidarr := &fakeLidarrLookup{albumByForeignID: func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
		return core.LidarrAlbum{}, false, errors.New("connection refused")
	}}
	id := NewIdentify(IdentifyParams{Lidarr: lidarr})
	got, err := id.AlbumLidarrStatus(context.Background(), "rg1")
	if err != nil {
		t.Fatalf("AlbumLidarrStatus should not return an error for an unreachable Lidarr, got %v", err)
	}
	if got.Known {
		t.Fatalf("expected Known = false, got %+v", got)
	}
}
