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
// internal/observ/config.go) may change: every field in Config except the
// unconditionally-derived Soulseek.Enabled. It mirrors Config's section tree
// so ApplySettings can map it onto the TOML document one table at a time. A
// nil secret pointer (LidarrSettings.APIKey and friends) means "keep the
// currently configured value" — the settings view never sends a configured
// secret back to the client, so it has no way to resubmit it unchanged.
type Settings struct {
	Lidarr   LidarrSettings
	Slskd    SlskdSettings
	Pipeline PipelineSettings
	Soulseek SoulseekSettings
	Store    StoreSettings
	Observ   ObservSettings
	Paths    PathsSettings
}

// LidarrSettings is the writable subset of LidarrConfig.
type LidarrSettings struct {
	URL    string
	APIKey *string
}

// SlskdSettings is the writable subset of SlskdConfig.
type SlskdSettings struct {
	URL    string
	APIKey *string
}

// WeightsSettings is the writable subset of Weights.
type WeightsSettings struct {
	Format      float64
	Bitrate     float64
	Reliability float64
	FileCount   float64
	KnownUser   float64
}

// PipelineSettings is the writable subset of PipelineConfig: every field.
type PipelineSettings struct {
	Backend               string
	MaxCandidatesPerAlbum int
	MaxActive             int
	MaxRetries            int
	MaxInflightPerPeer    int
	MaxTransferRetries    int
	MinBitrate            int
	TransferDeadline      time.Duration
	StallTimeout          time.Duration
	SearchTimeout         time.Duration
	BackoffBase           time.Duration
	BackoffCap            time.Duration
	CandidateTTL          time.Duration
	FailedReviveAfter     time.Duration
	StuckAfter            time.Duration
	TickTimeout           time.Duration
	ImportConfirmTimeout  time.Duration
	WantedSyncInterval    time.Duration
	DiscoveryInterval     time.Duration
	SelectingInterval     time.Duration
	DownloadingInterval   time.Duration
	ImportingInterval     time.Duration
	ManualImportTimeout   time.Duration
	ImportRetryCooldown   time.Duration
	Weights               WeightsSettings
}

// GluetunSettings is the writable subset of GluetunConfig.
type GluetunSettings struct {
	ControlURL string
	APIKey     *string
}

// SoulseekSettings is the writable subset of SoulseekConfig. Enabled is
// deliberately absent: it is derived (see SoulseekConfig.Enabled) from the
// other fields, not an independent setting.
type SoulseekSettings struct {
	ServerAddress string
	Username      string
	Password      *string
	ListenAddr    string
	UploadSlots   int
	Gluetun       GluetunSettings
	// SharedFolders replaces the on-disk list wholesale when it differs from
	// the currently parsed one (see applySharedFolders); it is not merged
	// element-by-element.
	SharedFolders []SharedFolderConfig
}

// StoreSettings is the writable subset of StoreConfig.
type StoreSettings struct {
	DSN *string
}

// ObservSettings is the writable subset of ObservConfig.
type ObservSettings struct {
	ListenAddr string
	AuthToken  *string
	LogLevel   string
}

// PathsSettings is the writable subset of PathsConfig.
type PathsSettings struct {
	SlskdCompleteDir string
}

// ErrNotWritable is returned by ApplySettings when the config file's
// directory does not accept the rename step of an atomic write — typical of
// a bind-mounted single file or a read-only mount. Atomic writes require
// creating a temp file and renaming it into place within the same directory,
// so the fix is to mount the directory writable rather than the file itself.
var ErrNotWritable = errors.New("config file is not writable; mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount")

// ErrValidationFailed is returned by ApplySettings when the rendered document
// — built from the current file plus the requested changes — would fail
// config.Validate (typically a cross-field rule the settings view's per-field
// validation does not duplicate, e.g. pipeline.backend = "soulseek" requiring
// a configured [soulseek] section). Nothing is written to disk in this case.
// The wrapped error is LoadBytes' own message, which never embeds paths or
// secrets and is safe to surface to the settings view.
var ErrValidationFailed = errors.New("rendered config failed validation, nothing was written")

// ApplySettings updates only the TOML keys whose current on-disk value
// differs from s, preserving comments, formatting, and any other keys a user
// may have hand-edited since startup. It re-reads path from disk first, so a
// concurrent hand edit to an unrelated key is never clobbered. Tables absent
// from the current file (e.g. a wholly unconfigured [soulseek] section) are
// created as needed. Immediately before writing, the pre-change bytes are
// best-effort saved to path+".bak" as a manual recovery point; a failure to
// do so does not block the save.
func ApplySettings(path string, s Settings) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	doc, err := tomledit.Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	// Captured before any mutation below, so an unchanged shared-folders list
	// leaves those sections byte-identical rather than being rewritten to an
	// equivalent-but-differently-formatted form.
	currentFolders := currentSharedFolders(doc)

	for _, fs := range settingsFields(s) {
		if err := fs.apply(doc); err != nil {
			return err
		}
	}
	if sharedFoldersChanged(currentFolders, s.Soulseek.SharedFolders) {
		if err := applySharedFolders(doc, s.Soulseek.SharedFolders); err != nil {
			return err
		}
	}

	var buf bytes.Buffer
	if err := tomledit.Format(&buf, doc); err != nil {
		return fmt.Errorf("format config: %w", err)
	}
	rendered := buf.Bytes()

	// Backstop: the rendered document must decode and validate cleanly
	// (identical rules to Load) before anything touches disk.
	if _, err := LoadBytes(rendered); err != nil {
		return fmt.Errorf("%w: %v", ErrValidationFailed, err)
	}

	// Best-effort manual recovery point: if this fails (e.g. the directory
	// somehow accepts the temp-file dance below but not an extra file), the
	// save proceeds anyway — the strict loader never reads *.bak, so its
	// absence cannot break startup, only a manual recovery.
	if info, statErr := os.Stat(path); statErr == nil {
		_ = os.WriteFile(path+".bak", data, info.Mode())
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

// fieldKind selects how a fieldSpec's desired value is rendered and compared
// against the document's current value.
type fieldKind int

const (
	fieldString fieldKind = iota
	fieldInt
	fieldFloat
	fieldDuration
	fieldSecret
)

// fieldSpec is one declarative entry in the Settings→TOML mapping: which
// table.key to touch, what kind of value it holds, and the desired value
// itself. settingsFields builds the full list once per call so ApplySettings
// can apply it with a single generic loop instead of one hand-written
// setXIfChanged call per field.
type fieldSpec struct {
	table  []string
	key    string
	kind   fieldKind
	str    string        // fieldString
	i      int           // fieldInt
	f      float64       // fieldFloat
	d      time.Duration // fieldDuration
	secret *string       // fieldSecret; nil means keep the current value
}

// apply upserts the field's value into doc according to its kind, or does
// nothing when a fieldSecret's value is nil (keep).
func (fs fieldSpec) apply(doc *tomledit.Document) error {
	switch fs.kind {
	case fieldString:
		return applyStringField(doc, fs.table, fs.key, fs.str)
	case fieldInt:
		return applyIntField(doc, fs.table, fs.key, fs.i)
	case fieldFloat:
		return applyFloatField(doc, fs.table, fs.key, fs.f)
	case fieldDuration:
		return applyDurationField(doc, fs.table, fs.key, fs.d)
	case fieldSecret:
		if fs.secret == nil {
			return nil
		}
		return applyStringField(doc, fs.table, fs.key, *fs.secret)
	default:
		return fmt.Errorf("unknown field kind for %s.%s", strings.Join(fs.table, "."), fs.key)
	}
}

// settingsFields is the declarative Settings→TOML mapping: every writable
// key except soulseek.shared_folders, which is an array of tables and is
// handled separately (see applySharedFolders).
func settingsFields(s Settings) []fieldSpec {
	lidarr := []string{"lidarr"}
	slskd := []string{"slskd"}
	pipeline := []string{"pipeline"}
	weights := []string{"pipeline", "weights"}
	soulseek := []string{"soulseek"}
	gluetun := []string{"soulseek", "gluetun"}
	store := []string{"store"}
	observ := []string{"observ"}
	paths := []string{"paths"}

	return []fieldSpec{
		{table: lidarr, key: "url", kind: fieldString, str: s.Lidarr.URL},
		{table: lidarr, key: "api_key", kind: fieldSecret, secret: s.Lidarr.APIKey},

		{table: slskd, key: "url", kind: fieldString, str: s.Slskd.URL},
		{table: slskd, key: "api_key", kind: fieldSecret, secret: s.Slskd.APIKey},

		{table: pipeline, key: "backend", kind: fieldString, str: s.Pipeline.Backend},
		{table: pipeline, key: "max_candidates_per_album", kind: fieldInt, i: s.Pipeline.MaxCandidatesPerAlbum},
		{table: pipeline, key: "max_active", kind: fieldInt, i: s.Pipeline.MaxActive},
		{table: pipeline, key: "max_retries", kind: fieldInt, i: s.Pipeline.MaxRetries},
		{table: pipeline, key: "max_inflight_per_peer", kind: fieldInt, i: s.Pipeline.MaxInflightPerPeer},
		{table: pipeline, key: "max_transfer_retries", kind: fieldInt, i: s.Pipeline.MaxTransferRetries},
		{table: pipeline, key: "min_bitrate", kind: fieldInt, i: s.Pipeline.MinBitrate},
		{table: pipeline, key: "transfer_deadline", kind: fieldDuration, d: s.Pipeline.TransferDeadline},
		{table: pipeline, key: "stall_timeout", kind: fieldDuration, d: s.Pipeline.StallTimeout},
		{table: pipeline, key: "search_timeout", kind: fieldDuration, d: s.Pipeline.SearchTimeout},
		{table: pipeline, key: "backoff_base", kind: fieldDuration, d: s.Pipeline.BackoffBase},
		{table: pipeline, key: "backoff_cap", kind: fieldDuration, d: s.Pipeline.BackoffCap},
		{table: pipeline, key: "candidate_ttl", kind: fieldDuration, d: s.Pipeline.CandidateTTL},
		{table: pipeline, key: "failed_revive_after", kind: fieldDuration, d: s.Pipeline.FailedReviveAfter},
		{table: pipeline, key: "stuck_after", kind: fieldDuration, d: s.Pipeline.StuckAfter},
		{table: pipeline, key: "tick_timeout", kind: fieldDuration, d: s.Pipeline.TickTimeout},
		{table: pipeline, key: "import_confirm_timeout", kind: fieldDuration, d: s.Pipeline.ImportConfirmTimeout},
		{table: pipeline, key: "wanted_sync_interval", kind: fieldDuration, d: s.Pipeline.WantedSyncInterval},
		{table: pipeline, key: "discovery_interval", kind: fieldDuration, d: s.Pipeline.DiscoveryInterval},
		{table: pipeline, key: "selecting_interval", kind: fieldDuration, d: s.Pipeline.SelectingInterval},
		{table: pipeline, key: "downloading_interval", kind: fieldDuration, d: s.Pipeline.DownloadingInterval},
		{table: pipeline, key: "importing_interval", kind: fieldDuration, d: s.Pipeline.ImportingInterval},
		{table: pipeline, key: "manual_import_timeout", kind: fieldDuration, d: s.Pipeline.ManualImportTimeout},
		{table: pipeline, key: "import_retry_cooldown", kind: fieldDuration, d: s.Pipeline.ImportRetryCooldown},

		{table: weights, key: "format", kind: fieldFloat, f: s.Pipeline.Weights.Format},
		{table: weights, key: "bitrate", kind: fieldFloat, f: s.Pipeline.Weights.Bitrate},
		{table: weights, key: "reliability", kind: fieldFloat, f: s.Pipeline.Weights.Reliability},
		{table: weights, key: "file_count", kind: fieldFloat, f: s.Pipeline.Weights.FileCount},
		{table: weights, key: "known_user", kind: fieldFloat, f: s.Pipeline.Weights.KnownUser},

		{table: soulseek, key: "server_address", kind: fieldString, str: s.Soulseek.ServerAddress},
		{table: soulseek, key: "username", kind: fieldString, str: s.Soulseek.Username},
		{table: soulseek, key: "password", kind: fieldSecret, secret: s.Soulseek.Password},
		{table: soulseek, key: "listen_addr", kind: fieldString, str: s.Soulseek.ListenAddr},
		{table: soulseek, key: "upload_slots", kind: fieldInt, i: s.Soulseek.UploadSlots},

		{table: gluetun, key: "control_url", kind: fieldString, str: s.Soulseek.Gluetun.ControlURL},
		{table: gluetun, key: "api_key", kind: fieldSecret, secret: s.Soulseek.Gluetun.APIKey},

		{table: store, key: "dsn", kind: fieldSecret, secret: s.Store.DSN},

		{table: observ, key: "listen_addr", kind: fieldString, str: s.Observ.ListenAddr},
		{table: observ, key: "auth_token", kind: fieldSecret, secret: s.Observ.AuthToken},
		{table: observ, key: "log_level", kind: fieldString, str: s.Observ.LogLevel},

		{table: paths, key: "slskd_complete_dir", kind: fieldString, str: s.Paths.SlskdCompleteDir},
	}
}

// applyStringField upserts table.key with value only if its current decoded
// content differs, so an unmodified value's line stays byte-identical
// (including any quote style already on disk). It skips entirely — writing
// nothing and creating no table — when the key is currently absent and value
// is blank, so an untouched, never-configured field (e.g. every Soulseek
// scalar before that section is first enabled) never forces its table into
// existence with an empty string.
func applyStringField(doc *tomledit.Document, table []string, key, value string) error {
	cur, ok := stringValue(doc, table, key)
	if !ok && value == "" {
		return nil
	}
	if ok && cur == value {
		return nil
	}
	return upsert(doc, table, key, quoteTOMLString(value))
}

// applyIntField is applyStringField's counterpart for integers; see its
// comment for the absent-and-zero skip rule.
func applyIntField(doc *tomledit.Document, table []string, key string, value int) error {
	cur, ok := intValue(doc, table, key)
	if !ok && value == 0 {
		return nil
	}
	if ok && cur == value {
		return nil
	}
	return upsert(doc, table, key, strconv.Itoa(value))
}

// applyFloatField is applyStringField's counterpart for the matcher weights;
// see its comment for the absent-and-zero skip rule.
func applyFloatField(doc *tomledit.Document, table []string, key string, value float64) error {
	cur, ok := floatValue(doc, table, key)
	if !ok && value == 0 {
		return nil
	}
	if ok && cur == value {
		return nil
	}
	return upsert(doc, table, key, formatTOMLFloat(value))
}

// applyDurationField upserts table.key with duration.String() only if the
// current value doesn't already parse to the same duration — so e.g. a file
// already spelling 5 minutes as "5m" is left untouched rather than rewritten
// to duration's canonical "5m0s" form. See applyStringField's comment for the
// absent-and-zero skip rule (every duration field here is unconditionally
// required to be positive by Validate, so it is never legitimately zero in a
// config that loaded successfully — this only guards a hypothetical zero).
func applyDurationField(doc *tomledit.Document, table []string, key string, value time.Duration) error {
	cur, ok := durationValue(doc, table, key)
	if !ok && value == 0 {
		return nil
	}
	if ok && cur == value {
		return nil
	}
	return upsert(doc, table, key, quoteTOMLString(value.String()))
}

// formatTOMLFloat renders v so it always parses back as a TOML float (which
// requires a decimal point), matching this project's config style (e.g.
// "format = 1.0" rather than "format = 1").
func formatTOMLFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// quoteTOMLString renders s as a TOML basic (double-quoted) string, matching
// the quoting style used throughout this project's config files.
func quoteTOMLString(s string) string {
	return `"` + string(scanner.Escape(s)) + `"`
}

// ensureTable returns the section for the dotted table path, creating and
// appending it to the document if it does not already exist. TOML allows a
// dotted table header (e.g. [soulseek.gluetun]) even when its parent table
// was never explicitly declared, so no parent section needs to exist first.
func ensureTable(doc *tomledit.Document, table []string) *tomledit.Section {
	if found := transform.FindTable(doc, table...); found != nil {
		return found.Section
	}
	name := make(parser.Key, len(table))
	copy(name, table)
	sec := &tomledit.Section{Heading: &parser.Heading{Name: name}}
	doc.Sections = append(doc.Sections, sec)
	return sec
}

// upsert sets table.key to the already-rendered TOML value text, creating
// the table and/or key if absent, and preserving any existing block/trailing
// comment on the key rather than dropping it.
func upsert(doc *tomledit.Document, table []string, key, renderedValue string) error {
	tab := ensureTable(doc, table)
	val, err := parser.ParseValue(renderedValue)
	if err != nil {
		return fmt.Errorf("build value for %s.%s: %w", strings.Join(table, "."), key, err)
	}
	kv := &parser.KeyValue{Name: parser.Key{key}, Value: val}
	if existing := doc.First(append(append([]string{}, table...), key)...); existing != nil && existing.KeyValue != nil {
		kv.Block = existing.Block
		kv.Value.Trailer = existing.Value.Trailer
	}
	transform.InsertMapping(tab, kv, true)
	return nil
}

// entryToken returns the scalar token backing table.key, if the key exists
// and its value is a scalar (not an array or inline table).
func entryToken(doc *tomledit.Document, table []string, key string) (parser.Token, bool) {
	entry := doc.First(append(append([]string{}, table...), key)...)
	if entry == nil || entry.KeyValue == nil {
		return parser.Token{}, false
	}
	tok, ok := entry.Value.X.(parser.Token)
	return tok, ok
}

// decodeStringToken decodes tok as a string, supporting the two single-line
// TOML string forms this project's config files use: basic ("...", with
// escapes) and literal ('...', without). Any other token type reports
// not-ok, so the caller treats the key as "no current value" and simply
// writes the requested one.
func decodeStringToken(tok parser.Token) (string, bool) {
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

// stringValue decodes table.key's current value as a string.
func stringValue(doc *tomledit.Document, table []string, key string) (string, bool) {
	tok, ok := entryToken(doc, table, key)
	if !ok {
		return "", false
	}
	return decodeStringToken(tok)
}

func durationValue(doc *tomledit.Document, table []string, key string) (time.Duration, bool) {
	s, ok := stringValue(doc, table, key)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

func intValue(doc *tomledit.Document, table []string, key string) (int, bool) {
	tok, ok := entryToken(doc, table, key)
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

// floatValue decodes table.key's current value as a float. A plain integer
// literal (e.g. "1") is accepted too, since TOML permits it wherever a float
// is expected on decode — otherwise an unmodified weight written as "1"
// would always look "changed" against a requested 1.0.
func floatValue(doc *tomledit.Document, table []string, key string) (float64, bool) {
	tok, ok := entryToken(doc, table, key)
	if !ok {
		return 0, false
	}
	text := strings.ReplaceAll(tok.String(), "_", "")
	switch tok.Type {
	case scanner.Float:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case scanner.Integer:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, false
		}
		return float64(n), true
	default:
		return 0, false
	}
}

// currentSharedFolders reads the on-disk [[soulseek.shared_folders]]
// sections, in file order, as name+path pairs. It is used only to decide
// whether the requested list actually changed (see applySharedFolders): an
// unchanged list must leave those sections byte-identical rather than being
// dropped and rewritten to an equivalent-but-reformatted version.
func currentSharedFolders(doc *tomledit.Document) []SharedFolderConfig {
	key := parser.Key{"soulseek", "shared_folders"}
	var out []SharedFolderConfig
	for _, sec := range doc.Sections {
		if sec.Heading == nil || !sec.Heading.IsArray || !sec.TableName().Equals(key) {
			continue
		}
		var sf SharedFolderConfig
		for _, item := range sec.Items {
			kv, ok := item.(*parser.KeyValue)
			if !ok {
				continue
			}
			tok, ok := kv.Value.X.(parser.Token)
			if !ok {
				continue
			}
			value, ok := decodeStringToken(tok)
			if !ok {
				continue
			}
			switch {
			case kv.Name.Equals(parser.Key{"name"}):
				sf.Name = value
			case kv.Name.Equals(parser.Key{"path"}):
				sf.Path = value
			}
		}
		out = append(out, sf)
	}
	return out
}

// sharedFoldersChanged reports whether requested differs from current,
// order-sensitively (a reorder counts as a change, same as any other edit).
func sharedFoldersChanged(current, requested []SharedFolderConfig) bool {
	if len(current) != len(requested) {
		return true
	}
	for i := range current {
		if current[i] != requested[i] {
			return true
		}
	}
	return false
}

// applySharedFolders drops every existing [[soulseek.shared_folders]]
// section and appends a fresh one per entry in folders, in order. Comments
// inside the dropped sections are sacrificed — an accepted trade-off, since
// there is no sound way to match "which new entry corresponds to which old
// one" when the list has been reordered, added to, or trimmed.
func applySharedFolders(doc *tomledit.Document, folders []SharedFolderConfig) error {
	key := parser.Key{"soulseek", "shared_folders"}
	var kept []*tomledit.Section
	for _, sec := range doc.Sections {
		if sec.Heading != nil && sec.Heading.IsArray && sec.TableName().Equals(key) {
			continue
		}
		kept = append(kept, sec)
	}
	doc.Sections = kept

	for _, sf := range folders {
		nameVal, err := parser.ParseValue(quoteTOMLString(sf.Name))
		if err != nil {
			return fmt.Errorf("build shared folder name value: %w", err)
		}
		pathVal, err := parser.ParseValue(quoteTOMLString(sf.Path))
		if err != nil {
			return fmt.Errorf("build shared folder path value: %w", err)
		}
		sec := &tomledit.Section{
			Heading: &parser.Heading{Name: append(parser.Key{}, key...), IsArray: true},
			Items: []parser.Item{
				&parser.KeyValue{Name: parser.Key{"name"}, Value: nameVal},
				&parser.KeyValue{Name: parser.Key{"path"}, Value: pathVal},
			},
		}
		doc.Sections = append(doc.Sections, sec)
	}
	return nil
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
