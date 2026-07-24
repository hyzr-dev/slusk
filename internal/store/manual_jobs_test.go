package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestCreateManualJobProducesRunnableDownload(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Some Album", "Some Artist", "peer1", []ManualJobFile{
		{Filename: "01 - Track.flac", Size: 111},
		{Filename: "02 - Track.flac", Size: 222},
	}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	if job.State != core.StateDownloading {
		t.Fatalf("job.State = %v, want DOWNLOADING", job.State)
	}
	if job.Source != core.SourceManual {
		t.Fatalf("job.Source = %v, want manual", job.Source)
	}
	if job.LidarrAlbumID != 0 {
		t.Fatalf("job.LidarrAlbumID = %d, want 0 (NULL)", job.LidarrAlbumID)
	}

	cand, found, err := s.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if cand.Username != "peer1" {
		t.Fatalf("candidate.Username = %q, want peer1", cand.Username)
	}
	if len(cand.Files) != 2 {
		t.Fatalf("candidate.Files = %+v, want 2 files", cand.Files)
	}

	transfers, err := s.TransfersForCandidate(ctx, cand.ID)
	if err != nil {
		t.Fatalf("TransfersForCandidate: %v", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("len(transfers) = %d, want 2", len(transfers))
	}
	for _, tr := range transfers {
		if tr.State != core.TransferPending {
			t.Errorf("transfer %q state = %v, want PENDING", tr.Filename, tr.State)
		}
		if tr.Username != "peer1" {
			t.Errorf("transfer %q username = %q, want peer1", tr.Filename, tr.Username)
		}
	}

	// Round-trip through the read paths used by the dashboard.
	got, found, err := s.JobDetail(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobDetail: %v found=%v", err, found)
	}
	if got.Job.Source != core.SourceManual {
		t.Fatalf("JobDetail job.Source = %v, want manual", got.Job.Source)
	}
}

func TestCreateManualJobCoexistsWithLidarrJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	lidarrJob, err := s.UpsertWantedJob(ctx, 900, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	manualJob, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer2",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}

	if lidarrJob.ID == manualJob.ID {
		t.Fatalf("expected distinct job ids, both are %d", lidarrJob.ID)
	}
	if lidarrJob.Source != core.SourceLidarr {
		t.Fatalf("lidarrJob.Source = %v, want lidarr", lidarrJob.Source)
	}
	if manualJob.Source != core.SourceManual {
		t.Fatalf("manualJob.Source = %v, want manual", manualJob.Source)
	}
}

// TestManualJobsMigrationSemantics exercises the migration's schema changes
// directly (0003_manual_jobs.sql): pre-existing rows default to
// source='lidarr' and keep their global uniqueness, while manual rows (NULL
// lidarr_album_id) are exempt from that uniqueness and may coexist freely.
func TestManualJobsMigrationSemantics(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// A job written the same way WantedSync/UpsertWantedJob writes one
	// (no explicit source column) must default to 'lidarr'.
	var source string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $3) RETURNING source`,
		1001, string(core.StateWanted), now).Scan(&source); err != nil {
		t.Fatalf("insert lidarr job: %v", err)
	}
	if source != string(core.SourceLidarr) {
		t.Fatalf("default source = %q, want %q", source, core.SourceLidarr)
	}

	// The old table-wide UNIQUE(lidarr_album_id) constraint must be gone,
	// replaced by the partial index below - otherwise the NULL-coexistence
	// assertions further down would pass for the wrong reason.
	var oldConstraintCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conname = 'album_jobs_lidarr_album_id_key'`).
		Scan(&oldConstraintCount); err != nil {
		t.Fatalf("query pg_constraint: %v", err)
	}
	if oldConstraintCount != 0 {
		t.Error("album_jobs_lidarr_album_id_key still present after migration 0003")
	}

	// A second 'lidarr' row with the same lidarr_album_id must still be
	// rejected by the partial unique index.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, source, state, created_at, updated_at)
		 VALUES ($1, 'lidarr', $2, $3, $3)`,
		1001, string(core.StateWanted), now)
	if err == nil {
		t.Fatal("expected duplicate lidarr_album_id with source='lidarr' to be rejected")
	}

	// Two manual rows (NULL lidarr_album_id) must coexist without tripping
	// that same index.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, source, state, created_at, updated_at)
		 VALUES (NULL, 'manual', $1, $2, $2)`,
		string(core.StateDownloading), now); err != nil {
		t.Fatalf("insert first manual job: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, source, state, created_at, updated_at)
		 VALUES (NULL, 'manual', $1, $2, $2)`,
		string(core.StateDownloading), now); err != nil {
		t.Fatalf("insert second manual job: %v", err)
	}
}

// A second manual job whose files overlap with a still-live candidate's
// (peer, filename) pair must be rejected rather than silently corrupting the
// live-remote-owner uniqueness invariant that Selecting also depends on.
func TestCreateManualJobRejectsLiveRemoteFileConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	if _, err := s.CreateManualJob(ctx, "First", "Artist", "peer3",
		[]ManualJobFile{{Filename: "shared.flac", Size: 1}}, now); err != nil {
		t.Fatalf("CreateManualJob (first): %v", err)
	}

	_, err := s.CreateManualJob(ctx, "Second", "Artist", "peer3",
		[]ManualJobFile{{Filename: "shared.flac", Size: 1}}, now.Add(time.Minute))
	if !errors.Is(err, ErrRemoteFileBusy) {
		t.Fatalf("CreateManualJob (conflicting) = %v, want ErrRemoteFileBusy", err)
	}

	// The rejected attempt must not leave a partially created job behind.
	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1 (the rolled-back job must not persist)", len(views))
	}
}
