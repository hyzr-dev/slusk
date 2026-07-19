package pipeline

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/dhowden/tag"
)

// Soulseek users' shared folders are not guaranteed to be tidy: the same track
// can appear twice in one album folder (e.g. both a FLAC and an MP3 copy, or a
// stray duplicate). Lidarr's manual import treats such a folder as ambiguous,
// so before the import scan the folder is deduplicated down to one file per
// track, keyed on embedded tags: track number when present, normalized title
// as fallback. Per track, lossless always beats lossy; within the same format
// class the larger file (a bitrate proxy — tag headers don't carry bitrate)
// wins.

// audioExts are the file extensions considered audio files; anything else in
// the folder (covers, cue sheets, logs) is ignored entirely.
var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true, ".opus": true,
	".wav": true, ".ape": true, ".wma": true, ".aac": true, ".aiff": true,
}

// losslessExts classifies by extension; readFileMeta upgrades m4a to lossless
// when the tag header identifies ALAC.
var losslessExts = map[string]bool{".flac": true, ".wav": true, ".ape": true, ".aiff": true}

// dedupFile is one audio file's identity for dedup grouping.
type dedupFile struct {
	path     string
	size     int64
	disc     int
	track    int
	titleKey string // normalizeTitle of the tag title; "" when untagged
	lossless bool
}

// readFileMeta parses one audio file's embedded tags into a dedupFile. A file
// whose tags can't be read still participates with whatever the extension
// tells us (it just can't be grouped, so it is never removed). Package-level
// var so tests can stub tag parsing instead of crafting real tagged audio
// fixtures.
var readFileMeta = func(path string, size int64) dedupFile {
	df := dedupFile{path: path, size: size, lossless: losslessExts[strings.ToLower(filepath.Ext(path))]}
	f, err := os.Open(path)
	if err != nil {
		return df
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return df
	}
	df.track, _ = m.Track()
	df.disc, _ = m.Disc()
	df.titleKey = normalizeTitle(m.Title())
	if ft := m.FileType(); ft == tag.FLAC || ft == tag.ALAC {
		df.lossless = true
	}
	return df
}

// dedupAlbumFolder removes duplicate track files from one album folder and
// returns the removed paths. Only the folder's direct entries are considered
// (slskd recreates a flat leaf folder per download). A remove failure is
// logged and skipped — a leftover duplicate degrades the import at worst,
// while blocking verify would strand the job.
func dedupAlbumFolder(log *slog.Logger, folder string) (removed []string, err error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	var files []dedupFile
	for _, e := range entries {
		if e.IsDir() || !audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, readFileMeta(filepath.Join(folder, e.Name()), info.Size()))
	}
	for _, loser := range dedupFiles(files) {
		if err := os.Remove(loser.path); err != nil {
			log.Warn("remove duplicate track file failed", "file", loser.path, "err", err)
			continue
		}
		removed = append(removed, loser.path)
	}
	return removed, nil
}

// dedupFiles groups files into same-track sets and returns the losers (every
// file except each group's winner). Grouping is track-number-first: files
// with a positive track number group on (disc, track); files without one join
// a numbered group whose title matches, or failing that group with other
// unnumbered files by title. A file with neither track number nor title can't
// be identified as a duplicate of anything and is never removed.
func dedupFiles(files []dedupFile) (losers []dedupFile) {
	var groups [][]dedupFile
	byNum := make(map[string]int)   // "disc/track" → groups index
	byTitle := make(map[string]int) // titleKey → groups index
	for _, f := range files {
		if f.track <= 0 {
			continue
		}
		k := fmt.Sprintf("%d/%d", f.disc, f.track)
		idx, ok := byNum[k]
		if !ok {
			groups = append(groups, nil)
			idx = len(groups) - 1
			byNum[k] = idx
		}
		groups[idx] = append(groups[idx], f)
		if f.titleKey != "" {
			if _, taken := byTitle[f.titleKey]; !taken {
				byTitle[f.titleKey] = idx
			}
		}
	}
	for _, f := range files {
		if f.track > 0 || f.titleKey == "" {
			continue
		}
		idx, ok := byTitle[f.titleKey]
		if !ok {
			groups = append(groups, nil)
			idx = len(groups) - 1
			byTitle[f.titleKey] = idx
		}
		groups[idx] = append(groups[idx], f)
	}
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		w := winner(g)
		for _, f := range g {
			if f.path != w.path {
				losers = append(losers, f)
			}
		}
	}
	return losers
}

// winner picks the file to keep from one same-track group: lossless beats
// lossy, then larger size (bitrate proxy), then lexicographically smallest
// path for determinism.
func winner(g []dedupFile) dedupFile {
	best := g[0]
	for _, f := range g[1:] {
		switch {
		case f.lossless != best.lossless:
			if f.lossless {
				best = f
			}
		case f.size != best.size:
			if f.size > best.size {
				best = f
			}
		case f.path < best.path:
			best = f
		}
	}
	return best
}

// featSuffix strips "(feat. X)" / "ft. X" style suffixes before comparing
// titles, so a tagged "Song (feat. X)" and a bare "Song" copy still match.
// The leading [\s([] requires a separator before feat/ft, so a title like
// "Shift work" (containing "ft" mid-word) is left alone.
var featSuffix = regexp.MustCompile(`(?i)[\s([](feat|ft|featuring)\.?\s.*$`)

// normalizeTitle reduces a tag title to a comparison key: feat-suffix
// stripped, lowercased, letters and digits only.
func normalizeTitle(s string) string {
	s = featSuffix.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
