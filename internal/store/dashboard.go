// Package store: dashboard.go holds read-only projections used by the web
// dashboard (internal/observ). Nothing here mutates state.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// jobViewSelect joins each album_job with its most recently created candidate
// row (a), and that candidate is in turn joined twice for two different
// purposes:
//
//   - t is the candidate's single most recent transfer (by updated_at). It
//     must stay a single row rather than becoming an aggregate because
//     observ.dashboardStatus derives a job's stalled/active/failed status from
//     Transfer.State, and scanJobView copies t.username into JobView.Peer,
//     which the jobs list renders. These read one arbitrary file's row as if
//     it spoke for the album — the same conflation issue #174 fixed for the
//     byte columns, left in place here because changing it would change what
//     those two values mean. Manual cancellation does not use this projection;
//     its store transaction captures every live transfer across all candidates.
//   - agg sums bytes_done/bytes_total/remaining across every transfer of the
//     candidate. ActivateCandidateWithTransfers and CreateManualJob insert a
//     transfers row for every file of the album upfront (state PENDING,
//     bytes_total from the file size), so this sum is already album-complete
//     even for files the per-peer throttle (#20) hasn't released to the peer
//     backend yet — see core.JobView.AlbumBytes* and issue #174.
//
// A job with no candidates yet still appears, with NULL candidate/transfer
// columns and agg's COALESCE-guarded zeros. a.files is included (additive; no
// new JOINs or aggregates) so the observ package can aggregate album-level
// live speed/ETA across every file of the candidate rather than just the one
// transfer this view already joins (issue #157) — see core.Candidate.Files.
// Callers append their own WHERE clause.
const jobViewSelect = `
	SELECT
		j.id, COALESCE(j.lidarr_album_id, 0), j.state, j.candidates_tried, j.next_attempt_at, j.created_at, j.updated_at, j.title, j.artist_name, j.retries, j.not_before, j.failed_at, j.source, j.year, j.tracks, j.format,
		t.id, t.candidate_id, t.slskd_id, t.username, t.filename, t.state, t.bytes_done, t.bytes_total, t.deadline, t.last_progress_at, t.updated_at,
		a.id, a.album_job_id, a.username, a.score, a.state, a.fail_reason, a.created_at, a.updated_at, a.files,
		agg.bytes_done, agg.bytes_total, agg.bytes_remaining
	FROM album_jobs j
	LEFT JOIN candidates a ON a.id = (
		SELECT id FROM candidates WHERE album_job_id = j.id ORDER BY created_at DESC, id DESC LIMIT 1
	)
	LEFT JOIN transfers t ON t.id = (
		SELECT id FROM transfers WHERE candidate_id = a.id ORDER BY updated_at DESC, id DESC LIMIT 1
	)
	LEFT JOIN LATERAL (
		SELECT
			COALESCE(SUM(bytes_done), 0)  AS bytes_done,
			COALESCE(SUM(bytes_total), 0) AS bytes_total,
			-- Remaining excludes only the three terminal states (core.TransferCompleted,
			-- core.TransferErrored, core.TransferCancelled), expressed as NOT IN so any
			-- future non-terminal state counts as remaining by default rather than
			-- silently dropping out. PENDING/QUEUED/IN_PROGRESS/STALLED all count:
			-- STALLED can still recover or be retried.
			-- This literal is hardcoded, unlike every other transfer-state bind in
			-- this package (e.g. string(core.TransferPending)). The reason is that
			-- jobViewSelect is a const prefix its callers concatenate their own WHERE
			-- clause onto, so the placeholder numbering space is shared: binding it
			-- here would claim $1-$3 and shift every caller's own params (see the
			-- StateCancelled and jobID binds below, both currently $1).
			COALESCE(SUM(GREATEST(bytes_total - bytes_done, 0))
				FILTER (WHERE state NOT IN ('COMPLETED', 'ERRORED', 'CANCELLED')), 0) AS bytes_remaining
		FROM transfers
		WHERE candidate_id = a.id
	) agg ON true`

func scanJobView(r rowScanner) (core.JobView, error) {
	var v core.JobView
	var jState, jSource string
	var tID sql.NullInt64
	var tCandidateID sql.NullInt64
	var tSlskdID, tUsername, tFilename, tState sql.NullString
	var tBytesDone, tBytesTotal sql.NullInt64
	var tDeadline, tLastProgressAt, tUpdatedAt sql.NullTime
	var aID, aAlbumJobID sql.NullInt64
	var aUsername, aState, aFailReason sql.NullString
	var aScore sql.NullFloat64
	var aCreatedAt, aUpdatedAt sql.NullTime
	var aFiles []byte
	var jYear, jTracks sql.NullInt64
	var jFormat sql.NullString

	err := r.Scan(
		&v.Job.ID, &v.Job.LidarrAlbumID, &jState, &v.Job.CandidatesTried, &v.Job.NextAttemptAt, &v.Job.CreatedAt, &v.Job.UpdatedAt, &v.Job.Title, &v.Job.ArtistName, &v.Job.Retries, &v.Job.NotBefore, &v.Job.FailedAt, &jSource, &jYear, &jTracks, &jFormat,
		&tID, &tCandidateID, &tSlskdID, &tUsername, &tFilename, &tState, &tBytesDone, &tBytesTotal, &tDeadline, &tLastProgressAt, &tUpdatedAt,
		&aID, &aAlbumJobID, &aUsername, &aScore, &aState, &aFailReason, &aCreatedAt, &aUpdatedAt, &aFiles,
		&v.AlbumBytesDone, &v.AlbumBytesTotal, &v.AlbumBytesRemaining,
	)
	if err != nil {
		return core.JobView{}, err
	}
	v.Job.State = core.AlbumJobState(jState)
	v.Job.Source = core.JobSource(jSource)
	if jYear.Valid {
		y := int(jYear.Int64)
		v.Job.Year = &y
	}
	if jTracks.Valid {
		t := int(jTracks.Int64)
		v.Job.Tracks = &t
	}
	if jFormat.Valid {
		f := jFormat.String
		v.Job.Format = &f
	}

	if tID.Valid {
		tr := &core.Transfer{
			ID:          tID.Int64,
			CandidateID: tCandidateID.Int64,
			SlskdID:     tSlskdID.String,
			Username:    tUsername.String,
			Filename:    tFilename.String,
			State:       core.TransferState(tState.String),
			BytesDone:   tBytesDone.Int64,
			BytesTotal:  tBytesTotal.Int64,
			Deadline:    tDeadline.Time,
			UpdatedAt:   tUpdatedAt.Time,
		}
		if tLastProgressAt.Valid {
			lp := tLastProgressAt.Time
			tr.LastProgressAt = &lp
		}
		v.Transfer = tr
		v.Peer = tUsername.String
	}

	if aID.Valid {
		attempt := &core.Candidate{
			ID:         aID.Int64,
			AlbumJobID: aAlbumJobID.Int64,
			Username:   aUsername.String,
			Score:      aScore.Float64,
			State:      core.CandidateState(aState.String),
			FailReason: aFailReason.String,
			CreatedAt:  aCreatedAt.Time,
			UpdatedAt:  aUpdatedAt.Time,
		}
		if len(aFiles) > 0 {
			if err := json.Unmarshal(aFiles, &attempt.Files); err != nil {
				return core.JobView{}, fmt.Errorf("unmarshal candidate files: %w", err)
			}
		}
		v.Attempt = attempt
	}
	return v, nil
}

// ListJobsWithTransfer returns every non-cancelled album job joined with its
// most recent transfer, newest job first. Used by the dashboard's Queue view.
func (s *Store) ListJobsWithTransfer(ctx context.Context) ([]core.JobView, error) {
	rows, err := s.db.QueryContext(ctx, jobViewSelect+` WHERE j.state != $1 ORDER BY j.updated_at DESC`, string(core.StateCancelled))
	if err != nil {
		return nil, fmt.Errorf("list jobs with transfer: %w", err)
	}
	defer rows.Close()

	var out []core.JobView
	for rows.Next() {
		v, err := scanJobView(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job view: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DashboardJobsPageSize is the fixed number of jobs returned by one dashboard page.
const DashboardJobsPageSize int64 = 12

// DashboardJobsQuery is the validated, persisted-only query used by the
// dashboard's paged REST endpoint. Live transfer data must never be threaded
// into this query because it would make page membership move between polls.
type DashboardJobsQuery struct {
	Page   int64
	Sort   string
	Dir    string
	Filter string
	Source string
	Query  string
}

// DashboardStatusFacets contains counts for each dashboard status. All ignores
// the selected status filter while the individual counts use the same q and
// source constraints as All.
type DashboardStatusFacets struct {
	All       int64
	Active    int64
	Importing int64
	Queued    int64
	Stalled   int64
	Failed    int64
	Parked    int64
	Done      int64
}

// DashboardSourceFacets contains counts for each persisted job source. All
// ignores the selected source while respecting q and status.
type DashboardSourceFacets struct {
	All    int64
	Manual int64
	Lidarr int64
}

// DashboardJobsFacets contains the independent status and source facets.
type DashboardJobsFacets struct {
	Status DashboardStatusFacets
	Source DashboardSourceFacets
}

// DashboardJobsPage is one persisted dashboard page plus the matching total
// and independent facet counts.
type DashboardJobsPage struct {
	Jobs   []core.JobView
	Total  int64
	Facets DashboardJobsFacets
}

const dashboardJobStatusSQL = `CASE
	WHEN j.state IN ('DONE', 'COMPLETED') THEN 'done'
	WHEN j.state = 'FAILED' THEN 'failed'
	WHEN j.state IN ('PARKED', 'ORPHANED') THEN 'parked'
	WHEN j.state = 'IMPORTING' THEN 'importing'
	WHEN j.state IN ('WANTED', 'SELECTING') THEN 'queued'
	WHEN t.state = 'STALLED' THEN 'stalled'
	WHEN t.state = 'IN_PROGRESS' THEN 'active'
	WHEN t.state IN ('ERRORED', 'CANCELLED') THEN 'failed'
	ELSE 'queued'
END`

const dashboardJobCountFrom = `
	FROM album_jobs j
	LEFT JOIN candidates a ON a.id = (
		SELECT id FROM candidates WHERE album_job_id = j.id ORDER BY created_at DESC, id DESC LIMIT 1
	)
	LEFT JOIN transfers t ON t.id = (
		SELECT id FROM transfers WHERE candidate_id = a.id ORDER BY updated_at DESC, id DESC LIMIT 1
	)`

func validateDashboardJobsQuery(q DashboardJobsQuery) error {
	if q.Page < 0 || q.Page > (int64(^uint64(0)>>1)/DashboardJobsPageSize) {
		return fmt.Errorf("invalid dashboard jobs page %d", q.Page)
	}
	switch q.Sort {
	case "st", "album", "peer", "try":
	default:
		return fmt.Errorf("invalid dashboard jobs sort %q", q.Sort)
	}
	switch q.Dir {
	case "asc", "desc":
	default:
		return fmt.Errorf("invalid dashboard jobs direction %q", q.Dir)
	}
	switch q.Filter {
	case "all", "active", "importing", "queued", "stalled", "failed", "parked", "done":
	default:
		return fmt.Errorf("invalid dashboard jobs filter %q", q.Filter)
	}
	switch q.Source {
	case "all", "manual", "lidarr":
	default:
		return fmt.Errorf("invalid dashboard jobs source %q", q.Source)
	}
	return nil
}

func dashboardJobsWhere(q DashboardJobsQuery, includeStatus, includeSource bool) (string, []any) {
	clauses := []string{"j.state != $1"}
	args := []any{string(core.StateCancelled)}
	bind := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if q.Query != "" {
		placeholder := bind(q.Query)
		clauses = append(clauses, "(strpos(lower(j.artist_name), lower("+placeholder+")) > 0 OR strpos(lower(j.title), lower("+placeholder+")) > 0 OR strpos(lower(COALESCE(t.username, '')), lower("+placeholder+")) > 0)")
	}
	if includeStatus && q.Filter != "all" {
		clauses = append(clauses, "("+dashboardJobStatusSQL+") = "+bind(q.Filter))
	}
	if includeSource && q.Source != "all" {
		clauses = append(clauses, "j.source = "+bind(q.Source))
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func dashboardJobsOrder(q DashboardJobsQuery) string {
	direction := "ASC"
	if q.Dir == "desc" {
		direction = "DESC"
	}
	switch q.Sort {
	case "st":
		return ` ORDER BY CASE (` + dashboardJobStatusSQL + `)
			WHEN 'active' THEN 1 WHEN 'importing' THEN 2 WHEN 'queued' THEN 3
			WHEN 'stalled' THEN 4 WHEN 'failed' THEN 5 WHEN 'parked' THEN 6
			WHEN 'done' THEN 7 ELSE 8 END ` + direction + `, j.id ASC`
	case "album":
		return " ORDER BY lower(j.title) " + direction + ", lower(j.artist_name) " + direction + ", j.id ASC"
	case "peer":
		return " ORDER BY (NULLIF(t.username, '') IS NULL) ASC, t.username " + direction + ", j.id ASC"
	case "try":
		return " ORDER BY j.retries " + direction + ", j.id ASC"
	default:
		panic("dashboardJobsOrder called without validation")
	}
}

// ListDashboardJobs returns one persisted-only page, total, and facets from a
// repeatable-read snapshot. This keeps the page and its counts mutually
// consistent even while pipeline modules update jobs concurrently.
func (s *Store) ListDashboardJobs(ctx context.Context, q DashboardJobsQuery) (DashboardJobsPage, error) {
	if err := validateDashboardJobsQuery(q); err != nil {
		return DashboardJobsPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: begin: %w", err)
	}
	defer tx.Rollback()

	statusWhere, statusArgs := dashboardJobsWhere(q, false, true)
	statusSQL := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE status = 'active'),
		COUNT(*) FILTER (WHERE status = 'importing'),
		COUNT(*) FILTER (WHERE status = 'queued'),
		COUNT(*) FILTER (WHERE status = 'stalled'),
		COUNT(*) FILTER (WHERE status = 'failed'),
		COUNT(*) FILTER (WHERE status = 'parked'),
		COUNT(*) FILTER (WHERE status = 'done')
		FROM (SELECT ` + dashboardJobStatusSQL + ` AS status` + dashboardJobCountFrom + statusWhere + `) dashboard_jobs`
	var page DashboardJobsPage
	if err := tx.QueryRowContext(ctx, statusSQL, statusArgs...).Scan(
		&page.Facets.Status.All, &page.Facets.Status.Active, &page.Facets.Status.Importing,
		&page.Facets.Status.Queued, &page.Facets.Status.Stalled, &page.Facets.Status.Failed,
		&page.Facets.Status.Parked, &page.Facets.Status.Done,
	); err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: status facets: %w", err)
	}

	sourceWhere, sourceArgs := dashboardJobsWhere(q, true, false)
	sourceSQL := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE source = 'manual'),
		COUNT(*) FILTER (WHERE source = 'lidarr')
		FROM (SELECT j.source AS source` + dashboardJobCountFrom + sourceWhere + `) dashboard_jobs`
	if err := tx.QueryRowContext(ctx, sourceSQL, sourceArgs...).Scan(
		&page.Facets.Source.All, &page.Facets.Source.Manual, &page.Facets.Source.Lidarr,
	); err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: source facets: %w", err)
	}

	where, args := dashboardJobsWhere(q, true, true)
	countSQL := `SELECT COUNT(*)` + dashboardJobCountFrom + where
	if err := tx.QueryRowContext(ctx, countSQL, args...).Scan(&page.Total); err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: total: %w", err)
	}

	args = append(args, DashboardJobsPageSize, q.Page*DashboardJobsPageSize)
	rows, err := tx.QueryContext(ctx, jobViewSelect+where+dashboardJobsOrder(q)+fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: page: %w", err)
	}
	page.Jobs = make([]core.JobView, 0, DashboardJobsPageSize)
	for rows.Next() {
		view, scanErr := scanJobView(rows)
		if scanErr != nil {
			rows.Close()
			return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: scan: %w", scanErr)
		}
		page.Jobs = append(page.Jobs, view)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: close rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: commit: %w", err)
	}
	return page, nil
}

// JobWithTransfer looks up a single job (regardless of state) with its most
// recent transfer for dashboard-facing actions and detail. It is a one-row
// projection, not a lifecycle work list. found is false if no job has that id.
func (s *Store) JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error) {
	row := s.db.QueryRowContext(ctx, jobViewSelect+` WHERE j.id = $1`, jobID)

	v, err := scanJobView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.JobView{}, false, nil
	}
	if err != nil {
		return core.JobView{}, false, fmt.Errorf("job with transfer: %w", err)
	}
	return v, true, nil
}

// JobDetail returns a job plus every candidate made for it (newest first) and
// each candidate's per-file transfers, for the dashboard's per-job detail
// panel (GET /api/jobs/{id}/detail). found is false if no job has that id.
// Built from CandidatesForJob/TransfersForCandidate (one query per candidate)
// rather than a single join, since the number of candidates per job is small
// (bounded by MaxCandidatesPerAlbum) and this reuses the existing read paths
// rather than a bespoke wide query.
func (s *Store) JobDetail(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
	var job core.AlbumJob
	var state, source string
	err := s.db.QueryRowContext(ctx,
		jobSelect+` WHERE id = $1`, jobID).
		Scan(&job.ID, &job.LidarrAlbumID, &state, &job.CandidatesTried, &job.NextAttemptAt, &job.CreatedAt, &job.UpdatedAt, &job.Title, &job.ArtistName, &job.ReleaseDate, &job.ArtistID, &job.Retries, &job.NotBefore, &job.FailedAt, &job.MinTrackCount, &job.MaxTrackCount, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return core.JobDetail{}, false, nil
	}
	if err != nil {
		return core.JobDetail{}, false, fmt.Errorf("job detail: read job: %w", err)
	}
	job.State = core.AlbumJobState(state)
	job.Source = core.JobSource(source)

	candidates, err := s.CandidatesForJob(ctx, jobID) // oldest first
	if err != nil {
		return core.JobDetail{}, false, fmt.Errorf("job detail: candidates: %w", err)
	}
	details := make([]core.AttemptDetail, len(candidates))
	for i, c := range candidates {
		transfers, err := s.TransfersForCandidate(ctx, c.ID)
		if err != nil {
			return core.JobDetail{}, false, fmt.Errorf("job detail: transfers for candidate %d: %w", c.ID, err)
		}
		details[i] = core.AttemptDetail{Attempt: c, Transfers: transfers}
	}
	// CandidatesForJob returns oldest first; the detail panel wants newest first.
	for i, j := 0, len(details)-1; i < j; i, j = i+1, j-1 {
		details[i], details[j] = details[j], details[i]
	}
	return core.JobDetail{Job: job, Attempts: details}, true, nil
}

// TransferBytesByCandidate returns each candidate's per-file persisted
// bytes-done, keyed by candidate id then filename, for exactly the given
// candidate ids — never the whole transfers table. It exists for
// internal/observ's live-bytes overlay (issue #161's backwards-jump/freeze
// fix): a job whose current candidate has at least one file with a live
// in-memory match still needs the OTHER, unmatched files' persisted bytes to
// build a correct album total, and jobViewSelect's own per-candidate
// aggregate doesn't break bytes out per file. Callers should only pass
// candidate ids that actually have a live match (see the observ package's
// anyLiveMatch) — a candidate with none needs no query at all, since its
// per-file sum trivially equals the AlbumBytesDone jobViewSelect already
// computed. An empty or nil ids yields an empty, non-nil map.
func (s *Store) TransferBytesByCandidate(ctx context.Context, candidateIDs []int64) (map[int64]map[string]int64, error) {
	out := make(map[int64]map[string]int64, len(candidateIDs))
	if len(candidateIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT candidate_id, filename, bytes_done FROM transfers WHERE candidate_id = ANY($1)`,
		candidateIDs)
	if err != nil {
		return nil, fmt.Errorf("transfer bytes by candidate: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var candidateID, bytesDone int64
		var filename string
		if err := rows.Scan(&candidateID, &filename, &bytesDone); err != nil {
			return nil, fmt.Errorf("transfer bytes by candidate: scan: %w", err)
		}
		byFilename, ok := out[candidateID]
		if !ok {
			byFilename = make(map[string]int64)
			out[candidateID] = byFilename
		}
		byFilename[filename] = bytesDone
	}
	return out, rows.Err()
}

// Peers returns every known Soulseek peer's global reliability plus their
// per-artist rows, for the dashboard's Peers view (GET /api/peers). Ordered by
// username for determinism; the dashboard sorts client-side. Score computation
// (which needs "now" for decay) is left to the caller — see
// matcher.ReliabilityHistoryScore — so this stays a plain read.
func (s *Store) Peers(ctx context.Context) ([]core.PeerRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, success_count, fail_count, last_success_at, last_fail_at
		 FROM known_users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("peers: query known_users: %w", err)
	}
	defer rows.Close()

	byUsername := map[string]*core.PeerRow{}
	var order []string
	for rows.Next() {
		var p core.PeerRow
		if err := rows.Scan(&p.Username, &p.Global.SuccessCount, &p.Global.FailCount, &p.Global.LastSuccessAt, &p.Global.LastFailAt); err != nil {
			return nil, fmt.Errorf("peers: scan known_users: %w", err)
		}
		byUsername[p.Username] = &p
		order = append(order, p.Username)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	artistRows, err := s.db.QueryContext(ctx,
		`SELECT ku.username, aur.artist_id, aur.success_count, aur.fail_count, aur.last_success_at, aur.last_fail_at
		 FROM artist_user_reliability aur JOIN known_users ku ON ku.id = aur.user_id`)
	if err != nil {
		return nil, fmt.Errorf("peers: query artist_user_reliability: %w", err)
	}
	defer artistRows.Close()
	for artistRows.Next() {
		var username string
		var artistID int64
		var c core.ReliabilityCounters
		if err := artistRows.Scan(&username, &artistID, &c.SuccessCount, &c.FailCount, &c.LastSuccessAt, &c.LastFailAt); err != nil {
			return nil, fmt.Errorf("peers: scan artist_user_reliability: %w", err)
		}
		p, ok := byUsername[username]
		if !ok {
			continue
		}
		if p.Artists == nil {
			p.Artists = map[int64]core.ReliabilityCounters{}
		}
		p.Artists[artistID] = c
	}
	if err := artistRows.Err(); err != nil {
		return nil, err
	}

	out := make([]core.PeerRow, 0, len(order))
	for _, u := range order {
		out = append(out, *byUsername[u])
	}
	return out, nil
}
