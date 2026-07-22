package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	"github.com/creachadair/tomledit/scanner"
	"github.com/creachadair/tomledit/transform"
)

// Settings is the subset of configuration the writable settings view (see
// internal/observ/config.go) may change. A nil LidarrAPIKey means "keep the
// currently configured value" — the settings view never sends the secret
// back to the client, so it has no way to resubmit it unchanged.
type Settings struct {
	LidarrURL          string
	LidarrAPIKey       *string
	WantedSyncInterval time.Duration
	StallTimeout       time.Duration
	MaxActive          int
	MinBitrate         int
}

// ErrNotWritable is returned by ApplySettings when the config file's
// directory does not accept the rename step of an atomic write — typical of
// a bind-mounted single file or a read-only mount. Atomic writes require
// creating a temp file and renaming it into place within the same directory,
// so the fix is to mount the directory writable rather than the file itself.
var ErrNotWritable = errors.New("config file is not writable; mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount")

// ApplySettings updates only the TOML keys whose current on-disk value
// differs from s, preserving comments, formatting, and any other keys a user
// may have hand-edited since startup. It re-reads path from disk first, so a
// concurrent hand edit to an unrelated key is never clobbered.
func ApplySettings(path string, s Settings) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	doc, err := tomledit.Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	if err := setStringIfChanged(doc, "lidarr", "url", s.LidarrURL); err != nil {
		return err
	}
	if s.LidarrAPIKey != nil {
		if err := setStringIfChanged(doc, "lidarr", "api_key", *s.LidarrAPIKey); err != nil {
			return err
		}
	}
	if err := setDurationIfChanged(doc, "pipeline", "wanted_sync_interval", s.WantedSyncInterval); err != nil {
		return err
	}
	if err := setDurationIfChanged(doc, "pipeline", "stall_timeout", s.StallTimeout); err != nil {
		return err
	}
	if err := setIntIfChanged(doc, "pipeline", "max_active", s.MaxActive); err != nil {
		return err
	}
	if err := setIntIfChanged(doc, "pipeline", "min_bitrate", s.MinBitrate); err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := tomledit.Format(&buf, doc); err != nil {
		return fmt.Errorf("format config: %w", err)
	}
	rendered := buf.Bytes()

	// Backstop: the rendered document must decode and validate cleanly
	// (identical rules to Load) before anything touches disk.
	if _, err := LoadBytes(rendered); err != nil {
		return fmt.Errorf("rendered config failed validation, nothing was written: %w", err)
	}

	return atomicWrite(path, rendered)
}

// ProbeWritable reports whether the directory containing path currently
// accepts writes, by creating and immediately removing a temp file in it —
// the same technique cmd/slskdarr's ensureWritableDir uses for the download
// directory. Used at startup to surface AppConfig.Writable to the settings
// view without performing (and discarding) a real config write.
func ProbeWritable(path string) bool {
	f, err := os.CreateTemp(filepath.Dir(path), ".slskdarr-config-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name) == nil
}

// setStringIfChanged upserts table.leaf with value only if its current
// decoded content differs, so an unmodified value's line stays byte-
// identical (including any quote style already on disk).
func setStringIfChanged(doc *tomledit.Document, table, leaf, value string) error {
	if cur, ok := stringValue(doc, table, leaf); ok && cur == value {
		return nil
	}
	return upsert(doc, table, leaf, quoteTOMLString(value))
}

// setDurationIfChanged upserts table.leaf with duration.String() only if the
// current value doesn't already parse to the same duration — so e.g. a file
// already spelling 5 minutes as "5m" is left untouched rather than rewritten
// to durations's canonical "5m0s" form.
func setDurationIfChanged(doc *tomledit.Document, table, leaf string, duration time.Duration) error {
	if cur, ok := durationValue(doc, table, leaf); ok && cur == duration {
		return nil
	}
	return upsert(doc, table, leaf, quoteTOMLString(duration.String()))
}

func setIntIfChanged(doc *tomledit.Document, table, leaf string, value int) error {
	if cur, ok := intValue(doc, table, leaf); ok && cur == value {
		return nil
	}
	return upsert(doc, table, leaf, strconv.Itoa(value))
}

// quoteTOMLString renders s as a TOML basic (double-quoted) string, matching
// the quoting style used throughout this project's config files.
func quoteTOMLString(s string) string {
	return `"` + string(scanner.Escape(s)) + `"`
}

// upsert sets table.leaf to the already-rendered TOML value text, creating
// the key if absent and preserving any existing block/trailing comment on it
// rather than dropping it.
func upsert(doc *tomledit.Document, table, leaf, renderedValue string) error {
	tab := transform.FindTable(doc, table)
	if tab == nil {
		return fmt.Errorf("config is missing required [%s] table", table)
	}
	val, err := parser.ParseValue(renderedValue)
	if err != nil {
		return fmt.Errorf("build value for %s.%s: %w", table, leaf, err)
	}
	kv := &parser.KeyValue{Name: parser.Key{leaf}, Value: val}
	if existing := doc.First(table, leaf); existing != nil && existing.KeyValue != nil {
		kv.Block = existing.Block
		kv.Value.Trailer = existing.Value.Trailer
	}
	transform.InsertMapping(tab.Section, kv, true)
	return nil
}

// entryToken returns the scalar token backing table.leaf, if the key exists
// and its value is a scalar (not an array or inline table).
func entryToken(doc *tomledit.Document, table, leaf string) (parser.Token, bool) {
	entry := doc.First(table, leaf)
	if entry == nil || entry.KeyValue == nil {
		return parser.Token{}, false
	}
	tok, ok := entry.Value.X.(parser.Token)
	return tok, ok
}

// stringValue decodes table.leaf's current value as a string, supporting the
// two single-line TOML string forms this project's config files use: basic
// ("...", with escapes) and literal ('...', without). Any other token type
// reports not-ok, so the caller treats the key as "no current value" and
// simply writes the requested one.
func stringValue(doc *tomledit.Document, table, leaf string) (string, bool) {
	tok, ok := entryToken(doc, table, leaf)
	if !ok {
		return "", false
	}
	text := tok.String()
	switch tok.Type {
	case scanner.String:
		if len(text) < 2 {
			return "", false
		}
		unescaped, err := scanner.Unescape([]byte(text[1 : len(text)-1]))
		if err != nil {
			return "", false
		}
		return string(unescaped), true
	case scanner.LString:
		if len(text) < 2 {
			return "", false
		}
		return text[1 : len(text)-1], true
	default:
		return "", false
	}
}

func durationValue(doc *tomledit.Document, table, leaf string) (time.Duration, bool) {
	s, ok := stringValue(doc, table, leaf)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

func intValue(doc *tomledit.Document, table, leaf string) (int, bool) {
	tok, ok := entryToken(doc, table, leaf)
	if !ok || tok.Type != scanner.Integer {
		return 0, false
	}
	// TOML integers may use "_" as a digit separator (e.g. 1_000).
	n, err := strconv.Atoi(strings.ReplaceAll(tok.String(), "_", ""))
	if err != nil {
		return 0, false
	}
	return n, true
}

// atomicWrite writes data to path atomically: a temp file in the same
// directory is written, fsynced, and chmod'd to path's existing mode, then
// renamed into place, followed by an fsync of the directory so the rename
// itself is durable. A failure that indicates the destination cannot accept
// this scheme — a read-only directory (seen at temp-file creation) or a
// rename rejected with EBUSY/EXDEV/EROFS (typical of a bind-mounted single
// file) — is reported as ErrNotWritable, and any temp file is removed so
// nothing is left behind.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".slskdarr-config-*.tmp")
	if err != nil {
		if isNotWritableErr(err) {
			return ErrNotWritable
		}
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		if isNotWritableErr(err) {
			return ErrNotWritable
		}
		return fmt.Errorf("rename temp config into place: %w", err)
	}
	cleanup = false

	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open config dir for sync: %w", err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("sync config dir: %w", err)
	}
	return nil
}

// isNotWritableErr reports whether err indicates the destination cannot
// accept an atomic write: permission denied (e.g. a chmod'd read-only
// directory), a read-only filesystem, a cross-device rename (single-file
// bind mounts commonly appear on a different device than their parent), or a
// device/resource busy rename (a bind-mounted file cannot be replaced by
// rename).
func isNotWritableErr(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EROFS) ||
		errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EBUSY)
}
