package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func TestAlbumFolder(t *testing.T) {
	complete := "/music/slskd-downloads"
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}
	got := AlbumFolder(complete, files)
	want := "/music/slskd-downloads/1000 Forms of Fear (2014)"
	if got != want {
		t.Errorf("AlbumFolder = %q, want %q", got, want)
	}
}

func TestAlbumFolderDeeplyNestedRemoteShare(t *testing.T) {
	// slskd only recreates the leaf album folder locally, discarding the
	// remote peer's own alphabetical share structure (Music/<letter>/<artist>/<album>).
	complete := "/data/media/downloads-slskd"
	files := []string{
		`Music\B\Blut Aus Nord\2023 - Disharmonium - Nahab\01 - Track.flac`,
		`Music\B\Blut Aus Nord\2023 - Disharmonium - Nahab\02 - Track.flac`,
	}
	got := AlbumFolder(complete, files)
	want := "/data/media/downloads-slskd/2023 - Disharmonium - Nahab"
	if got != want {
		t.Errorf("AlbumFolder = %q, want %q", got, want)
	}
}

func TestAlbumFolderFallsBackToRoot(t *testing.T) {
	if got := AlbumFolder("/music/dl", nil); got != "/music/dl" {
		t.Errorf("empty filenames should fall back to root, got %q", got)
	}
	// No common directory -> fall back to root.
	files := []string{`a\1.flac`, `b\2.flac`}
	if got := AlbumFolder("/music/dl", files); got != "/music/dl" {
		t.Errorf("no common dir should fall back to root, got %q", got)
	}
}

func TestCommonLeaf(t *testing.T) {
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}
	got := commonLeaf(files)
	want := "1000 Forms of Fear (2014)"
	if got != want {
		t.Errorf("commonLeaf = %q, want %q", got, want)
	}
}

// TestCommonLeafRejectsTraversal locks the upstream half of the path-traversal
// fix: a common remote directory that resolves to ".." must yield "" so it can
// never reach AlbumFolder (scan) or DeleteDownloadFolder (os.RemoveAll).
func TestCommonLeafRejectsTraversal(t *testing.T) {
	for _, files := range [][]string{
		{`..\track1.flac`, `..\track2.flac`},
		{`a\..\..\x.flac`, `a\..\..\y.flac`},
	} {
		if got := commonLeaf(files); got != "" {
			t.Errorf("commonLeaf(%v) = %q, want \"\" (traversal must be rejected)", files, got)
		}
	}
}

func TestCommonLeafEmptyWhenAmbiguous(t *testing.T) {
	if got := commonLeaf(nil); got != "" {
		t.Errorf("empty filenames should yield \"\", got %q", got)
	}
	// No common directory -> ambiguous.
	files := []string{`a\1.flac`, `b\2.flac`}
	if got := commonLeaf(files); got != "" {
		t.Errorf("no common dir should yield \"\", got %q", got)
	}
}

// fakeFolderRegistry is an in-memory DownloadFolderRegistry: the leaves a job
// registered while downloading, minus the ones cleanup has already stamped.
type fakeFolderRegistry struct {
	leaves  []string
	cleaned []string
	err     error
}

func (f *fakeFolderRegistry) DownloadFoldersForJob(ctx context.Context, jobID int64) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.leaves, nil
}

func (f *fakeFolderRegistry) MarkDownloadFolderCleaned(ctx context.Context, jobID int64, leaf string, now time.Time) error {
	f.cleaned = append(f.cleaned, leaf)
	return nil
}

// newTestLogger returns a *slog.Logger writing to buf, so cleanupCompletedFolder
// tests can assert on the specific log level (Info vs Error) emitted.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func TestCleanupCompletedFolderRemovesEmptyDir(t *testing.T) {
	completeDir := t.TempDir()
	folder := filepath.Join(completeDir, "1000 Forms of Fear (2014)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	reg := &fakeFolderRegistry{leaves: []string{"1000 Forms of Fear (2014)"}}

	var buf bytes.Buffer
	cleanupCompletedFolder(context.Background(), reg, newTestLogger(&buf), 1, completeDir, time.Now())

	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Errorf("expected empty folder to be removed, stat err = %v", err)
	}
	if len(reg.cleaned) != 1 || reg.cleaned[0] != "1000 Forms of Fear (2014)" {
		t.Errorf("cleaned = %v, want the removed folder stamped once", reg.cleaned)
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line, got %q", logged)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line, got %q", logged)
	}
}

func TestCleanupCompletedFolderSkipsNonEmptyDir(t *testing.T) {
	completeDir := t.TempDir()
	folder := filepath.Join(completeDir, "1000 Forms of Fear (2014)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "leftover.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	reg := &fakeFolderRegistry{leaves: []string{"1000 Forms of Fear (2014)"}}

	var buf bytes.Buffer
	cleanupCompletedFolder(context.Background(), reg, newTestLogger(&buf), 1, completeDir, time.Now())

	if _, err := os.Stat(folder); err != nil {
		t.Errorf("expected non-empty folder to remain, stat err = %v", err)
	}
	if len(reg.cleaned) != 1 {
		t.Errorf("cleaned = %v, want the deliberate leave-in-place stamped once", reg.cleaned)
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line for a non-empty folder, got %q", logged)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line, got %q", logged)
	}
}

// TestCleanupCompletedFolderLogsEmptyRegister pins the defect issue #314 was
// filed about: a job with nothing to clean up used to return in total silence -
// no log line, no job_event - so a folder left on disk was untraceable. An empty
// register must say so.
func TestCleanupCompletedFolderLogsEmptyRegister(t *testing.T) {
	completeDir := t.TempDir()
	reg := &fakeFolderRegistry{}

	var buf bytes.Buffer
	cleanupCompletedFolder(context.Background(), reg, newTestLogger(&buf), 1, completeDir, time.Now())

	logged := buf.String()
	if !strings.Contains(logged, "no registered download folder") {
		t.Errorf("expected an INFO line naming the empty register, got %q", logged)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line, got %q", logged)
	}
}

func TestCleanupCompletedFolderLogsMissingDirQuietly(t *testing.T) {
	completeDir := t.TempDir()
	reg := &fakeFolderRegistry{leaves: []string{"1000 Forms of Fear (2014)"}}

	var buf bytes.Buffer
	cleanupCompletedFolder(context.Background(), reg, newTestLogger(&buf), 1, completeDir, time.Now())

	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line for a missing folder, got %q", logged)
	}
	if len(reg.cleaned) != 1 {
		t.Errorf("cleaned = %v, want an already-absent folder stamped (local os.ReadDir is trustworthy evidence)", reg.cleaned)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line, got %q", logged)
	}
}

func TestQuarantineFolderMovesLeftovers(t *testing.T) {
	completeDir := t.TempDir()
	folder := filepath.Join(completeDir, "1000 Forms of Fear (2014)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "01 - Chandelier.flac.part"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	dst, moved := quarantineFolder(newTestLogger(&buf), 7, completeDir, "1000 Forms of Fear (2014)")

	if !moved {
		t.Fatalf("moved = false, want true; log = %q", buf.String())
	}
	want := filepath.Join(completeDir, quarantineDirName, "1000 Forms of Fear (2014)")
	if dst != want {
		t.Errorf("dst = %q, want %q", dst, want)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Errorf("expected source folder to be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, "01 - Chandelier.flac.part")); err != nil {
		t.Errorf("expected the partial file under quarantine, stat err = %v", err)
	}
	if logged := buf.String(); strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line, got %q", logged)
	}
}

func TestQuarantineFolderNoOpWhenSourceAbsent(t *testing.T) {
	completeDir := t.TempDir()

	var buf bytes.Buffer
	if _, moved := quarantineFolder(newTestLogger(&buf), 7, completeDir, "never downloaded"); moved {
		t.Error("moved = true, want false when the source never existed")
	}
	if _, err := os.Stat(filepath.Join(completeDir, quarantineDirName)); !os.IsNotExist(err) {
		t.Errorf("expected no quarantine dir to be created, stat err = %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line for the common absent case, got %q", logged)
	}
	if !strings.Contains(logged, "nothing to quarantine") {
		t.Errorf("expected an INFO line naming the absent folder, got %q", logged)
	}
}

func TestQuarantineFolderSkipsAmbiguousLeafAndQuarantineDir(t *testing.T) {
	completeDir := t.TempDir()
	self := filepath.Join(completeDir, quarantineDirName)
	if err := os.Mkdir(self, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	var buf bytes.Buffer
	log := newTestLogger(&buf)
	// commonLeaf's ambiguous result: the caller passes "" straight through.
	if _, moved := quarantineFolder(log, 7, completeDir, ""); moved {
		t.Error("moved = true, want false for an ambiguous leaf")
	}
	// A peer whose remote folder is literally named like the quarantine dir.
	if _, moved := quarantineFolder(log, 7, completeDir, quarantineDirName); moved {
		t.Error("moved = true, want false for the quarantine dir itself")
	}
	if _, err := os.Stat(self); err != nil {
		t.Errorf("expected the quarantine dir untouched, stat err = %v", err)
	}
	if _, moved := quarantineFolder(log, 7, "", "some album"); moved {
		t.Error("moved = true, want false when completeDir is empty")
	}
}

func TestQuarantineFolderSuffixesOnCollision(t *testing.T) {
	completeDir := t.TempDir()
	folder := filepath.Join(completeDir, "Album")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "new.flac"), []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	occupied := filepath.Join(completeDir, quarantineDirName, "Album")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "old.flac"), []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	dst, moved := quarantineFolder(newTestLogger(&buf), 42, completeDir, "Album")

	if !moved {
		t.Fatalf("moved = false, want true; log = %q", buf.String())
	}
	want := filepath.Join(completeDir, quarantineDirName, "Album.job42")
	if dst != want {
		t.Errorf("dst = %q, want %q", dst, want)
	}
	if _, err := os.Stat(filepath.Join(want, "new.flac")); err != nil {
		t.Errorf("expected the moved file under the suffixed dir, stat err = %v", err)
	}
	// The already-quarantined folder must survive untouched: os.Rename would
	// have silently replaced it had it been empty.
	body, err := os.ReadFile(filepath.Join(occupied, "old.flac"))
	if err != nil || string(body) != "old" {
		t.Errorf("pre-existing quarantined file = %q (%v), want %q", body, err, "old")
	}
}

// TestCleanupFolderRefusesQuarantineDir pins the destructive half of the
// ".failed" guard: a peer sharing a folder literally named like the quarantine
// directory must not make cleanupFolder hand that name to DeleteDownloadFolder,
// which is recursive on both backends and would destroy every album quarantined
// so far.
func TestCleanupFolderRefusesQuarantineDir(t *testing.T) {
	reg := &fakeFolderRegistry{leaves: []string{quarantineDirName}}

	var buf bytes.Buffer
	peers := &fakeSearcher{}
	cleanupFolder(context.Background(), peers, reg, newTestLogger(&buf), 7, time.Now())

	if len(peers.deletedFolders) != 0 {
		t.Errorf("DeleteDownloadFolder called with %v, want no call at all", peers.deletedFolders)
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line, got %q", logged)
	}
	if !strings.Contains(logged, "skipping cleanup") {
		t.Errorf("expected an INFO line saying the cleanup was skipped, got %q", logged)
	}
}

// TestCleanupFolderDeletesOrdinaryLeaf is the counterpart: an ordinary album
// folder must still be deleted, so the guard above cannot pass by disabling
// cleanup outright.
func TestCleanupFolderDeletesOrdinaryLeaf(t *testing.T) {
	reg := &fakeFolderRegistry{leaves: []string{"1000 Forms of Fear (2014)"}}
	var buf bytes.Buffer
	peers := &fakeSearcher{}
	cleanupFolder(context.Background(), peers, reg, newTestLogger(&buf), 7, time.Now())

	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "1000 Forms of Fear (2014)" {
		t.Errorf("deletedFolders = %v, want [%q]", peers.deletedFolders, "1000 Forms of Fear (2014)")
	}
	if len(reg.cleaned) != 1 || reg.cleaned[0] != "1000 Forms of Fear (2014)" {
		t.Errorf("cleaned = %v, want the deleted folder stamped", reg.cleaned)
	}
}

// TestCleanupFolderDeletesEveryRegisteredFolder is the reason the register is
// per-job rather than per-candidate: a job that retried has written to several
// peers' folders across several search cycles, and ResetJobToWanted deleted the
// transfer rows that used to be the only way to name the earlier ones.
func TestCleanupFolderDeletesEveryRegisteredFolder(t *testing.T) {
	reg := &fakeFolderRegistry{leaves: []string{"First Peer Album", "Second Peer Album"}}
	var buf bytes.Buffer
	peers := &fakeSearcher{}
	cleanupFolder(context.Background(), peers, reg, newTestLogger(&buf), 7, time.Now())

	if len(peers.deletedFolders) != 2 {
		t.Fatalf("deletedFolders = %v, want both registered folders", peers.deletedFolders)
	}
	if len(reg.cleaned) != 2 {
		t.Errorf("cleaned = %v, want both stamped", reg.cleaned)
	}
}

// TestCleanupFolderLogsEmptyRegister is cleanupFolder's half of the silence
// defect (see TestCleanupCompletedFolderLogsEmptyRegister).
func TestCleanupFolderLogsEmptyRegister(t *testing.T) {
	reg := &fakeFolderRegistry{}
	var buf bytes.Buffer
	peers := &fakeSearcher{}
	cleanupFolder(context.Background(), peers, reg, newTestLogger(&buf), 7, time.Now())

	if len(peers.deletedFolders) != 0 {
		t.Errorf("deletedFolders = %v, want no call", peers.deletedFolders)
	}
	if logged := buf.String(); !strings.Contains(logged, "no registered download folder") {
		t.Errorf("expected an INFO line naming the empty register, got %q", logged)
	}
}

// TestCleanupFolderDoesNotStampOnRemoteNotFound is the safety property that
// keeps issue #314's own defect from coming back through its fix: the slskd
// adapter maps every 404 to core.ErrRemoteNotFound, including one caused by a
// wrong base URL, so a 404 is not evidence the folder is gone. Stamping on it
// would let a misconfigured backend mark every job's folders cleaned while the
// files sit on disk, invisible to every later cleanup.
func TestCleanupFolderDoesNotStampOnRemoteNotFound(t *testing.T) {
	reg := &fakeFolderRegistry{leaves: []string{"Album"}}
	peers := &notFoundCleaner{}
	var buf bytes.Buffer
	cleanupFolder(context.Background(), peers, reg, newTestLogger(&buf), 7, time.Now())

	if len(reg.cleaned) != 0 {
		t.Errorf("cleaned = %v, want nothing stamped on a 404", reg.cleaned)
	}
	if logged := buf.String(); strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line for the routine 404, got %q", logged)
	}
}

// notFoundCleaner is a FolderCleaner whose delete always 404s.
type notFoundCleaner struct{ calls []string }

func (c *notFoundCleaner) DeleteDownloadFolder(ctx context.Context, name string) error {
	c.calls = append(c.calls, name)
	return core.ErrRemoteNotFound
}

// TestCleanupCompletedFolderRefusesQuarantineDir mirrors
// TestCleanupFolderRefusesQuarantineDir: the quarantine directory is not a
// completed job's to remove either, even on the days it happens to be empty.
func TestCleanupCompletedFolderRefusesQuarantineDir(t *testing.T) {
	completeDir := t.TempDir()
	self := filepath.Join(completeDir, quarantineDirName)
	if err := os.Mkdir(self, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	reg := &fakeFolderRegistry{leaves: []string{quarantineDirName}}

	var buf bytes.Buffer
	cleanupCompletedFolder(context.Background(), reg, newTestLogger(&buf), 1, completeDir, time.Now())

	if _, err := os.Stat(self); err != nil {
		t.Errorf("expected the quarantine dir untouched, stat err = %v", err)
	}
	if len(reg.cleaned) != 0 {
		t.Errorf("cleaned = %v, want the skipped quarantine dir left alone", reg.cleaned)
	}
}
