package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// writeFixtureFrom copies testdata/<base> into a fresh temp file, applying any
// string replacements first (so callers can inject comments, extra keys, or
// hand edits while keeping the rest of the fixture's required fields intact).
// It returns the path to the writable copy.
func writeFixtureFrom(t *testing.T, base string, replacements ...[2]string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", base))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	for _, r := range replacements {
		next := strings.Replace(contents, r[0], r[1], 1)
		if next == contents {
			t.Fatalf("replacement %q not found in fixture", r[0])
		}
		contents = next
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFixture is writeFixtureFrom against testdata/valid.toml, the minimal
// fixture: no [soulseek] section and several optional pipeline/weights keys
// left absent (relying on applyDefaults).
func writeFixture(t *testing.T, replacements ...[2]string) string {
	t.Helper()
	return writeFixtureFrom(t, "valid.toml", replacements...)
}

func strPtr(s string) *string { return &s }

// settingsFromConfig mirrors cfg's currently-resolved values into a Settings
// value with every secret pointer left nil ("keep"), modeling a settings-view
// form that was seeded from GET /api/config (which never receives a
// configured secret back) and resubmitted without touching anything.
func settingsFromConfig(cfg Config) Settings {
	return Settings{
		Lidarr: LidarrSettings{URL: cfg.Lidarr.URL},
		Slskd:  SlskdSettings{URL: cfg.Slskd.URL},
		Pipeline: PipelineSettings{
			Backend:               cfg.Pipeline.Backend,
			MaxCandidatesPerAlbum: cfg.Pipeline.MaxCandidatesPerAlbum,
			MaxActive:             cfg.Pipeline.MaxActive,
			MaxRetries:            cfg.Pipeline.MaxRetries,
			MaxInflightPerPeer:    cfg.Pipeline.MaxInflightPerPeer,
			MaxTransferRetries:    cfg.Pipeline.MaxTransferRetries,
			MinBitrate:            cfg.Pipeline.MinBitrate,
			TransferDeadline:      cfg.Pipeline.TransferDeadline.Duration,
			StallTimeout:          cfg.Pipeline.StallTimeout.Duration,
			SearchTimeout:         cfg.Pipeline.SearchTimeout.Duration,
			BackoffBase:           cfg.Pipeline.BackoffBase.Duration,
			BackoffCap:            cfg.Pipeline.BackoffCap.Duration,
			CandidateTTL:          cfg.Pipeline.CandidateTTL.Duration,
			FailedReviveAfter:     cfg.Pipeline.FailedReviveAfter.Duration,
			StuckAfter:            cfg.Pipeline.StuckAfter.Duration,
			TickTimeout:           cfg.Pipeline.TickTimeout.Duration,
			ImportConfirmTimeout:  cfg.Pipeline.ImportConfirmTimeout.Duration,
			WantedSyncInterval:    cfg.Pipeline.WantedSyncInterval.Duration,
			DiscoveryInterval:     cfg.Pipeline.DiscoveryInterval.Duration,
			SelectingInterval:     cfg.Pipeline.SelectingInterval.Duration,
			DownloadingInterval:   cfg.Pipeline.DownloadingInterval.Duration,
			ImportingInterval:     cfg.Pipeline.ImportingInterval.Duration,
			ManualImportTimeout:   cfg.Pipeline.ManualImportTimeout.Duration,
			ImportRetryCooldown:   cfg.Pipeline.ImportRetryCooldown.Duration,
			Weights: WeightsSettings{
				Format:      cfg.Pipeline.Weights.Format,
				Bitrate:     cfg.Pipeline.Weights.Bitrate,
				Reliability: cfg.Pipeline.Weights.Reliability,
				FileCount:   cfg.Pipeline.Weights.FileCount,
				KnownUser:   cfg.Pipeline.Weights.KnownUser,
			},
		},
		Soulseek: SoulseekSettings{
			ServerAddress:             cfg.Soulseek.ServerAddress,
			Username:                  cfg.Soulseek.Username,
			ListenAddr:                cfg.Soulseek.ListenAddr,
			UploadSlots:               cfg.Soulseek.UploadSlots,
			AllowPrivatePeerAddresses: cfg.Soulseek.AllowPrivatePeerAddresses,
			Gluetun:                   GluetunSettings{ControlURL: cfg.Soulseek.Gluetun.ControlURL},
			SharedFolders:             append([]SharedFolderConfig(nil), cfg.Soulseek.SharedFolders...),
		},
		Store: StoreSettings{},
		Observ: ObservSettings{
			ListenAddr: cfg.Observ.ListenAddr,
			LogLevel:   cfg.Observ.LogLevel,
		},
		Paths: PathsSettings{SlskdCompleteDir: cfg.Paths.SlskdCompleteDir},
	}
}

func mustLoad(t *testing.T, path string) Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// --- representative multi-section update, including missing-table creation ---

func TestApplySettingsUpdatesRepresentativeFieldsAcrossEverySection(t *testing.T) {
	// valid.toml has no [soulseek] section at all and no pipeline.weights.known_user,
	// so this also exercises table/key creation for both.
	path := writeFixture(t)
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)

	s.Lidarr.URL = "http://lidarr2:8686"
	s.Lidarr.APIKey = strPtr("newlidarrkey")
	s.Slskd.APIKey = strPtr("newslskdkey")
	s.Pipeline.MaxActive = 50
	s.Pipeline.WantedSyncInterval = 20 * time.Minute
	s.Pipeline.Weights.KnownUser = 0.6
	s.Soulseek = SoulseekSettings{
		ServerAddress:             "server.slsknet.org:2242",
		Username:                  "souluser",
		Password:                  strPtr("soulpass"),
		ListenAddr:                "0.0.0.0:2234",
		UploadSlots:               2,
		AllowPrivatePeerAddresses: true,
		Gluetun: GluetunSettings{
			ControlURL: "http://127.0.0.1:8000",
			APIKey:     strPtr("gluetun-key"),
		},
		SharedFolders: []SharedFolderConfig{
			{Name: "Music", Path: "/shares/music"},
			{Name: "Live", Path: "/shares/live"},
		},
	}

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got := mustLoad(t, path)
	if got.Lidarr.URL != "http://lidarr2:8686" || got.Lidarr.APIKey != "newlidarrkey" {
		t.Errorf("Lidarr = %+v", got.Lidarr)
	}
	if got.Slskd.APIKey != "newslskdkey" {
		t.Errorf("Slskd.APIKey = %q", got.Slskd.APIKey)
	}
	if got.Pipeline.MaxActive != 50 || got.Pipeline.WantedSyncInterval.Duration != 20*time.Minute {
		t.Errorf("Pipeline = %+v", got.Pipeline)
	}
	if got.Pipeline.Weights.KnownUser != 0.6 {
		t.Errorf("Weights.KnownUser = %v, want 0.6", got.Pipeline.Weights.KnownUser)
	}
	if !got.Soulseek.Enabled() {
		t.Fatal("soulseek section was not created/enabled")
	}
	if got.Soulseek.ServerAddress != "server.slsknet.org:2242" || got.Soulseek.Username != "souluser" ||
		got.Soulseek.Password != "soulpass" || got.Soulseek.ListenAddr != "0.0.0.0:2234" || got.Soulseek.UploadSlots != 2 ||
		!got.Soulseek.AllowPrivatePeerAddresses {
		t.Errorf("Soulseek = %+v", got.Soulseek)
	}
	if got.Soulseek.Gluetun.ControlURL != "http://127.0.0.1:8000" || got.Soulseek.Gluetun.APIKey != "gluetun-key" {
		t.Errorf("Soulseek.Gluetun = %+v", got.Soulseek.Gluetun)
	}
	want := []SharedFolderConfig{{Name: "Music", Path: "/shares/music"}, {Name: "Live", Path: "/shares/live"}}
	if len(got.Soulseek.SharedFolders) != len(want) {
		t.Fatalf("SharedFolders = %+v", got.Soulseek.SharedFolders)
	}
	for i := range want {
		if got.Soulseek.SharedFolders[i] != want[i] {
			t.Errorf("SharedFolders[%d] = %+v, want %+v", i, got.Soulseek.SharedFolders[i], want[i])
		}
	}
}

// --- untouched optional sections/keys must stay absent, not materialize as zero ---

func TestApplySettingsUntouchedOptionalSectionsStayAbsent(t *testing.T) {
	path := writeFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg) // Soulseek all zero (disabled) — matches the fixture's absence

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "[soulseek]") {
		t.Errorf("an untouched, never-configured soulseek section was materialized:\n%s", after)
	}
	got := mustLoad(t, path)
	if got.Soulseek.Enabled() {
		t.Error("soulseek became enabled despite an all-zero, untouched submission")
	}
	// known_user used to be asserted absent here, because it was the one weight
	// with no default: materializing it would have invented a value the user
	// never chose. Since #405 it has one, so it is no longer invented - it is
	// resolved, exactly like max_active and the other defaulted pipeline keys
	// this test already accepts materializing (see below). What must still hold
	// is that the value written is the documented default, not a zero.
	if got.Pipeline.Weights.KnownUser != defaultWeights.KnownUser {
		t.Errorf("KnownUser = %v after round trip, want the default %v",
			got.Pipeline.Weights.KnownUser, defaultWeights.KnownUser)
	}
	_ = before // the rest of the fixture may still gain defaulted pipeline keys (existing, accepted behavior)
}

// --- full-fixture unchanged round trip is byte-identical, including shared folders ---

func TestApplySettingsFullFixtureUnchangedIsByteIdentical(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("fully-populated fixture changed despite an all-unchanged update:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// --- secrets: nil always keeps the currently configured value, across all six ---

func TestApplySettingsNilSecretsKeepAllSixValues(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg) // every secret pointer nil
	s.Paths.SlskdCompleteDir = "/music/new-downloads"

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got := mustLoad(t, path)
	if got.Lidarr.APIKey != "abc" {
		t.Errorf("Lidarr.APIKey = %q, want unchanged", got.Lidarr.APIKey)
	}
	if got.Slskd.APIKey != "def" {
		t.Errorf("Slskd.APIKey = %q, want unchanged", got.Slskd.APIKey)
	}
	if got.Soulseek.Password != "soulpass" {
		t.Errorf("Soulseek.Password = %q, want unchanged", got.Soulseek.Password)
	}
	if got.Soulseek.Gluetun.APIKey != "gluetun-key" {
		t.Errorf("Soulseek.Gluetun.APIKey = %q, want unchanged", got.Soulseek.Gluetun.APIKey)
	}
	if got.Store.DSN != "postgres://slusk:password@postgres:5432/slusk?sslmode=disable" {
		t.Errorf("Store.DSN = %q, want unchanged", got.Store.DSN)
	}
	if got.Observ.AuthToken != "op-token" {
		t.Errorf("Observ.AuthToken = %q, want unchanged", got.Observ.AuthToken)
	}
	if got.Paths.SlskdCompleteDir != "/music/new-downloads" {
		t.Errorf("Paths.SlskdCompleteDir = %q, want the actual requested change applied", got.Paths.SlskdCompleteDir)
	}
}

// TestApplySettingsPreservesGluetunPollInterval locks that saving settings
// from the UI does not drop soulseek.gluetun.poll_interval (#395). The key is
// deliberately absent from GluetunSettings - it is not editable there - but it
// sits inside a table ApplySettings does upsert into, so unlike a wholly
// untouched section its survival depends on the writer editing in place rather
// than re-rendering the table. Losing it would silently reset a shortened
// interval back to the 5m default.
func TestApplySettingsPreservesGluetunPollInterval(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml",
		[2]string{"api_key = \"gluetun-key\"\n", "api_key = \"gluetun-key\"\npoll_interval = \"45s\"\n"},
	)
	cfg := mustLoad(t, path)
	if cfg.Soulseek.Gluetun.PollInterval.Duration != 45*time.Second {
		t.Fatalf("fixture PollInterval = %v, want 45s", cfg.Soulseek.Gluetun.PollInterval.Duration)
	}

	s := settingsFromConfig(cfg)
	s.Soulseek.Gluetun.ControlURL = "http://127.0.0.1:9000" // force the table to be touched

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got := mustLoad(t, path)
	if got.Soulseek.Gluetun.ControlURL != "http://127.0.0.1:9000" {
		t.Errorf("Gluetun.ControlURL = %q, want the requested change applied", got.Soulseek.Gluetun.ControlURL)
	}
	if got.Soulseek.Gluetun.PollInterval.Duration != 45*time.Second {
		t.Errorf("Gluetun.PollInterval = %v, want 45s preserved across a settings save", got.Soulseek.Gluetun.PollInterval.Duration)
	}
}

// --- shared folders: unchanged list is byte-identical; changed list is rewritten ---

func TestApplySettingsSharedFoldersUnchangedByteIdentical(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg) // same two folders, same order

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("shared folders section changed despite an unchanged list:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestApplySettingsSharedFoldersChangedRewritesAndRoundTrips(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	// Reorder, drop "Live", and add a new folder.
	s.Soulseek.SharedFolders = []SharedFolderConfig{
		{Name: "Podcasts", Path: "/shares/podcasts"},
		{Name: "Music", Path: "/shares/music"},
	}

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got := mustLoad(t, path)
	want := s.Soulseek.SharedFolders
	if len(got.Soulseek.SharedFolders) != len(want) {
		t.Fatalf("SharedFolders = %+v, want %+v", got.Soulseek.SharedFolders, want)
	}
	for i := range want {
		if got.Soulseek.SharedFolders[i] != want[i] {
			t.Errorf("SharedFolders[%d] = %+v, want %+v", i, got.Soulseek.SharedFolders[i], want[i])
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(after), "[[soulseek.shared_folders]]"); n != 2 {
		t.Errorf("found %d [[soulseek.shared_folders]] blocks, want 2:\n%s", n, after)
	}
	if strings.Contains(string(after), "Live") {
		t.Error("removed shared folder \"Live\" still present in the file")
	}
}

// --- config.toml.bak ---

func TestApplySettingsWritesBakWithPreSaveBytes(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Pipeline.MaxActive = 99

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if string(bak) != string(before) {
		t.Errorf(".bak does not contain the pre-save bytes:\n--- bak ---\n%s\n--- want ---\n%s", bak, before)
	}
}

func TestApplySettingsBakWriteFailureDoesNotBlockSave(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	// A directory at the .bak path makes os.WriteFile(path+".bak", ...) fail
	// (EISDIR), simulating any reason the best-effort backup can't be written.
	if err := os.Mkdir(path+".bak", 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Pipeline.MaxActive = 77

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings should tolerate a failed .bak write, got: %v", err)
	}

	got := mustLoad(t, path)
	if got.Pipeline.MaxActive != 77 {
		t.Errorf("MaxActive = %d, want 77 (main save should have proceeded)", got.Pipeline.MaxActive)
	}
}

// --- comment preservation on scalar edits, untouched sections stay byte-identical ---

func TestApplySettingsPreservesCommentsAndUntouchedLines(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml",
		[2]string{"[store]\n", "# Shared Postgres instance for the whole arr-stack.\n[store]\n"},
		[2]string{
			`dsn = "postgres://slusk:password@postgres:5432/slusk?sslmode=disable"`,
			`dsn = "postgres://slusk:password@postgres:5432/slusk?sslmode=disable" # primary`,
		},
	)

	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Lidarr.URL = "http://lidarr2:8686"
	s.Pipeline.MaxActive = 50

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "# Shared Postgres instance for the whole arr-stack.") {
		t.Error("block comment on an untouched section was dropped")
	}
	dsnComment := regexp.MustCompile(`dsn = "postgres://slusk:password@postgres:5432/slusk\?sslmode=disable"\s*# primary`)
	if !dsnComment.Match(after) {
		t.Errorf("inline trailing comment on an untouched key was dropped:\n%s", after)
	}
	for _, untouched := range []string{
		"[slskd]\nurl = \"http://slskd:5030\"\napi_key = \"def\"",
		"[observ]\nlisten_addr = \"127.0.0.1:9090\"",
		"[paths]\nslskd_complete_dir = \"/music/slskd-downloads\"",
	} {
		if !strings.Contains(string(after), untouched) {
			t.Errorf("untouched fixture text missing after update: %q\n--- got ---\n%s", untouched, after)
		}
	}
}

func TestApplySettingsReadOnlyDirReturnsErrNotWritable(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	dir := filepath.Dir(path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restore write permission before t.TempDir()'s cleanup tries to remove
	// the directory; t.Cleanup runs LIFO, so this registered-after cleanup
	// fires before TempDir's own removal.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Lidarr.URL = "http://changed:8686"

	err = ApplySettings(path, s)
	if !errors.Is(err, ErrNotWritable) {
		t.Fatalf("ApplySettings error = %v, want ErrNotWritable", err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("original file was modified despite a read-only directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want exactly the original config file (no temp litter): %v", len(entries), entries)
	}
}

func TestApplySettingsHandEditSurvives(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")

	// Simulate a hand edit to an unrelated key made after "startup" (i.e.
	// after the file we're about to ApplySettings against was last read).
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(contents), `search_timeout = "30s"`, `search_timeout = "45s"`, 1)
	if edited == string(contents) {
		t.Fatal("search_timeout replacement did not match")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Pipeline.MaxActive = 99

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got := mustLoad(t, path)
	if got.Pipeline.SearchTimeout.Duration != 45*time.Second {
		t.Errorf("hand-edited search_timeout was lost: got %v, want 45s", got.Pipeline.SearchTimeout.Duration)
	}
	if got.Pipeline.MaxActive != 99 {
		t.Errorf("ApplySettings change was lost: MaxActive = %d, want 99", got.Pipeline.MaxActive)
	}
}

func TestApplySettingsWrittenFileRoundTripsThroughLoad(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml")
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Lidarr.URL = "http://lidarr3:8686"
	s.Pipeline.MaxActive = 12

	if err := ApplySettings(path, s); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("written config failed to Load: %v", err)
	}
	if got.Lidarr.URL != "http://lidarr3:8686" || got.Pipeline.MaxActive != 12 {
		t.Errorf("round-tripped config = %+v", got)
	}
}

func TestApplySettingsCurrentFileSyntaxErrorFails(t *testing.T) {
	path := writeFixtureFrom(t, "write_full.toml", [2]string{
		`url = "http://lidarr:8686"`,
		`url = "http://lidarr:8686`, // unterminated string: a genuine parse error
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = ApplySettings(path, settingsFromConfig(Config{}))
	if err == nil {
		t.Fatal("expected an error for a syntactically invalid current file, got nil")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file was modified despite a parse failure")
	}
}

func TestApplySettingsCurrentFileUnknownKeyFails(t *testing.T) {
	// A key tomledit can parse structurally, but which our strict schema
	// backstop (LoadBytes) rejects, must still block the write.
	path := writeFixtureFrom(t, "write_full.toml", [2]string{"[store]\n", "[store]\nbogus_key = \"nope\"\n"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, "testdata/write_full.toml")
	s := settingsFromConfig(cfg)
	s.Lidarr.URL = "http://changed:8686"

	err = ApplySettings(path, s)
	if err == nil {
		t.Fatal("expected an error for a current file with an unknown key, got nil")
	}
	if !errors.Is(err, ErrValidationFailed) {
		t.Errorf("error = %v, want ErrValidationFailed", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file was modified despite the backstop validation failure")
	}
}

func TestApplySettingsBackstopRejectsCrossFieldInvalidCombination(t *testing.T) {
	// backend = "soulseek" requires a configured [soulseek] section — a
	// cross-field rule the per-field validation in observ does not duplicate,
	// left entirely to this backstop.
	path := writeFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	s := settingsFromConfig(cfg)
	s.Pipeline.Backend = "soulseek" // Soulseek stays entirely unconfigured

	err = ApplySettings(path, s)
	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("ApplySettings error = %v, want ErrValidationFailed", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file was modified despite a backstop-rejected cross-field combination")
	}
}
