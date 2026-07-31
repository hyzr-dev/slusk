package matcher

import "testing"

// kansasFiles is the false-positive case from issue #316: a peer's real
// share is Kansas's "The Absence Of Presence", which network-matches a
// search for "The Absence" / "The Absence" because Soulseek search is a
// token-AND over the whole path, not "belongs to this album".
var kansasFiles = []string{
	`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\01 - The Absence Of Presence.flac`,
	`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\02 - Throwing Mountains.flac`,
	`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\03 - Jets Overhead.flac`,
	`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\04 - Load Your Ships.flac`,
}

var theAbsenceTrackTitles = []string{
	"Wartorn", "Riders of the Plague", "Skin and Bones", "The Absence", "Void",
}

func TestCheckRelevanceRejectsWrongAlbumViaTrackTitles(t *testing.T) {
	r := CheckRelevance(RelevanceInput{
		ArtistName:  "The Absence",
		AlbumTitle:  "The Absence",
		TrackTitles: theAbsenceTrackTitles,
		Files:       kansasFiles,
	})
	if r.Match {
		t.Fatalf("expected Kansas candidate rejected, got Match=true (%+v)", r)
	}
	if r.Source != SourceTrackTitles {
		t.Errorf("expected decision via SourceTrackTitles, got %v (%+v)", r.Source, r)
	}
}

func TestCheckRelevanceRejectsWrongAlbumViaDirectoryFallback(t *testing.T) {
	files := []string{
		`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\01.flac`,
		`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\02.flac`,
		`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\03.flac`,
		`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\04.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName:  "The Absence",
		AlbumTitle:  "The Absence",
		TrackTitles: theAbsenceTrackTitles,
		Files:       files,
	})
	if r.Match {
		t.Fatalf("expected Kansas candidate (numeric filenames) rejected, got Match=true (%+v)", r)
	}
	if r.Source != SourceDirectory {
		t.Errorf("expected decision via SourceDirectory (no interpretable filenames), got %v (%+v)", r.Source, r)
	}
}

func TestCheckRelevanceRejectsOnLowRecall(t *testing.T) {
	files := []string{
		`The Absence - Riders Of The Plague (2007)\01 - Riders Of The Plague.flac`,
		`The Absence - Riders Of The Plague (2007)\02 - Second Track.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "The Absence",
		AlbumTitle: "The Absence",
		Files:      files,
	})
	if r.Match {
		t.Fatalf("expected rejection on title recall (wrong album, same artist), got %+v", r)
	}
}

func TestCheckRelevanceAcceptsMatchingAlbum(t *testing.T) {
	files := []string{
		`The Absence\The Absence (2016) [FLAC]\01 - Wartorn.flac`,
		`The Absence\The Absence (2016) [FLAC]\02 - Riders Of The Plague.flac`,
		`The Absence\The Absence (2016) [FLAC]\03 - Skin And Bones.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName:  "The Absence",
		AlbumTitle:  "The Absence",
		TrackTitles: theAbsenceTrackTitles,
		Files:       files,
	})
	if !r.Match {
		t.Fatalf("expected the matching candidate accepted, got %+v", r)
	}
}

func TestCheckRelevanceAcceptsSceneReleasePrecision(t *testing.T) {
	// Pins the 0.6 precision constant: directory tokens {the, absence, mtd} -
	// mtd unexplained -> precision 2/3 = 0.67, which must pass (a stricter
	// 0.75 would wrongly reject this common, legitimate naming pattern).
	files := []string{
		`The Absence-The Absence-2016-MTD\101-the_absence-wartorn.mp3`,
		`The Absence-The Absence-2016-MTD\102-the_absence-riders_of_the_plague.mp3`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "The Absence",
		AlbumTitle: "The Absence",
		Files:      files,
	})
	if !r.Match {
		t.Fatalf("expected scene-release directory naming accepted (precision 0.67 >= 0.6), got %+v", r)
	}
}

func TestCheckRelevanceAcceptsCatalogueNumberNoise(t *testing.T) {
	// Without catalogue-number/codec/year noise classification, this
	// directory's unexplained tokens (2016, flac, mb3984, 15107) would sink
	// precision to 2/6 = 0.33; with it, only {the, absence} remain and
	// precision is 1.0.
	files := []string{
		`The Absence - The Absence (2016) [FLAC] {MB3984-15107}\01 - Wartorn.flac`,
		`The Absence - The Absence (2016) [FLAC] {MB3984-15107}\02 - Riders Of The Plague.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "The Absence",
		AlbumTitle: "The Absence",
		Files:      files,
	})
	if !r.Match {
		t.Fatalf("expected catalogue-number-laden directory accepted, got %+v", r)
	}
}

func TestCheckRelevanceAcceptsEditionSuffixOnDirectory(t *testing.T) {
	files := []string{
		`Album X (2011 Remaster) [Deluxe Edition]\01 - Track One.flac`,
		`Album X (2011 Remaster) [Deluxe Edition]\02 - Track Two.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "Some Artist",
		AlbumTitle: "Album X",
		Files:      files,
	})
	if !r.Match {
		t.Fatalf("expected directory with edition marketing suffix accepted, got %+v", r)
	}
}

func TestCheckRelevanceAcceptsPlainDirectoryAgainstEditionTitle(t *testing.T) {
	files := []string{
		`Album X\01 - Track One.flac`,
		`Album X\02 - Track Two.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "Some Artist",
		AlbumTitle: "Album X (Deluxe Edition)",
		Files:      files,
	})
	if !r.Match {
		t.Fatalf("expected plain directory accepted against an edition-suffixed title, got %+v", r)
	}
}

func TestCheckRelevanceMultiDiscFallsBackToParentSegment(t *testing.T) {
	files := []string{
		`The Absence (2016)\CD1\01 - Wartorn.flac`,
		`The Absence (2016)\CD1\02 - Riders Of The Plague.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "The Absence",
		AlbumTitle: "The Absence",
		Files:      files,
	})
	if !r.Match {
		t.Fatalf("expected multi-disc candidate accepted via parent segment, got %+v", r)
	}
	if r.Source != SourceDirectory {
		t.Errorf("expected SourceDirectory, got %v", r.Source)
	}
}

func TestCheckRelevanceDiacriticFolding(t *testing.T) {
	files := []string{
		`Motorhead\Motorhead (1977)\01 - Motorhead.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName: "Motörhead",
		AlbumTitle: "Motörhead",
		Files:      files,
	})
	if !r.Match {
		t.Fatalf("expected diacritic-folded artist/title to match a plain-ASCII directory, got %+v", r)
	}
}

func TestCheckRelevanceEmptyTrackTitlesFallsBackToDirectory(t *testing.T) {
	files := []string{
		`The Absence\The Absence (2016)\01 - Wartorn.flac`,
	}
	r := CheckRelevance(RelevanceInput{
		ArtistName:  "The Absence",
		AlbumTitle:  "The Absence",
		TrackTitles: nil,
		Files:       files,
	})
	if !r.Match || r.Source != SourceDirectory {
		t.Fatalf("expected directory fallback to accept with no track titles, got %+v", r)
	}
}

func TestCheckRelevanceEmptyAlbumTitleIsNoData(t *testing.T) {
	r := CheckRelevance(RelevanceInput{
		ArtistName: "Some Artist",
		AlbumTitle: "",
		Files:      []string{`anything\goes\here.flac`},
	})
	if !r.Match || r.Source != SourceNoData {
		t.Fatalf("expected an empty album title to skip the check entirely, got %+v", r)
	}
}

// TestCheckRelevanceInterpretableFloor pins the minInterpretableFiles(3) and
// minInterpretableRatio(0.5) constants with a named-threshold test, per issue
// #316's explicit requirement.
func TestCheckRelevanceInterpretableFloor(t *testing.T) {
	t.Run("below absolute floor falls back to directory", func(t *testing.T) {
		// 6 files, only 2 interpretable (both matching) -> 2 < minInterpretableFiles(3).
		files := []string{
			`The Absence\The Absence (2016)\01 - Wartorn.flac`,
			`The Absence\The Absence (2016)\02 - Riders Of The Plague.flac`,
			`The Absence\The Absence (2016)\03.flac`,
			`The Absence\The Absence (2016)\04.flac`,
			`The Absence\The Absence (2016)\05.flac`,
			`The Absence\The Absence (2016)\06.flac`,
		}
		r := CheckRelevance(RelevanceInput{
			ArtistName:  "The Absence",
			AlbumTitle:  "The Absence",
			TrackTitles: theAbsenceTrackTitles,
			Files:       files,
		})
		if r.Source != SourceDirectory {
			t.Fatalf("expected SourceDirectory (below interpretable floor), got %+v", r)
		}
	})

	t.Run("at absolute floor uses track titles", func(t *testing.T) {
		// 6 files, 3 interpretable, none matching the requested tracklist.
		files := []string{
			`Kansas\The Absence Of Presence (2020)\01 - The Absence Of Presence.flac`,
			`Kansas\The Absence Of Presence (2020)\02 - Throwing Mountains.flac`,
			`Kansas\The Absence Of Presence (2020)\03 - Jets Overhead.flac`,
			`Kansas\The Absence Of Presence (2020)\04.flac`,
			`Kansas\The Absence Of Presence (2020)\05.flac`,
			`Kansas\The Absence Of Presence (2020)\06.flac`,
		}
		r := CheckRelevance(RelevanceInput{
			ArtistName:  "The Absence",
			AlbumTitle:  "The Absence",
			TrackTitles: theAbsenceTrackTitles,
			Files:       files,
		})
		if r.Source != SourceTrackTitles {
			t.Fatalf("expected SourceTrackTitles (at interpretable floor), got %+v", r)
		}
		if r.Match {
			t.Errorf("expected rejection (no interpretable filename matches the tracklist), got Match=true")
		}
	})

	t.Run("below interpretable ratio falls back to directory", func(t *testing.T) {
		// 10 files, 4 interpretable -> ratio 0.4 < minInterpretableRatio(0.5).
		files := []string{
			`The Absence\The Absence (2016)\01 - Wartorn.flac`,
			`The Absence\The Absence (2016)\02 - Riders Of The Plague.flac`,
			`The Absence\The Absence (2016)\03 - Skin And Bones.flac`,
			`The Absence\The Absence (2016)\04 - The Absence.flac`,
			`The Absence\The Absence (2016)\05.flac`,
			`The Absence\The Absence (2016)\06.flac`,
			`The Absence\The Absence (2016)\07.flac`,
			`The Absence\The Absence (2016)\08.flac`,
			`The Absence\The Absence (2016)\09.flac`,
			`The Absence\The Absence (2016)\10.flac`,
		}
		r := CheckRelevance(RelevanceInput{
			ArtistName:  "The Absence",
			AlbumTitle:  "The Absence",
			TrackTitles: theAbsenceTrackTitles,
			Files:       files,
		})
		if r.Source != SourceDirectory {
			t.Fatalf("expected SourceDirectory (below interpretable ratio), got %+v", r)
		}
	})
}
