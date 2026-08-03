package app

import (
	"context"
	"errors"
	"testing"

	"github.com/samuelenocsson/slusk/internal/core"
)

// fakeMusicBrainz is a MusicBrainzSearcher test double.
type fakeMusicBrainz struct {
	searchReleaseGroups func(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error)
	releases            func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error)
}

func (f *fakeMusicBrainz) SearchReleaseGroups(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error) {
	return f.searchReleaseGroups(ctx, artist, album)
}

func (f *fakeMusicBrainz) Releases(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error) {
	return f.releases(ctx, releaseGroupMBID)
}

// fakeLidarrLookup is a LidarrLibraryLookup test double.
type fakeLidarrLookup struct {
	albumByForeignID func(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
}

func (f *fakeLidarrLookup) AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
	return f.albumByForeignID(ctx, foreignAlbumID)
}

func TestIdentifySearchReleaseGroupsValidatesAlbum(t *testing.T) {
	id := NewIdentify(IdentifyParams{MusicBrainz: &fakeMusicBrainz{}})
	if _, _, err := id.SearchReleaseGroups(context.Background(), "metallica", "   "); !errors.Is(err, ErrIdentifyQueryInvalid) {
		t.Fatalf("SearchReleaseGroups(blank album) = %v, want ErrIdentifyQueryInvalid", err)
	}
}

// TestIdentifySearchReleaseGroupsAllowsBlankArtist covers issue #321's
// requirement that artist may be blank - only album is required.
func TestIdentifySearchReleaseGroupsAllowsBlankArtist(t *testing.T) {
	mb := &fakeMusicBrainz{searchReleaseGroups: func(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error) {
		if artist != "" {
			t.Errorf("artist = %q, want blank", artist)
		}
		return []core.MBReleaseGroup{{ID: "rg1"}}, 1, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	if _, _, err := id.SearchReleaseGroups(context.Background(), "  ", "ride the lightning"); err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
}

func TestIdentifySearchReleaseGroupsMapsBackendErrorToUnavailable(t *testing.T) {
	mb := &fakeMusicBrainz{searchReleaseGroups: func(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error) {
		return nil, 0, errors.New("boom")
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	if _, _, err := id.SearchReleaseGroups(context.Background(), "metallica", "ride the lightning"); !errors.Is(err, ErrIdentifyUnavailable) {
		t.Fatalf("SearchReleaseGroups = %v, want ErrIdentifyUnavailable", err)
	}
}

func TestIdentifySearchReleaseGroupsSuccess(t *testing.T) {
	want := []core.MBReleaseGroup{{ID: "rg1", Title: "Ride the Lightning", ArtistName: "Metallica"}}
	mb := &fakeMusicBrainz{searchReleaseGroups: func(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error) {
		if artist != "metallica" || album != "ride the lightning" {
			t.Errorf("artist = %q, album = %q", artist, album)
		}
		return want, 1, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	got, total, err := id.SearchReleaseGroups(context.Background(), "metallica", "ride the lightning")
	if err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if len(got) != 1 || got[0].ID != "rg1" {
		t.Fatalf("unexpected: %+v", got)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// TestIdentifySearchReleaseGroupsSurfacesTotalPastCap covers issue #321's
// review finding: when the backend's slice was capped, total must still
// report the true match count so the caller can detect the truncation.
func TestIdentifySearchReleaseGroupsSurfacesTotalPastCap(t *testing.T) {
	mb := &fakeMusicBrainz{searchReleaseGroups: func(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error) {
		return []core.MBReleaseGroup{{ID: "rg1"}}, 250, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	got, total, err := id.SearchReleaseGroups(context.Background(), "metallica", "ride the lightning")
	if err != nil {
		t.Fatalf("SearchReleaseGroups: %v", err)
	}
	if total != 250 || total == len(got) {
		t.Fatalf("total = %d, want 250 (exceeding len(got)=%d)", total, len(got))
	}
}

func TestIdentifyAlbumEditionsValidatesID(t *testing.T) {
	id := NewIdentify(IdentifyParams{MusicBrainz: &fakeMusicBrainz{}})
	if _, _, err := id.AlbumEditions(context.Background(), ""); !errors.Is(err, ErrIdentifyQueryInvalid) {
		t.Fatalf("AlbumEditions(blank) = %v, want ErrIdentifyQueryInvalid", err)
	}
}

func TestIdentifyAlbumEditionsMapsBackendErrorToUnavailable(t *testing.T) {
	mb := &fakeMusicBrainz{releases: func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error) {
		return nil, 0, errors.New("boom")
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	if _, _, err := id.AlbumEditions(context.Background(), "rg1"); !errors.Is(err, ErrIdentifyUnavailable) {
		t.Fatalf("AlbumEditions = %v, want ErrIdentifyUnavailable", err)
	}
}

func TestIdentifyAlbumEditionsSuccess(t *testing.T) {
	want := []core.MBRelease{{ID: "r1", TrackCount: 8, TrackCountKnown: true}}
	mb := &fakeMusicBrainz{releases: func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error) {
		return want, 1, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	got, total, err := id.AlbumEditions(context.Background(), "rg1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if len(got) != 1 || got[0].TrackCount != 8 {
		t.Fatalf("unexpected: %+v", got)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// TestIdentifyAlbumEditionsSurfacesTotalPastCap covers issue #321's review
// finding: when the backend's slice was capped, total must still report the
// true match count so the caller can detect the truncation.
func TestIdentifyAlbumEditionsSurfacesTotalPastCap(t *testing.T) {
	mb := &fakeMusicBrainz{releases: func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error) {
		return []core.MBRelease{{ID: "r1"}}, 60, nil
	}}
	id := NewIdentify(IdentifyParams{MusicBrainz: mb})
	got, total, err := id.AlbumEditions(context.Background(), "rg1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if total != 60 || total == len(got) {
		t.Fatalf("total = %d, want 60 (exceeding len(got)=%d)", total, len(got))
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
