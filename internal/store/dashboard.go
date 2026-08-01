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
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// currentCandidateOrder is the ORDER BY that picks the one candidate of
// album_jobs j that represents the job everywhere the dashboard reads a
// "current" candidate: its state and progress, its peer, and (via
// jobViewFrom's agg lateral) the job's dashboard status.
//
// The naive "most recently created" ordering (created_at DESC, id DESC) looks
// correct but is not: InsertCandidates writes every candidate of one search
// pass in a single batch, so created_at is identical across them and id DESC
// is the real tiebreak. NextNewCandidate tries candidates best-score-first
// (ORDER BY score DESC, id ASC), so id DESC deterministically selects the
// WORST-ranked candidate — usually one that was never attempted and has zero
// transfers (issue #269).
//
// Instead:
//   - an ACTIVE candidate always wins. A job has at most one at a time
//     (see Store.ActiveCandidate), and it stays ACTIVE through both
//     DOWNLOADING and IMPORTING.
//   - otherwise, prefer a candidate that was actually attempted: NEW sorts
//     last, so a never-tried candidate can never displace a finished job's
//     real SUCCEEDED/FAILED one.
//   - updated_at DESC, id DESC break any remaining tie deterministically.
//
// One consequence is deliberate: FailCandidateAndAdvance returns a job to
// SELECTING without deleting the failed candidate's transfers, so a job
// between attempts points at that FAILED candidate and reports its peer and
// its partial bytes. The peer reads as "last attempted", which is more useful
// than the blank the deleted transfer join produced. The bytes are not — a
// dead attempt's progress must not render as the job's own, so the jobs view
// zeroes it for queued rows (see noProgress in web/src/routes/Jobs.tsx).
const currentCandidateOrder = `ORDER BY (state = 'ACTIVE') DESC, (state = 'NEW') ASC, updated_at DESC, id DESC`

// jobViewFrom is the FROM clause shared by every dashboard read of
// album_jobs: the row projection (jobViewSelect) and the count/facet/filter/
// sort queries (ListDashboardJobs) all join through it, so they can never
// disagree about which candidate or which transfer aggregate a job's status
// is computed from.
//
//   - a is the job's current candidate, a LEFT JOIN LATERAL that returns the
//     candidate row directly rather than joining on a scalar subquery's id:
//     the earlier `a.id = (SELECT id FROM candidates WHERE ... ORDER BY ...
//     LIMIT 1)` form made Postgres hash-build all of candidates and evaluate
//     the correlated subplan roughly twice per outer row (once for the hash
//     probe key, once to recheck the predicate after a hash match) — issue
//     #286. The LATERAL is a single nested loop instead: one index-scan-plus-
//     limit per outer row. currentCandidateOrder is the tiebreak this
//     LATERAL orders by; see its own doc comment. Earlier versions of this
//     view additionally joined the candidate's single most recently updated
//     transfer (aliased t) to answer "is this job stalled/active/failed" and
//     to supply JobView.Peer — but a transfer belongs to one file, and "most
//     recently updated" has no relationship to whether that file, or the
//     album as a whole, is actually progressing (issue #269). That join is
//     gone: agg below aggregates every transfer of the candidate instead, and
//     Peer is a.username — the candidate's own peer, unambiguous without
//     picking a single file.
//   - agg aggregates every transfer of the candidate: summed bytes (issue
//     #174; ActivateCandidateWithTransfers and CreateManualJob insert a
//     transfers row for every file of the album upfront, state PENDING with
//     bytes_total from the file size, so the sum is already album-complete
//     even for files the per-peer throttle (#20) hasn't released to the peer
//     backend yet — see core.JobView.AlbumBytes*) and per-state counts used
//     by dashboardJobStatusSQL to decide active/stalled/failed from the
//     aggregate rather than one arbitrarily-chosen row.
//
// A job with no candidates yet still appears, with NULL candidate columns and
// agg's COALESCE-guarded zeros. Callers append their own WHERE clause.
const jobViewFrom = `
	FROM album_jobs j
	LEFT JOIN LATERAL (
		SELECT id, album_job_id, username, score, state, fail_reason, created_at, updated_at, files
		FROM candidates WHERE album_job_id = j.id
		` + currentCandidateOrder + `
		LIMIT 1
	) a ON true
	LEFT JOIN LATERAL (
		SELECT
			COALESCE(SUM(bytes_done), 0)  AS bytes_done,
			COALESCE(SUM(bytes_total), 0) AS bytes_total,
			-- Remaining excludes only the three terminal states (core.TransferCompleted,
			-- core.TransferErrored, core.TransferCancelled), expressed as NOT IN so any
			-- future non-terminal state counts as remaining by default rather than
			-- silently dropping out. PENDING/QUEUED/IN_PROGRESS/STALLED all count:
			-- STALLED can still recover or be retried.
			-- These literals are hardcoded, unlike every other transfer-state bind
			-- in this package (e.g. string(core.TransferPending)). The reason is
			-- that jobViewFrom is a const prefix its callers concatenate their own
			-- WHERE clause onto, so the placeholder numbering space is shared:
			-- binding them here would claim the leading placeholders and shift
			-- every caller's own params (see the StateCancelled and jobID binds
			-- below, both currently $1).
			COALESCE(SUM(GREATEST(bytes_total - bytes_done, 0))
				FILTER (WHERE state NOT IN ('COMPLETED', 'ERRORED', 'CANCELLED')), 0) AS bytes_remaining,
			-- The four counters dashboardJobStatusSQL reads. Deriving status from
			-- an aggregate over every transfer of the candidate (rather than one
			-- arbitrarily-chosen row) is the fix for issue #269: a candidate with
			-- ten files where only one is IN_PROGRESS and the rest are still
			-- PENDING is "active", not whatever state the pointed-at row happened
			-- to be in.
			COUNT(*) FILTER (WHERE state = 'IN_PROGRESS')                              AS in_progress,
			COUNT(*) FILTER (WHERE state = 'STALLED')                                  AS stalled,
			COUNT(*) FILTER (WHERE state NOT IN ('COMPLETED', 'ERRORED', 'CANCELLED')) AS live,
			COUNT(*) FILTER (WHERE state IN ('ERRORED', 'CANCELLED'))                  AS failed
		FROM transfers
		WHERE candidate_id = a.id
	) agg ON true`

// dashboardJobStatusSQL is the single source of truth for a job's dashboard
// display status — selected as core.JobView.Status by jobViewSelect and
// reused by every filter/facet/sort query below, so a job's rendered status
// and the counts/ordering built around it can never drift apart (issue
// #269 — the Go copy of this rule, observ.dashboardStatus, is gone; it had
// already drifted from this one over IMPORTING).
//
// Job-level states are checked first because they're unambiguous regardless
// of any transfer activity. SELECTING is deliberately mapped to 'queued', not
// 'active': it's the pipeline's waiting room — candidates are cached but the
// job is waiting for a MaxActive slot (the cap only counts
// DOWNLOADING+IMPORTING). Counting it as active made the dashboard's Aktiv
// figure grow past max_active, which read like a broken cap.
//
// Only once none of those apply does the CASE fall through to agg (see
// jobViewFrom), which aggregates every transfer of the job's current
// candidate rather than reading one arbitrarily-chosen row: any file actually
// moving makes the job 'active', any file stalled (with none in progress)
// makes it 'stalled', and a candidate whose transfers are all terminal with
// at least one failure reports 'failed'. A DOWNLOADING job with no transfer
// activity yet (agg.live > 0 but nothing in progress or errored, e.g. all
// still PENDING) falls through to 'queued' — nothing is happening yet.
const dashboardJobStatusSQL = `CASE
	WHEN j.state IN ('DONE', 'COMPLETED') THEN 'done'
	WHEN j.state = 'FAILED' THEN 'failed'
	WHEN j.state IN ('PARKED', 'ORPHANED') THEN 'parked'
	WHEN j.state = 'IMPORTING' THEN 'importing'
	-- A manual job (issue #59) whose download completed with no Lidarr album
	-- to import into. Terminal and deliberately NOT 'failed' or 'done': the
	-- download itself succeeded, but there is nothing to report as
	-- imported. Checked alongside the other job-level states above, before
	-- any transfer-aggregate fallback applies.
	WHEN j.state = 'NOT_IMPORTED' THEN 'notImported'
	WHEN j.state IN ('WANTED', 'SELECTING') THEN 'queued'
	WHEN agg.in_progress > 0 THEN 'active'
	WHEN agg.stalled > 0 THEN 'stalled'
	WHEN agg.live = 0 AND agg.failed > 0 THEN 'failed'
	ELSE 'queued'
END`

// jobViewSelect projects one row per album_job (see jobViewFrom for the
// joins) plus a.files, included so the observ package can aggregate
// album-level live speed/ETA across every file of the candidate rather than
// just a single transfer (issue #157) — see core.Candidate.Files. Callers
// append their own WHERE clause.
const jobViewSelect = `
	SELECT
		j.id, COALESCE(j.lidarr_album_id, 0), j.state, j.candidates_tried, j.next_attempt_at, j.created_at, j.updated_at, j.title, j.artist_name, j.retries, j.empty_searches, j.not_before, j.failed_at, j.source, j.year, j.tracks, j.format, j.album_mbid,
		a.id, a.album_job_id, a.username, a.score, a.state, a.fail_reason, a.created_at, a.updated_at, a.files,
		agg.bytes_done, agg.bytes_total, agg.bytes_remaining,
		(` + dashboardJobStatusSQL + `) AS status
	` + jobViewFrom

func scanJobView(r rowScanner) (core.JobView, error) {
	var v core.JobView
	var jState, jSource string
	var aID, aAlbumJobID sql.NullInt64
	var aUsername, aState, aFailReason sql.NullString
	var aScore sql.NullFloat64
	var aCreatedAt, aUpdatedAt sql.NullTime
	var aFiles []byte
	var jYear, jTracks sql.NullInt64
	var jFormat sql.NullString

	err := r.Scan(
		&v.Job.ID, &v.Job.LidarrAlbumID, &jState, &v.Job.CandidatesTried, &v.Job.NextAttemptAt, &v.Job.CreatedAt, &v.Job.UpdatedAt, &v.Job.Title, &v.Job.ArtistName, &v.Job.Retries, &v.Job.EmptySearches, &v.Job.NotBefore, &v.Job.FailedAt, &jSource, &jYear, &jTracks, &jFormat, &v.Job.AlbumMBID,
		&aID, &aAlbumJobID, &aUsername, &aScore, &aState, &aFailReason, &aCreatedAt, &aUpdatedAt, &aFiles,
		&v.AlbumBytesDone, &v.AlbumBytesTotal, &v.AlbumBytesRemaining,
		&v.Status,
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
		v.Peer = aUsername.String
	}
	return v, nil
}

// ListJobsWithTransfer returns every non-cancelled album job with its current
// candidate and that candidate's aggregated transfer progress (see
// jobViewFrom), newest job first. Used by the dashboard's Queue view.
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

// DashboardJobsPageSize is the default number of jobs returned by one
// dashboard page when a query doesn't specify DashboardJobsQuery.PageSize
// (issue #268 made page size a bounded, per-request choice — see PageSize
// and ListDashboardJobs).
const DashboardJobsPageSize int64 = 12

// DashboardFinishedWindow is how far back filter=finished looks: a job counts
// as recently finished when its updated_at falls inside this window. It is a
// constant rather than configuration deliberately — internal/config rejects
// unknown keys and a merge to main deploys straight to production, so a new
// required key could stop the container from starting, and this number is not
// yet known to be wrong (see the design spec's "Beslut som avvägts").
//
// album_jobs.updated_at is a trustworthy completion stamp for DONE and FAILED:
// MarkJobFailed is guarded against re-failing an already-terminal job, and the
// metadata backfill in SyncWantedJobs deliberately leaves updated_at alone for
// jobs past WANTED. WANTED jobs *do* get a fresh updated_at on every sync pass,
// which is why this filter can never include them.
const DashboardFinishedWindow = time.Hour

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
	// PageSize is how many jobs one page holds, bounded to [1, 50] by
	// validateDashboardJobsQuery (issue #268). A zero value (the type's own
	// zero value, e.g. from a caller or test predating #268 that never set
	// it) is treated as "unset" by ListDashboardJobs and defaults to
	// DashboardJobsPageSize, rather than failing validation on a bound
	// nothing chose.
	PageSize int64
	// Now anchors filter=finished's window (see DashboardFinishedWindow) and is
	// required only for that filter — validateDashboardJobsQuery rejects a zero
	// value there and ignores it everywhere else. Threaded in rather than read
	// from the database's now() so tests are independent of the wall clock,
	// matching how every other time-dependent store method takes its `now`.
	Now time.Time
	// SkipFacets omits the status/source facet queries and the total count,
	// leaving DashboardJobsPage.Total and .Facets at their zero values. The
	// facet query evaluates dashboardJobStatusSQL over every non-cancelled row
	// — measured at ~85ms warm against production (5183 album_jobs, 15716
	// candidates, 74174 transfers, see issue #286) — and it runs regardless of
	// which filter was asked for, since facets deliberately ignore the status
	// filter. A caller that renders neither a total nor facet chips should not
	// pay for them. Callers that do read them must leave this false.
	SkipFacets bool
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

func validateDashboardJobsQuery(q DashboardJobsQuery) error {
	// Checked before Page's own overflow guard, which divides by it.
	if q.PageSize < 1 || q.PageSize > 50 {
		return fmt.Errorf("invalid dashboard jobs page size %d", q.PageSize)
	}
	if q.Page < 0 || q.Page > (int64(^uint64(0)>>1)/q.PageSize) {
		return fmt.Errorf("invalid dashboard jobs page %d", q.Page)
	}
	switch q.Sort {
	case "st", "album", "peer", "try", "transfer", "recent":
	default:
		return fmt.Errorf("invalid dashboard jobs sort %q", q.Sort)
	}
	switch q.Dir {
	case "asc", "desc":
	default:
		return fmt.Errorf("invalid dashboard jobs direction %q", q.Dir)
	}
	// sort=transfer's whole purpose is a stable status-group-then-age
	// ranking (see dashboardJobsOrder) — reversing it is not a meaningful
	// alternative order, so it's rejected rather than silently reinterpreted.
	if q.Sort == "transfer" && q.Dir == "desc" {
		return fmt.Errorf("dir=desc is not supported for dashboard jobs sort %q", q.Sort)
	}
	// sort=recent is a newest-first ranking; ascending would be "oldest
	// finished first", which no caller wants and which would silently turn
	// Overview's recently-finished panel into its opposite. Rejected rather
	// than reinterpreted, the same treatment sort=transfer gets above.
	if q.Sort == "recent" && q.Dir == "asc" {
		return fmt.Errorf("dir=asc is not supported for dashboard jobs sort %q", q.Sort)
	}
	switch q.Filter {
	// "failed" here is the status-derived filter that falls through to
	// dashboardJobsWhere's default case — it is NOT the same set as
	// "failures" below; see that case's comment for why the two must never
	// be merged.
	case "all", "active", "importing", "queued", "stalled", "failed", "parked", "done", "inflight", "finished", "failures":
	default:
		return fmt.Errorf("invalid dashboard jobs filter %q", q.Filter)
	}
	if q.Filter == "finished" && q.Now.IsZero() {
		return fmt.Errorf("dashboard jobs filter %q requires a non-zero Now", q.Filter)
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
		clauses = append(clauses, "(strpos(lower(j.artist_name), lower("+placeholder+")) > 0 OR strpos(lower(j.title), lower("+placeholder+")) > 0 OR strpos(lower(COALESCE(a.username, '')), lower("+placeholder+")) > 0)")
	}
	if includeStatus && q.Filter != "all" {
		switch q.Filter {
		case "inflight":
			// Everything the pipeline currently holds a MaxActive slot for
			// (issue #287, Overview's TRANSFERS panel). Deliberately keyed on
			// j.state, not on dashboardJobStatusSQL: a DOWNLOADING job whose
			// transfers all errored reports status 'failed' while still being
			// in flight, so a status-keyed predicate would put it in this
			// region AND in 'finished' at the same time. A job has exactly one
			// state, which makes the two regions disjoint by construction.
			clauses = append(clauses, "j.state IN ("+bind(string(core.StateDownloading))+", "+bind(string(core.StateImporting))+")")
		case "finished":
			// Terminal in the pipeline sense and recent (issue #287, Overview's
			// recently-finished panel). Keyed on j.state for the same
			// disjointness reason as inflight above.
			//
			// The membership rule is "the job just stopped, and something
			// happened when it did" — which is narrower than
			// AlbumJobState.PipelineTerminal(), so this list is maintained by
			// hand rather than derived from it. Two states are terminal and
			// still excluded, each for its own reason: PARKED can sit for
			// days, so its updated_at reads as fresh without anything having
			// just happened; CANCELLED means the album left Lidarr's wanted
			// list, which is a removal rather than a finish. NOT_IMPORTED
			// (issue #59) is included: the download genuinely completed and
			// its updated_at marks that moment, so a manual job that finishes
			// with no Lidarr album to import into must still surface here —
			// omitting it would make a successful download vanish from the
			// one panel the user watches for completions.
			clauses = append(clauses,
				"j.state IN ("+bind(string(core.StateDone))+", "+bind(string(core.StateFailed))+
					", "+bind(string(core.StateNotImported))+")"+
					" AND j.updated_at > "+bind(q.Now.Add(-DashboardFinishedWindow)))
		case "failures":
			// Overview's FAILED panel (issue #310/review follow-up). Keyed on
			// j.state, for the same disjointness reason as inflight/finished
			// above: dashboardJobStatusSQL's status-derived 'failed' (the
			// default case below) also matches a job still in DOWNLOADING
			// whose current candidate's transfers all errored and which the
			// pipeline will retry with the next candidate — that job belongs
			// in TRANSFERS, not here, and a status-keyed predicate would put
			// it in both at once. j.state = 'FAILED' is terminal and
			// unambiguous.
			//
			// Deliberately time-unbounded, unlike finished above: an
			// unresolved failure is still worth showing however old it is,
			// so this filter takes no Now and must not be added to the
			// finished/Now validation guard in validateDashboardJobsQuery.
			//
			// "failures" and "failed" are deliberately different predicates:
			// "failed" (the default case below) is the status-derived filter
			// the Jobs page's facets are counted from, which also matches the
			// mid-retry DOWNLOADING case above; "failures" is this
			// state-derived one. Do not merge them.
			clauses = append(clauses, "j.state = "+bind(string(core.StateFailed)))
		default:
			// q.Filter == "failed" lands here: it is status-derived
			// (dashboardJobStatusSQL's CASE), not the same set as "failures"
			// above — see that case's comment.
			clauses = append(clauses, "("+dashboardJobStatusSQL+") = "+bind(q.Filter))
		}
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
		return " ORDER BY (NULLIF(a.username, '') IS NULL) ASC, a.username " + direction + ", j.id ASC"
	case "try":
		return " ORDER BY j.retries " + direction + ", j.id ASC"
	case "transfer":
		// Overview's TRANSFERS panel (issue #268, formerly web/src/routes/jobSort.ts's
		// 'transferOrder'): status group first (active ranks above stalled,
		// everything else — including jobs the union filter never selects —
		// sorts last), THEN created_at ascending within a group. The group
		// comes before age deliberately: max_active sits well above the
		// panel's row count, so a stalled job (old by construction, and
		// staying that way) would otherwise pin a slot forever under pure
		// age ordering and hide active jobs that started later. A row only
		// moves when its status actually changes — real information — never
		// merely because it aged relative to another row.
		//
		// Direction is hardcoded ASC, never `direction`: validation rejects
		// dir=desc for this sort (see validateDashboardJobsQuery), since
		// reversing a ranking whose whole purpose is stability isn't a
		// meaningful alternative order. j.id ASC — never affected by dir —
		// is the tiebreaker: without one, Postgres' order for equal
		// (group, created_at) pairs is undefined, and the same job could
		// appear on two pages while another never shows at all.
		//
		// Four groups since issue #287 widened the panel's filter from the
		// active+stalled union to every in-flight job: a job waiting for more
		// files ('queued') and a job past download ('importing') used to be
		// unreachable here and both collapsed into the old ELSE, which made
		// their relative order fall out of created_at alone. They now rank
		// explicitly, in pipeline order — moving, stuck, waiting, importing.
		return ` ORDER BY CASE (` + dashboardJobStatusSQL + `)
			WHEN 'active' THEN 1 WHEN 'stalled' THEN 2 WHEN 'queued' THEN 3
			WHEN 'importing' THEN 4 ELSE 5 END ASC, j.created_at ASC, j.id ASC`
	case "recent":
		// Newest finish first, for Overview's recently-finished panel (issue
		// #287). Direction is hardcoded DESC, never `direction`: validation
		// rejects dir=asc. j.id DESC is the tiebreaker — two jobs finishing in
		// the same transaction share an updated_at, and without a tiebreaker
		// Postgres' order between them is undefined, so the same job could
		// appear on two pages while another never shows at all.
		return " ORDER BY j.updated_at DESC, j.id DESC"
	default:
		panic("dashboardJobsOrder called without validation")
	}
}

// ListDashboardJobs returns one persisted-only page, total, and facets from a
// repeatable-read snapshot. This keeps the page and its counts mutually
// consistent even while pipeline modules update jobs concurrently.
func (s *Store) ListDashboardJobs(ctx context.Context, q DashboardJobsQuery) (DashboardJobsPage, error) {
	if q.PageSize == 0 {
		// Unset (the zero value) rather than an explicit invalid choice —
		// every real caller (parsePagedJobsQuery) always sets a value in
		// [1, 50] before reaching here, so this only ever fires for a
		// caller/test that predates issue #268 and never mentioned
		// PageSize. Defaulting it keeps those callers valid rather than
		// failing validation on a bound nothing chose.
		q.PageSize = DashboardJobsPageSize
	}
	if err := validateDashboardJobsQuery(q); err != nil {
		return DashboardJobsPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: begin: %w", err)
	}
	defer tx.Rollback()

	var page DashboardJobsPage
	if !q.SkipFacets {
		statusWhere, statusArgs := dashboardJobsWhere(q, false, true)
		statusSQL := `SELECT COUNT(*),
			COUNT(*) FILTER (WHERE status = 'active'),
			COUNT(*) FILTER (WHERE status = 'importing'),
			COUNT(*) FILTER (WHERE status = 'queued'),
			COUNT(*) FILTER (WHERE status = 'stalled'),
			COUNT(*) FILTER (WHERE status = 'failed'),
			COUNT(*) FILTER (WHERE status = 'parked'),
			COUNT(*) FILTER (WHERE status = 'done')
			FROM (SELECT ` + dashboardJobStatusSQL + ` AS status` + jobViewFrom + statusWhere + `) dashboard_jobs`
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
			FROM (SELECT j.source AS source` + jobViewFrom + sourceWhere + `) dashboard_jobs`
		if err := tx.QueryRowContext(ctx, sourceSQL, sourceArgs...).Scan(
			&page.Facets.Source.All, &page.Facets.Source.Manual, &page.Facets.Source.Lidarr,
		); err != nil {
			return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: source facets: %w", err)
		}
	}

	where, args := dashboardJobsWhere(q, true, true)
	if !q.SkipFacets {
		countSQL := `SELECT COUNT(*)` + jobViewFrom + where
		if err := tx.QueryRowContext(ctx, countSQL, args...).Scan(&page.Total); err != nil {
			return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: total: %w", err)
		}
	}

	args = append(args, q.PageSize, q.Page*q.PageSize)
	rows, err := tx.QueryContext(ctx, jobViewSelect+where+dashboardJobsOrder(q)+fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return DashboardJobsPage{}, fmt.Errorf("list dashboard jobs: page: %w", err)
	}
	page.Jobs = make([]core.JobView, 0, q.PageSize)
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

// JobWithTransfer looks up a single job (regardless of state) with its current
// candidate and that candidate's aggregated transfer progress (see
// jobViewFrom) for dashboard-facing actions and detail. It is a one-row
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
		Scan(&job.ID, &job.LidarrAlbumID, &state, &job.CandidatesTried, &job.NextAttemptAt, &job.CreatedAt, &job.UpdatedAt, &job.Title, &job.ArtistName, &job.ReleaseDate, &job.ArtistID, &job.Retries, &job.EmptySearches, &job.NotBefore, &job.FailedAt, &job.MinTrackCount, &job.MaxTrackCount, &source, &job.AlbumMBID)
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

// failureExplainingEvents ranks job_events kinds that state a failure reason
// outright above every other kind. It is a preference, NOT a filter — see
// LatestFailureDetails for why it stopped being one.
//
// Built from the core.Event* constants (not string literals) so a renamed
// constant fails to compile rather than silently dropping out of the ranking.
// Declared as []string directly (rather than []core.JobEventType converted on
// every call) so LatestFailureDetails doesn't re-allocate a copy per call.
//
// EventJobFailed earns its place on intent rather than on current behaviour:
// the only writer is recordBackoffEvent (internal/pipeline/backoff.go), which
// hardcodes an empty detail, so the non-empty-detail test below can never
// match it today. It stays listed so that giving it a real detail (issue #318)
// needs no change here.
var failureExplainingEvents = []string{
	string(core.EventImportRejected),
	string(core.EventAttemptFailed),
	string(core.EventCandidateRejected),
	string(core.EventJobFailed),
}

// LatestFailureDetails returns one explanatory job_events detail per given job
// id, keyed by job id. It backs Overview's FAILED JOBS panel (issue #310): a
// job's own fail_reason/status is not enough to show a human a *reason*, since
// the audit trail — not the job row — is where that text (often Lidarr's
// verbatim rejection message) actually lives. Jobs with no detailed event at
// all are simply absent from the map, not present with "". An empty or nil
// jobIDs yields an empty, non-nil map.
//
// Selection is two-tier: an event from failureExplainingEvents always wins,
// even over a newer event of another kind, because a real rejection describes
// the failure better than whatever the pipeline happened to log afterwards.
// Only when a job has none does the newest detail of any kind get used.
//
// The fallback tier is not a nicety. #310 first shipped the allowlist as a
// hard filter, and on a real database that left the reason column empty for
// most rows: the dominant failure mode is a search that returns nothing, whose
// only record is a 'search' event ("results=0 candidates=0") plus a detail-less
// job_failed. 10 of 12 failed jobs in a lab run rendered an em dash while the
// reason sat in the audit trail one event-kind away. Restricting the tiers to
// a curated list of "runner-up" kinds was rejected as the same bet the
// allowlist already lost — any kind the pipeline gains later would silently
// fall out again.
func (s *Store) LatestFailureDetails(ctx context.Context, jobIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (album_job_id) album_job_id, detail
		FROM job_events
		WHERE album_job_id = ANY($1) AND detail <> ''
		-- The event test is the first ORDER BY key, not a WHERE clause: it
		-- ranks explanatory kinds above everything else while still leaving a
		-- lesser event to be picked when a job has nothing better. Making it a
		-- filter is what left most rows with no reason at all.
		-- id DESC is load-bearing, not cosmetic: one pipeline pass shares a
		-- single value of now across every recordEvent call it makes (see
		-- internal/pipeline), so multiple job_events rows for the same job can
		-- genuinely share created_at. Without the id tiebreak, DISTINCT ON
		-- would pick an arbitrary one of them instead of the actual latest.
		ORDER BY album_job_id, (event = ANY($2)) DESC, created_at DESC, id DESC`,
		jobIDs, failureExplainingEvents)
	if err != nil {
		return nil, fmt.Errorf("latest failure details: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID int64
		var detail string
		if err := rows.Scan(&jobID, &detail); err != nil {
			return nil, fmt.Errorf("latest failure details: scan: %w", err)
		}
		out[jobID] = detail
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
