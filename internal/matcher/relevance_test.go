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

// relevanceCase is one CheckRelevance scenario. checkSource opts a case into
// asserting Source too - most cases only care about Match.
type relevanceCase struct {
	name        string
	artist      string
	title       string
	trackTitles []string
	files       []string
	wantMatch   bool
	checkSource bool
	wantSource  RelevanceSource
}

func TestCheckRelevance(t *testing.T) {
	cases := []relevanceCase{
		{
			name:        "rejects wrong album via track titles",
			artist:      "The Absence",
			title:       "The Absence",
			trackTitles: theAbsenceTrackTitles,
			files:       kansasFiles,
			wantMatch:   false,
			checkSource: true,
			wantSource:  SourceTrackTitles,
		},
		{
			name:        "rejects wrong album via directory fallback (numeric filenames)",
			artist:      "The Absence",
			title:       "The Absence",
			trackTitles: theAbsenceTrackTitles,
			files: []string{
				`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\01.flac`,
				`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\02.flac`,
				`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\03.flac`,
				`@@abc\Music\Kansas\Kansas - The Absence Of Presence (2020) [FLAC]\04.flac`,
			},
			wantMatch:   false,
			checkSource: true,
			wantSource:  SourceDirectory,
		},
		{
			name:   "rejects on low title recall (wrong album, same artist)",
			artist: "The Absence",
			title:  "The Absence",
			files: []string{
				`The Absence - Riders Of The Plague (2007)\01 - Riders Of The Plague.flac`,
				`The Absence - Riders Of The Plague (2007)\02 - Second Track.flac`,
			},
			wantMatch: false,
		},
		{
			name:        "accepts matching album",
			artist:      "The Absence",
			title:       "The Absence",
			trackTitles: theAbsenceTrackTitles,
			files: []string{
				`The Absence\The Absence (2016) [FLAC]\01 - Wartorn.flac`,
				`The Absence\The Absence (2016) [FLAC]\02 - Riders Of The Plague.flac`,
				`The Absence\The Absence (2016) [FLAC]\03 - Skin And Bones.flac`,
			},
			wantMatch: true,
		},
		{
			// Pins the 0.6 precision constant: directory tokens {the, absence,
			// mtd} - mtd unexplained -> precision 2/3 = 0.67, which must pass (a
			// stricter 0.75 would wrongly reject this common, legitimate naming
			// pattern).
			name:   "accepts scene release precision (0.67 >= 0.6)",
			artist: "The Absence",
			title:  "The Absence",
			files: []string{
				`The Absence-The Absence-2016-MTD\101-the_absence-wartorn.mp3`,
				`The Absence-The Absence-2016-MTD\102-the_absence-riders_of_the_plague.mp3`,
			},
			wantMatch: true,
		},
		{
			// Without catalogue-number/codec/year noise classification, this
			// directory's unexplained tokens (2016, flac, mb3984, 15107) would
			// sink precision to 2/6 = 0.33; with it, only {the, absence} remain
			// and precision is 1.0.
			name:   "accepts catalogue number noise",
			artist: "The Absence",
			title:  "The Absence",
			files: []string{
				`The Absence - The Absence (2016) [FLAC] {MB3984-15107}\01 - Wartorn.flac`,
				`The Absence - The Absence (2016) [FLAC] {MB3984-15107}\02 - Riders Of The Plague.flac`,
			},
			wantMatch: true,
		},
		{
			name:   "accepts edition marketing suffix on directory",
			artist: "Some Artist",
			title:  "Album X",
			files: []string{
				`Album X (2011 Remaster) [Deluxe Edition]\01 - Track One.flac`,
				`Album X (2011 Remaster) [Deluxe Edition]\02 - Track Two.flac`,
			},
			wantMatch: true,
		},
		{
			name:   "accepts plain directory against an edition-suffixed title",
			artist: "Some Artist",
			title:  "Album X (Deluxe Edition)",
			files: []string{
				`Album X\01 - Track One.flac`,
				`Album X\02 - Track Two.flac`,
			},
			wantMatch: true,
		},
		{
			name:   "multi-disc falls back to parent segment",
			artist: "The Absence",
			title:  "The Absence",
			files: []string{
				`The Absence (2016)\CD1\01 - Wartorn.flac`,
				`The Absence (2016)\CD1\02 - Riders Of The Plague.flac`,
			},
			wantMatch:   true,
			checkSource: true,
			wantSource:  SourceDirectory,
		},
		{
			name:   "diacritic folding",
			artist: "Motörhead",
			title:  "Motörhead",
			files: []string{
				`Motorhead\Motorhead (1977)\01 - Motorhead.flac`,
			},
			wantMatch: true,
		},
		{
			name:        "empty track titles falls back to directory",
			artist:      "The Absence",
			title:       "The Absence",
			trackTitles: nil,
			files: []string{
				`The Absence\The Absence (2016)\01 - Wartorn.flac`,
			},
			wantMatch:   true,
			checkSource: true,
			wantSource:  SourceDirectory,
		},
		{
			name:      "empty album title is no data",
			artist:    "Some Artist",
			title:     "",
			files:     []string{`anything\goes\here.flac`},
			wantMatch: true, checkSource: true, wantSource: SourceNoData,
		},
		// --- issue #316 follow-up: parent-segment fallback narrowed (finding 1) ---
		{
			// The parent-segment fallback must only fire when the last segment
			// carries no usable tokens at all (the multi-disc case above), never
			// merely because dirCheck on the last segment failed. Before the
			// fix, a self-titled album's ordinary parent-directory layout let
			// the wrong album through: titleTokens == artistTokens for a
			// self-titled album, so the parent "The Absence" would satisfy
			// dirCheck on its own even though the actual release is unrelated.
			name:   "self-titled album: last segment has tokens and fails, parent is not consulted",
			artist: "The Absence",
			title:  "The Absence",
			files: []string{
				`Music\The Absence\Riders Of The Plague (2007)\01.flac`,
				`Music\The Absence\Riders Of The Plague (2007)\02.flac`,
			},
			wantMatch:   false,
			checkSource: true,
			wantSource:  SourceDirectory,
		},
		// --- track title bracket stripping (finding 2) ---
		{
			// "(feat. Guest A)", "(Live)" and "(Remastered 2011)" are routine
			// Lidarr track-title metadata that a peer's filename legitimately
			// omits. Stripping them from the EXPECTED track title (not the
			// filename, and never the directory) is what lets these match.
			name:   "track titles with feat/live/remaster suffixes match plain filenames",
			artist: "Some Artist",
			title:  "Some Album",
			trackTitles: []string{
				"Song One (feat. Guest A)",
				"Song Two (Live)",
				"Song Three (Remastered 2011)",
				"Song Four",
			},
			files: []string{
				`New folder\01 - Song One.flac`,
				`New folder\02 - Song Two.flac`,
				`New folder\03 - Song Three.flac`,
				`New folder\04 - Song Four.flac`,
			},
			wantMatch:   true,
			checkSource: true,
			wantSource:  SourceTrackTitles,
		},
		{
			// Pins the asymmetry: a directory's parenthesized content must
			// still count against it, even after track titles started
			// stripping their own. No track titles here, so this exercises
			// dirCheck directly.
			name:   "directory with (Of Presence) is still rejected despite track-title stripping",
			artist: "The Absence",
			title:  "The Absence",
			files: []string{
				`The Absence (Of Presence)\01.flac`,
				`The Absence (Of Presence)\02.flac`,
			},
			wantMatch: false,
		},
		// --- apostrophe elision (finding 3) ---
		{
			name:   "apostrophe: peer folder dropping the apostrophe in Don't Cry still matches",
			artist: "Someone",
			title:  "Don't Cry",
			files: []string{
				`Someone - Dont Cry (1991)\01 - Dont Cry.flac`,
				`Someone - Dont Cry (1991)\02 - Another Track.flac`,
			},
			wantMatch: true,
		},
		{
			name:   "apostrophe: Pepper's title matches a Peppers folder",
			artist: "Some Artist",
			title:  "Pepper's Album",
			files: []string{
				`Peppers Album\01 - Track One.flac`,
				`Peppers Album\02 - Track Two.flac`,
			},
			wantMatch: true,
		},
		// --- noise applied to the requested title too (finding 4) ---
		{
			// A purely-numeric album title tokenizes to nothing (yearRe), so
			// titleTokens is empty and the gate fails open via SourceNoData -
			// it accepts a totally unrelated candidate. This is documented,
			// fail-open behaviour, not a regression; pinned here so it stays
			// visible rather than a surprise the next time someone edits the
			// noise list.
			name:      "purely numeric title fails open via SourceNoData",
			artist:    "Van Halen",
			title:     "1984",
			files:     []string{`Some Totally Unrelated Band\Totally Different Album (1990)\01.flac`},
			wantMatch: true, checkSource: true, wantSource: SourceNoData,
		},
		// --- trailing bracket-group directory noise (finding 7) ---
		{
			// Before: directory tokens {kansas, leftoverture, epic, records} -
			// {epic, records} unexplained -> precision 2/4 = 0.50, rejected.
			// After: the trailing "[Epic Records]" label group is stripped
			// before tokenizing -> precision 2/2 = 1.0.
			name:   "trailing square-bracket label group is directory noise",
			artist: "Kansas",
			title:  "Leftoverture",
			files: []string{
				`Kansas - Leftoverture (1976) [Epic Records]\01 - Carry On Wayward Son.flac`,
				`Kansas - Leftoverture (1976) [Epic Records]\02 - The Wall.flac`,
			},
			wantMatch: true,
		},
		// --- decisive in both directions (finding 9) ---
		{
			name:        "track titles accept a well-matching candidate in a badly-named folder",
			artist:      "The Absence",
			title:       "The Absence",
			trackTitles: theAbsenceTrackTitles,
			files: []string{
				`New folder (2)\01 - Wartorn.flac`,
				`New folder (2)\02 - Riders Of The Plague.flac`,
				`New folder (2)\03 - Skin And Bones.flac`,
			},
			wantMatch:   true,
			checkSource: true,
			wantSource:  SourceTrackTitles,
		},
		{
			name:        "track titles reject a wrong candidate in a plausible-looking folder",
			artist:      "The Absence",
			title:       "The Absence",
			trackTitles: theAbsenceTrackTitles,
			files: []string{
				`The Absence - The Absence (2016)\01 - Totally Different Song.flac`,
				`The Absence - The Absence (2016)\02 - Another Wrong Title.flac`,
				`The Absence - The Absence (2016)\03 - Yet More Random Words.flac`,
			},
			wantMatch:   false,
			checkSource: true,
			wantSource:  SourceTrackTitles,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := CheckRelevance(RelevanceInput{
				ArtistName:  tc.artist,
				AlbumTitle:  tc.title,
				TrackTitles: tc.trackTitles,
				Files:       tc.files,
			})
			if r.Match != tc.wantMatch {
				t.Fatalf("Match = %v, want %v (%+v)", r.Match, tc.wantMatch, r)
			}
			if tc.checkSource && r.Source != tc.wantSource {
				t.Errorf("Source = %v, want %v (%+v)", r.Source, tc.wantSource, r)
			}
		})
	}
}

// TestCheckRelevanceInterpretableFloor pins the minInterpretableFiles(3) and
// minInterpretableRatio(0.5) constants with a named-threshold test, per issue
// #316's explicit requirement. Kept as t.Run subtests rather than folded into
// the table above: each subtest exercises a distinct threshold boundary and
// asserts a different field (Source, sometimes also Match), not just a single
// Match/Source pair.
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
