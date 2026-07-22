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

// writeFixture copies testdata/valid.toml into a fresh temp file, applying
// any string replacements first (so callers can inject comments, extra keys,
// or hand edits while keeping the rest of the fixture's required fields
// intact). It returns the path to the writable copy.
func writeFixture(t *testing.T, replacements ...[2]string) string {
	t.Helper()
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(base)
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

func strPtr(s string) *string { return &s }

func TestApplySettingsUpdatesAllSixKeys(t *testing.T) {
	// testdata/valid.toml has no explicit wanted_sync_interval or max_active
	// (both rely on applyDefaults), so this also exercises key creation.
	path := writeFixture(t)

	err := ApplySettings(path, Settings{
		LidarrURL:          "http://lidarr2:8686",
		LidarrAPIKey:       strPtr("newkey"),
		WantedSyncInterval: 20 * time.Minute,
		StallTimeout:       10 * time.Minute,
		MaxActive:          50,
		MinBitrate:         256,
	})
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after ApplySettings: %v", err)
	}
	if cfg.Lidarr.URL != "http://lidarr2:8686" {
		t.Errorf("Lidarr.URL = %q", cfg.Lidarr.URL)
	}
	if cfg.Lidarr.APIKey != "newkey" {
		t.Errorf("Lidarr.APIKey = %q", cfg.Lidarr.APIKey)
	}
	if cfg.Pipeline.WantedSyncInterval.Duration != 20*time.Minute {
		t.Errorf("WantedSyncInterval = %v", cfg.Pipeline.WantedSyncInterval.Duration)
	}
	if cfg.Pipeline.StallTimeout.Duration != 10*time.Minute {
		t.Errorf("StallTimeout = %v", cfg.Pipeline.StallTimeout.Duration)
	}
	if cfg.Pipeline.MaxActive != 50 {
		t.Errorf("MaxActive = %d", cfg.Pipeline.MaxActive)
	}
	if cfg.Pipeline.MinBitrate != 256 {
		t.Errorf("MinBitrate = %d", cfg.Pipeline.MinBitrate)
	}
}

func TestApplySettingsPreservesCommentsAndUntouchedLines(t *testing.T) {
	path := writeFixture(t,
		[2]string{"[store]\n", "# Shared Postgres instance for the whole arr-stack.\n[store]\n"},
		[2]string{
			`dsn = "postgres://slskdarr:password@postgres:5432/slskdarr?sslmode=disable"`,
			`dsn = "postgres://slskdarr:password@postgres:5432/slskdarr?sslmode=disable" # primary`,
		},
	)

	// Change only the Lidarr URL and max_active; leave the rest matching the
	// fixture's current effective values (stall_timeout, min_bitrate) or its
	// applied defaults (wanted_sync_interval), and keep the API key untouched.
	err := ApplySettings(path, Settings{
		LidarrURL:          "http://lidarr2:8686",
		WantedSyncInterval: 15 * time.Minute,
		StallTimeout:       5 * time.Minute,
		MaxActive:          50,
		MinBitrate:         192,
	})
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "# Shared Postgres instance for the whole arr-stack.") {
		t.Error("block comment on an untouched section was dropped")
	}
	// tomledit's Formatter re-serializes the whole document and normalizes the
	// gutter width before a trailing comment (observed: one space on disk
	// becomes two), so this checks the comment's content survived rather than
	// asserting exact original spacing.
	dsnComment := regexp.MustCompile(`dsn = "postgres://slskdarr:password@postgres:5432/slskdarr\?sslmode=disable"\s*# primary`)
	if !dsnComment.Match(after) {
		t.Errorf("inline trailing comment on an untouched key was dropped:\n%s", after)
	}
	// The [slskd], [pipeline.weights], [observ], and [paths] sections are
	// entirely untouched by this update; their lines must be byte-identical.
	for _, untouched := range []string{
		"[slskd]\nurl = \"http://slskd:5030\"\napi_key = \"def\"",
		"[pipeline.weights]\nformat = 1.0",
		"[observ]\nlisten_addr = \"127.0.0.1:9090\"",
		"[paths]\nslskd_complete_dir = \"/music/slskd-downloads\"",
	} {
		if !strings.Contains(string(after), untouched) {
			t.Errorf("untouched fixture text missing after update: %q\n--- got ---\n%s", untouched, after)
		}
	}
}

func TestApplySettingsUnchangedValuesLeaveLinesByteIdentical(t *testing.T) {
	// Make wanted_sync_interval and max_active explicit (rather than relying
	// on applyDefaults) so "no change requested" is meaningful at the byte
	// level for every one of the six keys.
	path := writeFixture(t, [2]string{
		"max_transfer_retries = 3\n",
		"max_transfer_retries = 3\nwanted_sync_interval = \"15m\"\nmax_active = 30\n",
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = ApplySettings(path, Settings{
		LidarrURL:          cfg.Lidarr.URL,
		LidarrAPIKey:       nil, // leave untouched
		WantedSyncInterval: cfg.Pipeline.WantedSyncInterval.Duration,
		StallTimeout:       cfg.Pipeline.StallTimeout.Duration,
		MaxActive:          cfg.Pipeline.MaxActive,
		MinBitrate:         cfg.Pipeline.MinBitrate,
	})
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("file changed despite an all-unchanged update:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestApplySettingsNilAPIKeyKeepsExisting(t *testing.T) {
	path := writeFixture(t)

	err := ApplySettings(path, Settings{
		LidarrURL:          "http://lidarr:8686",
		LidarrAPIKey:       nil,
		WantedSyncInterval: 15 * time.Minute,
		StallTimeout:       5 * time.Minute,
		MaxActive:          30,
		MinBitrate:         192,
	})
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `api_key = "abc"`) {
		t.Errorf("api_key line changed despite a nil LidarrAPIKey:\n%s", contents)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Lidarr.APIKey != "abc" {
		t.Errorf("Lidarr.APIKey = %q, want unchanged %q", cfg.Lidarr.APIKey, "abc")
	}
}

func TestApplySettingsReadOnlyDirReturnsErrNotWritable(t *testing.T) {
	path := writeFixture(t)
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

	err = ApplySettings(path, Settings{
		LidarrURL: "http://changed:8686", WantedSyncInterval: 15 * time.Minute,
		StallTimeout: 5 * time.Minute, MaxActive: 30, MinBitrate: 192,
	})
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
	path := writeFixture(t)

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

	err = ApplySettings(path, Settings{
		LidarrURL: "http://lidarr:8686", WantedSyncInterval: 15 * time.Minute,
		StallTimeout: 5 * time.Minute, MaxActive: 99, MinBitrate: 192,
	})
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.SearchTimeout.Duration != 45*time.Second {
		t.Errorf("hand-edited search_timeout was lost: got %v, want 45s", cfg.Pipeline.SearchTimeout.Duration)
	}
	if cfg.Pipeline.MaxActive != 99 {
		t.Errorf("ApplySettings change was lost: MaxActive = %d, want 99", cfg.Pipeline.MaxActive)
	}
}

func TestApplySettingsWrittenFileRoundTripsThroughLoad(t *testing.T) {
	path := writeFixture(t)

	err := ApplySettings(path, Settings{
		LidarrURL: "http://lidarr3:8686", WantedSyncInterval: 25 * time.Minute,
		StallTimeout: 15 * time.Minute, MaxActive: 12, MinBitrate: 320,
	})
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("written config failed to Load: %v", err)
	}
	if cfg.Lidarr.URL != "http://lidarr3:8686" || cfg.Pipeline.MaxActive != 12 {
		t.Errorf("round-tripped config = %+v", cfg)
	}
}

func TestApplySettingsCurrentFileSyntaxErrorFails(t *testing.T) {
	path := writeFixture(t, [2]string{
		`url = "http://lidarr:8686"`,
		`url = "http://lidarr:8686`, // unterminated string: a genuine parse error
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = ApplySettings(path, Settings{
		LidarrURL: "http://changed:8686", WantedSyncInterval: 15 * time.Minute,
		StallTimeout: 5 * time.Minute, MaxActive: 30, MinBitrate: 192,
	})
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
	path := writeFixture(t, [2]string{"[store]\n", "[store]\nbogus_key = \"nope\"\n"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = ApplySettings(path, Settings{
		LidarrURL: "http://changed:8686", WantedSyncInterval: 15 * time.Minute,
		StallTimeout: 5 * time.Minute, MaxActive: 30, MinBitrate: 192,
	})
	if err == nil {
		t.Fatal("expected an error for a current file with an unknown key, got nil")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file was modified despite the backstop validation failure")
	}
}
