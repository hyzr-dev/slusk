# Overview som nulägesbild — implementationsplan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Overview ska visa hela den sena pipelinen — det som laddar ner, väntar, importerar — plus det som avslutats senaste timmen, istället för bara aktiva och stalled jobb.

**Architecture:** Två nya state-baserade filter (`inflight`, `finished`) i `internal/store/dashboard.go` läggs vid sidan av dagens `transferring`, plus en ny sort (`recent`) och ett `SkipFacets`-fält som låter en anropare hoppa över de dyra facett- och count-frågorna. Frontend byter TRANSFERS-panelen till `inflight` och lägger en ny full-breddspanel under den som renderar `finished`. Sista task tar bort `transferring` när ingen längre skickar det.

**Tech Stack:** Go 1.26.3, Postgres (pgx), React 19, TypeScript, Vite, CSS Modules, TanStack Query, Vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-07-30-overview-nulage-design.md`
**Issue:** #287
**Gren:** `feat/overview-nulage-287`, worktree redan skapad från `origin/main` (`4cccd4e`)

## Global Constraints

- **Lägg till först, ta bort sist.** `transferring` får inte tas bort förrän frontend slutat skicka det (Task 9). Varje task måste lämna `go test ./...` och `npm test` gröna — ingen task får lämna repot rött för nästa.
- `dashboardJobStatusSQL` (`internal/store/dashboard.go:134-144`) **ändras inte**. Den är single source of truth för hela dashboarden sedan #269. Nya filter läggs på `j.state`, inte på den härledda statusen.
- Fönstret för `finished` är konstanten `DashboardFinishedWindow = time.Hour` i `internal/store/dashboard.go`. **Ingen config-nyckel** — `internal/config` avvisar okända nycklar och merge till `main` deployar direkt, så en ny obligatorisk nyckel skulle kunna hindra containern från att starta.
- **Ingen migration.** Inget index, ingen ny kolumn.
- `now` trådas alltid in explicit som `time.Time`. **Aldrig `now()` i SQL** — det skulle göra tester beroende av väggklockan.
- All användartext går genom `web/src/strings.ts`. Komponenter får aldrig inline:a text.
- All formatering går genom `web/src/format.ts`. Återanvänd `formatAge` (rad 157), skriv ingen ny åldersformatterare.
- CSS Modules genomgående. Inline `style` bara för genuint dynamiska värden.
- Kommentarer och identifierare på engelska. Exporterade Go-symboler får doc-kommentar som förklarar *varför*.
- Commit-ämne: `<type>: <description> (#287)`.
- `gofmt -l .` och `go vet ./...` ska vara rena före varje commit.
- **Känt brus** (får inte "fixas" genom att svaga tester): `internal/store` `TestOpenRecyclesIdleConnections` (#171), `web/` `Settings.test.tsx` under full svit (#242), `internal/soulseek` `TestConnectPeerIndirectSuccess` i container (#250). Ett fel som *inte* står här betyder att branchen gått sönder.

---

### Task 1: `inflight`-filter i store

**Files:**
- Modify: `internal/store/dashboard.go` — `validateDashboardJobsQuery` (rad 322-326), `dashboardJobsWhere` (rad 346-356)
- Test: `internal/store/dashboard_test.go`

**Interfaces:**
- Consumes: inget
- Produces: `DashboardJobsQuery{Filter: "inflight"}` väljer `j.state IN ('DOWNLOADING','IMPORTING')`. `transferring` fungerar oförändrat.

- [ ] **Step 1: Write the failing test**

Lägg till i `internal/store/dashboard_test.go`:

```go
func TestListDashboardJobsFilterInflightSelectsByStateNotStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// DOWNLOADING with a file actually moving -> status 'active'.
	moving := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Moving", "A", "peer1", 0, now)
	// DOWNLOADING with everything still PENDING -> status 'queued', which the
	// transferring union never selected. This is the whole point of inflight.
	pending := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDownloading, core.TransferPending, "Pending", "B", "peer2", 0, now.Add(time.Second))
	// DOWNLOADING with a stalled file -> status 'stalled'.
	stalled := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateDownloading, core.TransferStalled, "Stalled", "C", "peer3", 0, now.Add(2*time.Second))
	// IMPORTING -> status 'importing', also never in the transferring union.
	importing := insertDashboardTestJob(t, s, 4, core.SourceLidarr, core.StateImporting, "", "Importing", "D", "peer4", 0, now.Add(3*time.Second))
	// Excluded: not yet started, and already finished.
	insertDashboardTestJob(t, s, 5, core.SourceLidarr, core.StateWanted, "", "Wanted", "E", "", 0, now.Add(4*time.Second))
	insertDashboardTestJob(t, s, 6, core.SourceLidarr, core.StateSelecting, "", "Selecting", "F", "", 0, now.Add(5*time.Second))
	insertDashboardTestJob(t, s, 7, core.SourceLidarr, core.StateDone, "", "Done", "G", "", 0, now.Add(6*time.Second))
	insertDashboardTestJob(t, s, 8, core.SourceLidarr, core.StateFailed, "", "Failed", "H", "", 0, now.Add(7*time.Second))
	insertDashboardTestJob(t, s, 9, core.SourceLidarr, core.StateParked, "", "Parked", "I", "", 0, now.Add(8*time.Second))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "st", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := map[int64]bool{}
	for _, view := range page.Jobs {
		got[view.Job.ID] = true
	}
	for _, want := range []int64{moving, pending, stalled, importing} {
		if !got[want] {
			t.Errorf("job %d missing from inflight page", want)
		}
	}
	if len(page.Jobs) != 4 {
		t.Fatalf("len(jobs) = %d, want 4; got ids %v", len(page.Jobs), got)
	}
	if page.Total != 4 {
		t.Errorf("Total = %d, want 4", page.Total)
	}
}

func TestListDashboardJobsFilterInflightAndFinishedAreDisjoint(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// A DOWNLOADING job whose only transfer errored reports status 'failed'
	// via the candidate aggregate (dashboard.go:142) while its state is still
	// DOWNLOADING. Filtering on status would place it in BOTH regions; both
	// filters go through j.state precisely so it cannot.
	id := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDownloading, core.TransferErrored, "Errored", "A", "peer1", 0, now)

	inflight, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "st", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("inflight: %v", err)
	}
	if len(inflight.Jobs) != 1 || inflight.Jobs[0].Job.ID != id {
		t.Fatalf("inflight jobs = %+v, want exactly job %d", inflight.Jobs, id)
	}
	if inflight.Jobs[0].Status != "failed" {
		t.Errorf("Status = %q, want %q (the aggregate-derived status is unchanged)", inflight.Jobs[0].Status, "failed")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestListDashboardJobsFilterInflight' -v`
Expected: FAIL — `invalid dashboard jobs filter "inflight"`.

- [ ] **Step 3: Allow the filter in validation**

I `internal/store/dashboard.go`, i `validateDashboardJobsQuery`, byt filter-switchen:

```go
	switch q.Filter {
	case "all", "active", "importing", "queued", "stalled", "failed", "parked", "done", "transferring", "inflight":
	default:
		return fmt.Errorf("invalid dashboard jobs filter %q", q.Filter)
	}
```

- [ ] **Step 4: Add the WHERE clause**

I samma fil, i `dashboardJobsWhere`, byt `if includeStatus && q.Filter != "all" { ... }`-blocket mot en switch:

```go
	if includeStatus && q.Filter != "all" {
		switch q.Filter {
		case "transferring":
			// The union of 'active' and 'stalled' (issue #268, Overview's
			// TRANSFERS panel) — expressed against the same dashboardJobStatusSQL
			// CASE every other status filter uses, rather than a second copy of
			// the state predicates, so the two can never drift apart.
			clauses = append(clauses, "("+dashboardJobStatusSQL+") IN ("+bind("active")+", "+bind("stalled")+")")
		case "inflight":
			// Everything the pipeline currently holds a MaxActive slot for
			// (issue #287, Overview's TRANSFERS panel). Deliberately keyed on
			// j.state, not on dashboardJobStatusSQL: a DOWNLOADING job whose
			// transfers all errored reports status 'failed' while still being
			// in flight, so a status-keyed predicate would put it in this
			// region AND in 'finished' at the same time. A job has exactly one
			// state, which makes the two regions disjoint by construction.
			clauses = append(clauses, "j.state IN ("+bind(string(core.StateDownloading))+", "+bind(string(core.StateImporting))+")")
		default:
			clauses = append(clauses, "("+dashboardJobStatusSQL+") = "+bind(q.Filter))
		}
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestListDashboardJobsFilter' -v`
Expected: PASS, inklusive de befintliga `transferring`-testerna.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/store/ && go vet ./internal/store/
git add internal/store/dashboard.go internal/store/dashboard_test.go
git commit -m "feat(store): add the inflight job filter (#287)"
```

---

### Task 2: `sort=transfer` får fyra grupper

**Files:**
- Modify: `internal/store/dashboard.go` — `dashboardJobsOrder`, case `"transfer"` (rad 380-400)
- Test: `internal/store/dashboard_test.go`

**Interfaces:**
- Consumes: `Filter: "inflight"` från Task 1
- Produces: `Sort: "transfer"` rankar `active(1) → stalled(2) → queued(3) → importing(4) → övrigt(5)`, sen `created_at ASC, j.id ASC`.

Bakåtkompatibelt: grupp 1 och 2 är oförändrade, och `transferring`-unionen kan aldrig innehålla en rad som hamnar i grupp 3-5.

- [ ] **Step 1: Write the failing test**

```go
func TestListDashboardJobsSortTransferRanksImportingAfterWaiting(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Inserted in reverse rank order so a passing test cannot be an accident
	// of insertion order: importing first, active last.
	importing := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateImporting, "", "Importing", "A", "peer1", 0, now)
	waiting := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDownloading, core.TransferPending, "Waiting", "B", "peer2", 0, now.Add(time.Second))
	stalled := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateDownloading, core.TransferStalled, "Stalled", "C", "peer3", 0, now.Add(2*time.Second))
	active := insertDashboardTestJob(t, s, 4, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Active", "D", "peer4", 0, now.Add(3*time.Second))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := make([]int64, 0, len(page.Jobs))
	for _, view := range page.Jobs {
		got = append(got, view.Job.ID)
	}
	want := []int64{active, stalled, waiting, importing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (active, stalled, waiting, importing)", got, want)
	}
}

func TestListDashboardJobsSortTransferKeepsAgeOrderWithinGroup(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	older := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateImporting, "", "Older", "A", "peer1", 0, now)
	newer := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateImporting, "", "Newer", "B", "peer2", 0, now.Add(time.Minute))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != 2 || page.Jobs[0].Job.ID != older || page.Jobs[1].Job.ID != newer {
		t.Fatalf("order = %+v, want [%d %d] (created_at ascending within a group)", page.Jobs, older, newer)
	}
}
```

Lägg till `"reflect"` i importblocket om det inte redan finns.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestListDashboardJobsSortTransfer' -v`
Expected: FAIL på `TestListDashboardJobsSortTransferRanksImportingAfterWaiting` — dagens `ELSE 3` gör grupperna för `waiting` och `importing` lika, så ordningen inom dem avgörs av `created_at` och blir `[active, stalled, importing, waiting]`.

- [ ] **Step 3: Widen the ranking**

Byt `return`-satsen i case `"transfer"` (behåll kommentaren ovanför, men lägg till stycket nedan om de nya grupperna):

```go
		// Four groups since issue #287 widened the panel's filter from the
		// active+stalled union to every in-flight job: a job waiting for more
		// files ('queued') and a job past download ('importing') used to be
		// unreachable here and both collapsed into the old ELSE, which made
		// their relative order fall out of created_at alone. They now rank
		// explicitly, in pipeline order — moving, stuck, waiting, importing.
		return ` ORDER BY CASE (` + dashboardJobStatusSQL + `)
			WHEN 'active' THEN 1 WHEN 'stalled' THEN 2 WHEN 'queued' THEN 3
			WHEN 'importing' THEN 4 ELSE 5 END ASC, j.created_at ASC, j.id ASC`
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestListDashboardJobsSort' -v`
Expected: PASS, inklusive den befintliga `TestListDashboardJobsSortTransferGroupsActiveBeforeStalledThenAge`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/store/ && go vet ./internal/store/
git add internal/store/dashboard.go internal/store/dashboard_test.go
git commit -m "feat(store): rank importing and waiting in sort=transfer (#287)"
```

---

### Task 3: `finished`-filter med tidsfönster och `sort=recent`

**Files:**
- Modify: `internal/store/dashboard.go` — `DashboardJobsQuery` (rad 246-260), `validateDashboardJobsQuery`, `dashboardJobsWhere`, `dashboardJobsOrder`
- Test: `internal/store/dashboard_test.go`

**Interfaces:**
- Consumes: switchen i `dashboardJobsWhere` från Task 1
- Produces:
  - `DashboardFinishedWindow time.Duration` — exporterad konstant, `time.Hour`
  - `DashboardJobsQuery.Now time.Time` — obligatoriskt när `Filter == "finished"`, annars oanvänt
  - `Filter: "finished"` väljer `j.state IN ('DONE','FAILED') AND j.updated_at > Now - DashboardFinishedWindow`
  - `Sort: "recent"` ger `ORDER BY j.updated_at DESC, j.id DESC`; `Dir` måste vara `"desc"`

- [ ] **Step 1: Write the failing test**

```go
func TestListDashboardJobsFilterFinishedHonoursTheWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// insertDashboardTestJob writes its `at` argument to both created_at and
	// updated_at, which is exactly what the window reads.
	justDone := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "Just done", "A", "peer1", 0, now.Add(-time.Minute))
	justFailed := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateFailed, "", "Just failed", "B", "peer2", 0, now.Add(-30*time.Minute))
	// One second inside the window survives; one second outside does not.
	insideEdge := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateDone, "", "Inside edge", "C", "peer3", 0, now.Add(-DashboardFinishedWindow).Add(time.Second))
	insertDashboardTestJob(t, s, 4, core.SourceLidarr, core.StateDone, "", "Outside edge", "D", "peer4", 0, now.Add(-DashboardFinishedWindow).Add(-time.Second))
	// Excluded by state regardless of how fresh they are.
	insertDashboardTestJob(t, s, 5, core.SourceLidarr, core.StateParked, "", "Parked", "E", "", 0, now)
	insertDashboardTestJob(t, s, 6, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Downloading", "F", "peer6", 0, now)
	insertDashboardTestJob(t, s, 7, core.SourceLidarr, core.StateWanted, "", "Wanted", "G", "", 0, now)

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := make([]int64, 0, len(page.Jobs))
	for _, view := range page.Jobs {
		got = append(got, view.Job.ID)
	}
	// sort=recent is updated_at descending, so newest finish first.
	want := []int64{justDone, justFailed, insideEdge}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestListDashboardJobsSortRecentBreaksTiesById(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := now.Add(-time.Minute)

	low := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "Low id", "A", "peer1", 0, finishedAt)
	high := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDone, "", "High id", "B", "peer2", 0, finishedAt)

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	// Equal updated_at: without the id tiebreaker Postgres' order is
	// undefined and the same job could appear on two pages.
	if len(page.Jobs) != 2 || page.Jobs[0].Job.ID != high || page.Jobs[1].Job.ID != low {
		t.Fatalf("order = %+v, want [%d %d] (id descending on an updated_at tie)", page.Jobs, high, low)
	}
}

func TestValidateDashboardJobsQueryRejectsRecentAscendingAndMissingNow(t *testing.T) {
	base := DashboardJobsQuery{Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 5, Now: time.Now()}

	ascending := base
	ascending.Dir = "asc"
	if err := validateDashboardJobsQuery(ascending); err == nil {
		t.Error("dir=asc accepted for sort=recent, want rejected")
	}

	noNow := base
	noNow.Now = time.Time{}
	if err := validateDashboardJobsQuery(noNow); err == nil {
		t.Error("filter=finished accepted with a zero Now, want rejected")
	}

	// A zero Now is fine for every other filter: nothing reads it.
	otherFilter := base
	otherFilter.Filter = "done"
	otherFilter.Now = time.Time{}
	if err := validateDashboardJobsQuery(otherFilter); err != nil {
		t.Errorf("filter=done with zero Now: %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'Finished|SortRecent|RejectsRecent' -v`
Expected: kompileringsfel — `DashboardFinishedWindow` och fältet `Now` finns inte.

- [ ] **Step 3: Add the constant and the query field**

I `internal/store/dashboard.go`, ovanför `DashboardJobsQuery`:

```go
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
```

Lägg till fältet i `DashboardJobsQuery`:

```go
	// Now anchors filter=finished's window (see DashboardFinishedWindow) and is
	// required only for that filter — validateDashboardJobsQuery rejects a zero
	// value there and ignores it everywhere else. Threaded in rather than read
	// from the database's now() so tests are independent of the wall clock,
	// matching how every other time-dependent store method takes its `now`.
	Now time.Time
```

- [ ] **Step 4: Allow the filter and sort in validation**

Byt sort-switchen:

```go
	switch q.Sort {
	case "st", "album", "peer", "try", "transfer", "recent":
	default:
		return fmt.Errorf("invalid dashboard jobs sort %q", q.Sort)
	}
```

Byt filter-switchen (utökar Task 1:s rad):

```go
	switch q.Filter {
	case "all", "active", "importing", "queued", "stalled", "failed", "parked", "done", "transferring", "inflight", "finished":
	default:
		return fmt.Errorf("invalid dashboard jobs filter %q", q.Filter)
	}
```

Lägg till efter den befintliga `sort=transfer`/`dir=desc`-kontrollen:

```go
	// sort=recent is a newest-first ranking; ascending would be "oldest
	// finished first", which no caller wants and which would silently turn
	// Overview's recently-finished panel into its opposite. Rejected rather
	// than reinterpreted, the same treatment sort=transfer gets above.
	if q.Sort == "recent" && q.Dir == "asc" {
		return fmt.Errorf("dir=asc is not supported for dashboard jobs sort %q", q.Sort)
	}
	if q.Filter == "finished" && q.Now.IsZero() {
		return fmt.Errorf("dashboard jobs filter %q requires a non-zero Now", q.Filter)
	}
```

- [ ] **Step 5: Add the WHERE clause and the ORDER BY**

I `dashboardJobsWhere`, lägg ett case i switchen från Task 1 (efter `case "inflight":`):

```go
		case "finished":
			// Terminal in the pipeline sense and recent (issue #287, Overview's
			// recently-finished panel). PARKED is excluded on purpose: a job can
			// sit parked for days, so its updated_at would read as fresh without
			// anything having just happened. Keyed on j.state for the same
			// disjointness reason as inflight above.
			clauses = append(clauses,
				"j.state IN ("+bind(string(core.StateDone))+", "+bind(string(core.StateFailed))+")"+
					" AND j.updated_at > "+bind(q.Now.Add(-DashboardFinishedWindow)))
```

I `dashboardJobsOrder`, lägg ett case före `default`:

```go
	case "recent":
		// Newest finish first, for Overview's recently-finished panel (issue
		// #287). Direction is hardcoded DESC, never `direction`: validation
		// rejects dir=asc. j.id DESC is the tiebreaker — two jobs finishing in
		// the same transaction share an updated_at, and without a tiebreaker
		// Postgres' order between them is undefined, so the same job could
		// appear on two pages while another never shows at all.
		return " ORDER BY j.updated_at DESC, j.id DESC"
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'Finished|SortRecent|RejectsRecent' -v`
Expected: PASS.

- [ ] **Step 7: Verify the window never reaches the facets**

Run: `go test ./internal/store/ -run 'TestListDashboardJobs' -v`
Expected: PASS. Facetterna beräknas med `includeStatus=false`, så fönstret — som ligger inuti `finished`-grenen — följer med bort av sig självt. `facets.status.done` räknar fortfarande alla klara jobb, inte timmens.

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/store/ && go vet ./internal/store/
git add internal/store/dashboard.go internal/store/dashboard_test.go
git commit -m "feat(store): add the finished filter and recent sort (#287)"
```

---

### Task 4: `SkipFacets` i store

**Files:**
- Modify: `internal/store/dashboard.go` — `DashboardJobsQuery`, `ListDashboardJobs` (rad 409-489)
- Test: `internal/store/dashboard_test.go`

**Interfaces:**
- Consumes: Task 3:s `DashboardJobsQuery`
- Produces: `DashboardJobsQuery.SkipFacets bool`. När satt kör `ListDashboardJobs` bara sidfrågan; `Total` och `Facets` returneras som nollvärden.

Motivering: facettfrågan mättes till ~85 ms varm mot prod (5183 `album_jobs`) och körs annars vid varje anrop, oavsett filter. Overviews nya panel läser varken `Total` eller `Facets`.

- [ ] **Step 1: Write the failing test**

```go
func TestListDashboardJobsSkipFacetsReturnsPageWithoutCounts(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	first := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "First", "A", "peer1", 0, now.Add(-time.Minute))
	insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDone, "", "Second", "B", "peer2", 0, now.Add(-2*time.Minute))
	insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateWanted, "", "Wanted", "C", "", 0, now)

	query := DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 1, Now: now,
		SkipFacets: true,
	}
	page, err := s.ListDashboardJobs(context.Background(), query)
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Job.ID != first {
		t.Fatalf("jobs = %+v, want exactly job %d", page.Jobs, first)
	}
	if page.Total != 0 {
		t.Errorf("Total = %d, want 0 when facets are skipped", page.Total)
	}
	if page.Facets != (DashboardJobsFacets{}) {
		t.Errorf("Facets = %+v, want the zero value when skipped", page.Facets)
	}

	// The same query without SkipFacets still reports both.
	query.SkipFacets = false
	withFacets, err := s.ListDashboardJobs(context.Background(), query)
	if err != nil {
		t.Fatalf("ListDashboardJobs without SkipFacets: %v", err)
	}
	if withFacets.Total != 2 {
		t.Errorf("Total = %d, want 2", withFacets.Total)
	}
	if withFacets.Facets.Status.Done != 2 {
		t.Errorf("Facets.Status.Done = %d, want 2", withFacets.Facets.Status.Done)
	}
	// Facets ignore the status filter, so the WANTED job still counts in All.
	if withFacets.Facets.Status.All != 3 {
		t.Errorf("Facets.Status.All = %d, want 3", withFacets.Facets.Status.All)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestListDashboardJobsSkipFacets' -v`
Expected: kompileringsfel — `SkipFacets` finns inte.

- [ ] **Step 3: Add the field**

I `DashboardJobsQuery`:

```go
	// SkipFacets omits the status/source facet queries and the total count,
	// leaving DashboardJobsPage.Total and .Facets at their zero values. The
	// facet query evaluates dashboardJobStatusSQL over every non-cancelled row
	// — measured at ~85ms warm against production (5183 album_jobs, 15716
	// candidates, 74174 transfers, see issue #286) — and it runs regardless of
	// which filter was asked for, since facets deliberately ignore the status
	// filter. A caller that renders neither a total nor facet chips should not
	// pay for them. Callers that do read them must leave this false.
	SkipFacets bool
```

- [ ] **Step 4: Guard the three queries**

I `ListDashboardJobs`, omslut facett- och count-frågorna. Behåll `var page DashboardJobsPage`-deklarationen före blocket så sidfrågan kan skriva till den:

```go
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
```

Resten av funktionen (sidfrågan, `rows`-loopen, `tx.Commit()`) är oförändrad.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestListDashboardJobs' -v`
Expected: PASS — både det nya testet och samtliga befintliga, som aldrig sätter `SkipFacets` och därför beter sig identiskt.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/store/ && go vet ./internal/store/
git add internal/store/dashboard.go internal/store/dashboard_test.go
git commit -m "feat(store): let a caller skip the facet and count queries (#287)"
```

---

### Task 5: observ accepterar de nya parametrarna

**Files:**
- Modify: `internal/observ/observ.go` — `PagedJobsQuery` (rad 202-216), `parsePagedJobsQuery` (rad 757-823)
- Modify: `cmd/slusk/main.go` — `pagedJobsFn` (rad 271-296)
- Test: `internal/observ/observ_test.go`

**Interfaces:**
- Consumes: `store.DashboardJobsQuery`s nya fält från Task 3 och 4
- Produces:
  - `filter=inflight`, `filter=finished` och `sort=recent` accepteras på `GET /api/jobs`
  - `dir=asc` avvisas för `sort=recent`
  - Ny query-parameter `facets`: `facets=0` sätter `SkipFacets`, allt annat än `0` och `1` avvisas, default `1`
  - `PagedJobsQuery.SkipFacets bool` — enda nya fältet på structen

`PagedJobsQuery` får medvetet **inget** `Now`-fält: tidpunkten är inte något
klienten får bestämma (då kunde en anropare be om ett fönster bakåt i tiden).
`main.go` sätter `Now: time.Now()` när det mappar till `store.DashboardJobsQuery`.
Det håller också structen jämförbar med `!=`, vilket parsningstestet nedan
förlitar sig på.

**Beslut som specen lämnade öppen:** `SkipFacets` styrs av en egen query-parameter, inte av att servern gissar från `filter=finished`. Att låta ett filter tysta ändra svarets *form* skulle göra `finished` olik varje annat filter av skäl som inte har med filtrering att göra, och låsa in en framtida anropare som vill ha `finished` *med* `total`.

- [ ] **Step 1: Write the failing test**

```go
func TestPagedJobsEndpointParsesInflightFinishedAndFacets(t *testing.T) {
	cases := []struct {
		suffix string
		want   PagedJobsQuery
	}{
		{
			suffix: "?filter=inflight&sort=transfer&dir=asc&pageSize=8",
			want:   PagedJobsQuery{Page: 0, Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 8},
		},
		{
			suffix: "?filter=finished&sort=recent&dir=desc&pageSize=5&facets=0",
			want:   PagedJobsQuery{Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 5, SkipFacets: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.suffix, func(t *testing.T) {
			deps := testServerDeps(prometheus.NewRegistry())
			var got PagedJobsQuery
			deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
				got = query
				return PagedJobsResult{}, nil
			}
			rec := httptest.NewRecorder()
			NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs"+tc.suffix, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if got != tc.want {
				t.Fatalf("query = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPagedJobsEndpointRejectsRecentAscendingAndBadFacets(t *testing.T) {
	invalid := []string{
		"?sort=recent&dir=asc",
		"?sort=recent",  // dir defaults to asc, which sort=recent rejects
		"?facets=2",
		"?facets=yes",
		"?facets=",
		"?facets=0&facets=1",
	}
	for _, suffix := range invalid {
		t.Run(suffix, func(t *testing.T) {
			deps := testServerDeps(prometheus.NewRegistry())
			called := false
			deps.PagedJobs = func(ctx context.Context, query PagedJobsQuery) (PagedJobsResult, error) {
				called = true
				return PagedJobsResult{}, nil
			}
			rec := httptest.NewRecorder()
			NewServer(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs"+suffix, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if called {
				t.Fatal("PagedJobs called for invalid query")
			}
		})
	}
}
```

Notera `"?sort=recent"` utan `dir`: `parsePagedJobsQuery` defaultar `Dir` till `"asc"`, så en anropare som utelämnar `dir` får 400. Det är avsiktligt — klienten måste vara explicit — och testet låser fast beteendet så ingen "råkar" göra det tyst tillåtet senare.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/observ/ -run 'TestPagedJobsEndpointParsesInflight|TestPagedJobsEndpointRejectsRecent' -v`
Expected: kompileringsfel — `SkipFacets` finns inte på `PagedJobsQuery`.

- [ ] **Step 3: Extend PagedJobsQuery**

I `internal/observ/observ.go`, lägg till i `PagedJobsQuery`:

```go
	// SkipFacets asks the store to omit the total and facet counts (see
	// store.DashboardJobsQuery.SkipFacets — the facet query is the expensive
	// part of the request and runs regardless of filter). Set by facets=0.
	// A caller that renders a total or facet chips must leave this false.
	SkipFacets bool
```

- [ ] **Step 4: Parse and validate**

I `parsePagedJobsQuery`, lägg `"facets"` i allowlisten:

```go
	allowed := map[string]struct{}{"page": {}, "sort": {}, "dir": {}, "filter": {}, "source": {}, "q": {}, "pageSize": {}, "facets": {}}
```

Lägg parsningen efter `q`-blocket (före valideringen):

```go
	// facets=0 opts out of the total and the facet counts; 1 is the default and
	// the only other accepted value. Anything else is rejected rather than
	// coerced, so a typo can't silently drop counts the caller meant to render.
	if raw, ok := values["facets"]; ok {
		switch raw[0] {
		case "0":
			query.SkipFacets = true
		case "1":
			query.SkipFacets = false
		default:
			return PagedJobsQuery{}, errors.New("invalid facets")
		}
	}
```

Byt sort- och filter-valideringen:

```go
	if !oneOf(query.Sort, "st", "album", "peer", "try", "transfer", "recent") {
		return PagedJobsQuery{}, errors.New("invalid sort")
	}
```

```go
	if !oneOf(query.Filter, "all", "active", "importing", "queued", "stalled", "failed", "parked", "done", "transferring", "inflight", "finished") {
		return PagedJobsQuery{}, errors.New("invalid filter")
	}
```

Lägg efter den befintliga `sort=transfer`/`dir=desc`-kontrollen:

```go
	// sort=recent is newest-first by definition (see store.dashboardJobsOrder);
	// ascending would invert Overview's recently-finished panel. Note that Dir
	// defaults to "asc", so a caller asking for sort=recent must pass dir=desc
	// explicitly rather than relying on the default.
	if query.Sort == "recent" && query.Dir == "asc" {
		return PagedJobsQuery{}, errors.New("dir=asc is not supported for sort=recent")
	}
```

- [ ] **Step 5: Thread the new fields in main.go**

I `cmd/slusk/main.go`, i `pagedJobsFn`, utöka `store.DashboardJobsQuery`-literalen:

```go
	pagedJobsFn := func(ctx context.Context, query observ.PagedJobsQuery) (observ.PagedJobsResult, error) {
		page, err := st.ListDashboardJobs(ctx, store.DashboardJobsQuery{
			Page: query.Page, Sort: query.Sort, Dir: query.Dir,
			Filter: query.Filter, Source: query.Source, Query: query.Query,
			PageSize: query.PageSize, SkipFacets: query.SkipFacets,
			// filter=finished anchors its window here rather than in SQL, so
			// the store never reads the database clock (see
			// store.DashboardFinishedWindow). Every other filter ignores it.
			Now: time.Now(),
		})
```

Resten av funktionen är oförändrad. Säkerställ att `"time"` finns i importblocket.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/observ/ ./cmd/... -v -run 'PagedJobs'`
Expected: PASS, inklusive den befintliga `TestPagedJobsEndpointRejectsInvalidQueries`.

- [ ] **Step 7: Run the full Go suite**

Run: `go test ./...`
Expected: PASS. Endast de kända bruskandidaterna får fallera (#171, #250).

- [ ] **Step 8: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/observ/observ.go internal/observ/observ_test.go cmd/slusk/main.go
git commit -m "feat(observ): accept the inflight, finished, recent and facets parameters (#287)"
```

---

### Task 6: frontend-typer och `useJobs`-URL

**Files:**
- Modify: `web/src/api/types.ts` — `JobPageSort`, `JobStatusFilter`, `JobPageParams`
- Modify: `web/src/api/queries.ts` — `jobsPageUrl` (rad 251-265), `queryKeys.jobsPage` (rad 78-113)
- Create: `web/src/api/queries.test.ts` (finns inte idag — `web/src/api/` har bara `client.test.ts` och `normalize.test.ts`)

**Interfaces:**
- Consumes: Task 5:s HTTP-kontrakt
- Produces:
  - `JobPageSort` innehåller `'recent'`
  - `JobStatusFilter` innehåller `'inflight'` och `'finished'`
  - `JobPageParams.skipFacets?: boolean`
  - `jobsPageUrl` sätter `facets=0` när `skipFacets` är `true`, annars ingen `facets`-parameter
  - `queryKeys.jobsPage` inkluderar `skipFacets`, så två annars identiska frågor inte delar cache-post

- [ ] **Step 1: Write the failing test**

Skapa `web/src/api/queries.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { jobsPageUrl, queryKeys } from './queries';
import type { JobPageParams } from './types';

const base: JobPageParams = {
  page: 0,
  sort: 'st',
  dir: 'asc',
  filter: 'all',
  source: 'all',
  q: '',
};

describe('jobsPageUrl', () => {
  it('omits facets unless skipFacets is set', () => {
    expect(jobsPageUrl(base)).not.toContain('facets');
  });

  it('sends facets=0 when skipFacets is set', () => {
    const url = jobsPageUrl({ ...base, filter: 'finished', sort: 'recent', dir: 'desc', pageSize: 5, skipFacets: true });
    expect(url).toContain('facets=0');
    expect(url).toContain('filter=finished');
    expect(url).toContain('sort=recent');
    expect(url).toContain('dir=desc');
  });

  it('keys the cache on skipFacets so two otherwise identical queries do not collide', () => {
    const withFacets = queryKeys.jobsPage(base);
    const without = queryKeys.jobsPage({ ...base, skipFacets: true });
    expect(withFacets).not.toEqual(without);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/api/queries.test.ts`
Expected: FAIL — typfel på `skipFacets`, och `facets=0` saknas i URL:en.

- [ ] **Step 3: Widen the types**

I `web/src/api/types.ts`:

```ts
export type JobPageSort = 'st' | 'album' | 'peer' | 'try' | 'transfer' | 'recent';
```

```ts
export type JobStatusFilter =
  | 'all'
  | 'active'
  | 'importing'
  | 'queued'
  | 'stalled'
  | 'failed'
  | 'parked'
  | 'done'
  | 'transferring'
  | 'inflight'
  | 'finished';
```

Lägg fältet i `JobPageParams`:

```ts
export interface JobPageParams {
  page: number;
  sort: JobPageSort;
  dir: JobPageDirection;
  filter: JobStatusFilter;
  source: JobSourceFilter;
  q: string;
  pageSize?: number;
  /**
   * Opt out of `total` and the facet counts (`facets=0`). The server's facet
   * query is the expensive part of `/api/jobs` and runs whatever the filter is,
   * so a panel that renders neither should not ask for them. Leave unset in any
   * view that reads `total` or renders facet chips.
   */
  skipFacets?: boolean;
}
```

- [ ] **Step 4: Send the parameter and key the cache on it**

I `web/src/api/queries.ts`, i `jobsPageUrl`, före `return`:

```ts
  if (params.pageSize !== undefined) query.set('pageSize', String(params.pageSize));
  // Only sent when opting out: the server defaults to facets=1, so an absent
  // parameter and facets=1 mean the same thing and omitting it keeps the URL
  // (and therefore the cache key) unchanged for every existing caller.
  if (params.skipFacets) query.set('facets', '0');
  return `/api/jobs?${query.toString()}`;
```

I `queryKeys.jobsPage`, lägg `skipFacets` sist i arrayen:

```ts
  jobsPage: (params: JobPageParams) => [
    'jobs',
    'page',
    params.page,
    params.sort,
    params.dir,
    params.filter,
    params.source,
    params.q,
    params.pageSize,
    params.skipFacets,
  ] as const,
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd web && npx vitest run src/api/ && npx tsc --noEmit`
Expected: PASS och rent `tsc`.

- [ ] **Step 6: Commit**

```bash
git add web/src/api/types.ts web/src/api/queries.ts web/src/api/queries.test.ts
git commit -m "feat(ui): allow inflight, finished, recent and skipFacets in the jobs query (#287)"
```

---

### Task 7: TRANSFERS byter till `inflight`

**Files:**
- Modify: `web/src/routes/Overview.tsx` — `useJobs`-anropet (rad 42), kommentaren ovanför (rad 17-25), `SectionHeader`-metan (rad 146-151)
- Modify: `web/src/strings.ts` — `t.overview`
- Test: `web/src/routes/Overview.test.tsx`

**Interfaces:**
- Consumes: Task 6:s typer
- Produces: TRANSFERS hämtar `filter: 'inflight'`; metan visar `result.total` och avslöjar avkapning

`t.overview.activeCountMeta` ersätts av `inFlightCountMeta` och `inFlightTruncatedMeta`. Den gamla nyckeln tas bort — `strings.test.ts` vaktar oanvända nycklar, så en kvarlämnad nyckel fäller sviten.

- [ ] **Step 1: Write the failing test**

I `web/src/routes/Overview.test.tsx`, byt `TRANSFER_PARAMS` och lägg till testerna:

```tsx
const TRANSFER_PARAMS: JobPageParams = {
  page: 0,
  filter: 'inflight',
  sort: 'transfer',
  dir: 'asc',
  source: 'all',
  q: '',
  pageSize: 8,
};
```

```tsx
it('renders an importing row and a waiting row that the old union never selected', () => {
  renderOverview(makeJobPage([
    { ...baseJob, id: 1, title: 'Importing Album', status: 'importing', state: 'IMPORTING', speed: 0 },
    { ...baseJob, id: 2, title: 'Waiting Album', status: 'queued', state: 'DOWNLOADING', speed: 0, bytesDone: 0, bytesTotal: 100 },
  ]));

  expect(screen.getByText('Importing Album')).toBeInTheDocument();
  expect(screen.getByText('Waiting Album')).toBeInTheDocument();
  // IMPORTING replaces the byte counts with the verifying label.
  expect(screen.getByText(t.jobs.verifying)).toBeInTheDocument();
  // Neither row is moving bytes, so neither may flare.
  expect(document.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
});

it('takes the transfers meta from total, not from the active counter', () => {
  renderOverview(makeJobPage([{ ...baseJob, id: 1, title: 'Only Row', status: 'active' }], 1));
  expect(screen.getByText(t.overview.inFlightCountMeta(1))).toBeInTheDocument();
});

it('reveals truncation when total exceeds the rendered rows', () => {
  // pageSize is 8; a total of 12 means four in-flight jobs are not shown.
  const rows = Array.from({ length: 8 }, (_, i) => ({ ...baseJob, id: i + 1, title: `Row ${i + 1}`, status: 'active' as JobStatus }));
  renderOverview(makeJobPage(rows, 12));
  expect(screen.getByText(t.overview.inFlightTruncatedMeta(8, 12))).toBeInTheDocument();
});
```

`makeJobPage` hårdkodar idag `total: jobs.length` (`Overview.test.tsx:71`), vilket gör avkapningsfallet oåtkomligt. Gör `total` till en valfri parameter och behåll `makeFacets(jobs)` som är:

```tsx
function makeJobPage(jobs: Job[], total: number = jobs.length): JobPage {
  return { jobs, total, facets: makeFacets(jobs) };
}
```

Standardvärdet gör att varje befintlig anropare beter sig oförändrat.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npx vitest run src/routes/Overview.test.tsx`
Expected: FAIL — `t.overview.inFlightCountMeta` finns inte, och den seedade cache-nyckeln matchar inte längre den `filter: 'transferring'` komponenten fortfarande skickar.

- [ ] **Step 3: Add the strings**

I `web/src/strings.ts`, i `overview:`-objektet: **ta bort** `activeCountMeta` och lägg till

```ts
  inFlightCountMeta: (n: number) => `${n} in flight`,
  // Shown instead of inFlightCountMeta when the panel cannot fit every
  // in-flight job: max_active can exceed the panel's row count, and silently
  // dropping the remainder would read as "this is all of it".
  inFlightTruncatedMeta: (shown: number, total: number) => `${shown} of ${total} in flight`,
```

- [ ] **Step 4: Switch the filter and the meta**

I `web/src/routes/Overview.tsx`, byt rad 42:

```tsx
  const jobsQuery = useJobs({ page: 0, filter: 'inflight', sort: 'transfer', dir: 'asc', source: 'all', q: '', pageSize: TRANSFER_PAGE_SIZE });
```

Byt kommentaren ovanför `TRANSFER_PAGE_SIZE` (rad 17-25):

```tsx
// Rows in the TRANSFERS panel — matches the mock
// (docs/design/slusk-tui.dc.html:105) rather than the full jobs list.
// Selection, ordering and this row count are all server-side (issue #268):
// filter=inflight is every job the pipeline holds a MaxActive slot for —
// state DOWNLOADING or IMPORTING (issue #287 widened this from the old
// active+stalled 'transferring' union, which dropped a job the moment it
// stopped moving bytes). sort=transfer ranks active, then stalled, then
// waiting, then importing, and orders by createdAt ascending inside a group.
// The client renders result.jobs exactly as returned — no filter, sort or
// slice here.
const TRANSFER_PAGE_SIZE = 8;
```

Byt `SectionHeader`-metan (rad 146-151):

```tsx
        <SectionHeader
          label={t.overview.transfersHeading}
          // A count is a claim, not a placeholder — omit the meta until
          // /api/jobs has answered. SectionHeader skips a falsy meta.
          // total comes from the same response as the rows, so it can reveal
          // the rows this fixed-height panel could not fit.
          meta={
            hasData(jobsPhase)
              ? (result?.total ?? 0) > transferRows.length
                ? t.overview.inFlightTruncatedMeta(transferRows.length, result?.total ?? 0)
                : t.overview.inFlightCountMeta(transferRows.length)
              : undefined
          }
        />
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd web && npx vitest run src/routes/Overview.test.tsx && npx tsc --noEmit`
Expected: PASS.

Verifiera sedan att den gamla nyckeln verkligen är borta:

Run: `grep -rn "activeCountMeta" web/src`
Expected: inga träffar.

Detta måste vara en grep, inte ett test: `strings.test.ts` testar bara tre label-uppslagsfunktioner och vaktar inte oanvända nycklar, och `tsc` bryr sig inte om en oanvänd objektegenskap. Ingenting utom greppen fångar en kvarlämnad nyckel.

- [ ] **Step 6: Commit**

```bash
git add web/src/routes/Overview.tsx web/src/routes/Overview.test.tsx web/src/strings.ts
git commit -m "feat(ui): widen the transfers panel to every in-flight job (#287)"
```

---

### Task 8: region 2 — panelen för nyligen avslutade

**Files:**
- Modify: `web/src/routes/Overview.tsx`
- Modify: `web/src/routes/Overview.module.css`
- Modify: `web/src/strings.ts`
- Test: `web/src/routes/Overview.test.tsx`

**Interfaces:**
- Consumes: Task 6:s typer, Task 7:s Overview-struktur
- Produces: en `<Panel>` mellan TRANSFERS och `styles.mainGrid` som renderar `filter=finished`

- [ ] **Step 1: Write the failing test**

```tsx
// Mirrors the params Overview.tsx passes for the recently-finished panel.
const FINISHED_PARAMS: JobPageParams = {
  page: 0,
  filter: 'finished',
  sort: 'recent',
  dir: 'desc',
  source: 'all',
  q: '',
  pageSize: 5,
  skipFacets: true,
};
```

`renderOverview` måste seeda även den nya cache-nyckeln. Utöka den:

```tsx
function renderOverview(
  jobsData: JobPage = jobPage,
  chartsData: ChartsReport | undefined = charts,
  statusData: StatusReport | undefined = status,
  // `null` means "leave this key unseeded" so the query stays pending —
  // distinct from the default, which seeds a ready but empty page. The two
  // are different states and a test asserting on one must not silently get
  // the other (see the gate test below).
  finishedData: JobPage | null = makeJobPage([]),
) {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.jobsPage(TRANSFER_PARAMS), jobsData);
  if (finishedData !== null) {
    queryClient.setQueryData(queryKeys.jobsPage(FINISHED_PARAMS), finishedData);
  }
  queryClient.setQueryData(queryKeys.status, statusData);
  queryClient.setQueryData(queryKeys.charts, chartsData);
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Overview />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}
```

Testerna:

```tsx
it('renders a done row and a failed row in the recently finished panel', () => {
  renderOverview(jobPage, charts, status, makeJobPage([
    { ...baseJob, id: 90, title: 'Finished Album', artist: 'Artist A', status: 'done', state: 'DONE', peer: 'someuser', updatedAt: new Date(Date.now() - 12 * 60 * 1000).toISOString() },
    { ...baseJob, id: 91, title: 'Dead Album', artist: 'Artist B', status: 'failed', state: 'FAILED', peer: '', updatedAt: new Date(Date.now() - 41 * 60 * 1000).toISOString() },
  ]));

  expect(screen.getByText(t.overview.finishedHeading)).toBeInTheDocument();
  expect(screen.getByText('Finished Album')).toBeInTheDocument();
  expect(screen.getByText('Dead Album')).toBeInTheDocument();
  // formatAge on updatedAt — a one-hour window can only ever produce minutes.
  expect(screen.getByText('12m')).toBeInTheDocument();
  expect(screen.getByText('41m')).toBeInTheDocument();
});

it('shows a window-agnostic empty state when nothing finished recently', () => {
  renderOverview(jobPage, charts, status, makeJobPage([]));
  expect(screen.getByText(`── ${t.overview.noneFinished} ──`)).toBeInTheDocument();
  // The copy must not name the window: the length is a Go constant and no
  // test in either suite could catch the two drifting apart.
  expect(t.overview.noneFinished).not.toMatch(/hour|minute|\d/i);
});

it('keeps the transfers panel alive when the finished query has no data', () => {
  // Passing `undefined` here would NOT work: renderOverview's 4th parameter
  // has a JS default (`= makeJobPage([])`), and an explicit `undefined`
  // triggers that default regardless of the declared type — so the test
  // would seed a ready, empty page and prove nothing. `null` is the sentinel
  // for "do not seed this key at all", leaving the query pending (fetch is
  // stubbed to hang forever). That is what makes this test able to fail:
  // if both regions shared one gate, a pending finished query would suppress
  // the transfers rows too.
  renderOverview(jobPage, charts, status, null);
  // A dead poll for one region must never blank another (issue #201).
  expect(screen.getByText(t.overview.transfersHeading)).toBeInTheDocument();
  expect(document.querySelectorAll('[class*="transferRow"]').length).toBeGreaterThan(0);
});

it('renders both panels from their own independent queries', () => {
  renderOverview(
    makeJobPage([{ ...baseJob, id: 1, title: 'In Flight', status: 'active' }]),
    charts,
    status,
    makeJobPage([{ ...baseJob, id: 90, title: 'Finished Album', status: 'done', state: 'DONE' }]),
  );
  expect(screen.getByText('In Flight')).toBeInTheDocument();
  expect(screen.getByText('Finished Album')).toBeInTheDocument();
});
```

**Ingen test för SSE-scopet, avsiktligt.** Kravet "bara de pågående radernas
id:n i scopet" uppfylls av att `useJobScope`-raden *inte* ändras — det finns
inget nytt beteende att verifiera. Och det vore inte observerbart här ändå:
`Overview.test.tsx` renderar utan `StreamProvider`, så
`JobScopeSetterContext`s setter är sin no-op-default. Ett test som heter
"scopes the stream" men bara kontrollerar att rader renderas skulle ljuga om
sin egen täckning. Skyddet är kommentaren i Step 5 plus att raden syns
oförändrad i diffen.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npx vitest run src/routes/Overview.test.tsx`
Expected: FAIL — `t.overview.finishedHeading` finns inte och panelen renderas inte.

- [ ] **Step 3: Add the strings**

I `web/src/strings.ts`, i `overview:`:

```ts
  finishedHeading: 'RECENTLY FINISHED',
  // Deliberately says nothing about how long "recently" is: the window is a
  // Go constant (store.DashboardFinishedWindow) and this is a TypeScript
  // string, so no test in either suite could catch them contradicting each
  // other. An agnostic phrasing cannot go out of date.
  noneFinished: 'Nothing finished recently',
  finishedGridHead: {
    status: 'ST',
    album: 'ALBUM',
    peer: 'PEER',
    when: 'WHEN',
  },
```

- [ ] **Step 4: Add the CSS**

I `web/src/routes/Overview.module.css`, efter `.transferGrid`-blocket:

```css
/* Four columns rather than the six TRANSFERS uses: a finished job has no
   progress or speed left to report, and its byte counts are either complete
   or meaningless. Same 28px status gutter and same album column shape, so
   the two panels read as one stack. */
.finishedGrid {
  display: grid;
  grid-template-columns: 28px minmax(150px, 2fr) minmax(76px, 1fr) 62px;
  gap: 10px;
  align-items: center;
}

/* Same treatment as .reconcileDur (line 151): right-aligned and dimmed.
   tabular-nums keeps the column from jittering as ages tick 9m -> 10m. */
.finishedWhen {
  text-align: right;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
}
```

`--text-dim` finns redan (`web/src/styles/tokens.css:20`).

- [ ] **Step 5: Add the query and the panel**

I `web/src/routes/Overview.tsx`, lägg konstanten vid `TRANSFER_PAGE_SIZE`:

```tsx
// Rows in the RECENTLY FINISHED panel. Selection is server-side: filter=finished
// is state DONE or FAILED with an updated_at inside the backend's window
// (store.DashboardFinishedWindow), sort=recent is newest finish first. The panel
// reads neither total nor facets, so it opts out of them (skipFacets) — the facet
// query is the expensive half of /api/jobs and runs whatever the filter is
// (issue #286).
const FINISHED_PAGE_SIZE = 5;
```

Lägg queryn efter `chartsQuery`:

```tsx
  const finishedQuery = useJobs({
    page: 0, filter: 'finished', sort: 'recent', dir: 'desc',
    source: 'all', q: '', pageSize: FINISHED_PAGE_SIZE, skipFacets: true,
  });
```

Efter `const charts = chartsQuery.data;`:

```tsx
  const finishedRows = finishedQuery.data?.jobs ?? [];
```

Efter `const chartsPhase = queryPhase(chartsQuery);`:

```tsx
  const finishedPhase = queryPhase(finishedQuery);
```

`useJobScope` lämnas **oförändrad** — den ska bara ha de pågående radernas id:n. Lägg en kommentar så nästa läsare inte "rättar" det:

```tsx
  // Only the in-flight rows: a finished job is terminal and the stream never
  // sends deltas for it, so widening the scope would cost the backend
  // bookkeeping for updates that can never arrive.
  useJobScope(transferRows.map((job) => job.id));
```

Lägg panelen mellan TRANSFERS-`</Panel>` och `<div className={styles.mainGrid}>`:

```tsx
      <Panel>
        <SectionHeader label={t.overview.finishedHeading} />
        <QueryNotice phase={finishedPhase} />
        {hasData(finishedPhase) &&
          (finishedRows.length === 0 ? (
            <EmptyState message={t.overview.noneFinished} />
          ) : (
            <div role="table">
              <div role="row" className={`${styles.finishedGrid} ${styles.transferHead}`}>
                <span role="columnheader">{t.overview.finishedGridHead.status}</span>
                <span role="columnheader">{t.overview.finishedGridHead.album}</span>
                <span role="columnheader">{t.overview.finishedGridHead.peer}</span>
                <span role="columnheader" className={styles.headRight}>{t.overview.finishedGridHead.when}</span>
              </div>
              {finishedRows.map((job) => (
                <div
                  key={job.id}
                  role="row"
                  className={`${styles.finishedGrid} ${styles.transferRow}`}
                  onClick={() => navigate(`/jobs/${job.id}`)}
                >
                  <span role="cell">
                    <Tag status={job.status} bare />
                  </span>
                  <span role="cell" className={styles.albumCell}>
                    <span className={styles.transferTitle}>{job.title}</span>
                    <span className={styles.transferArtist}>{job.artist}</span>
                  </span>
                  <span role="cell" className={styles.peerCell}>{job.peer || '—'}</span>
                  <span role="cell" className={styles.finishedWhen}>{finishedAge(job.updatedAt)}</span>
                </div>
              ))}
            </div>
          ))}
      </Panel>
```

`Tag` får medvetet **ingen** `queuePosition`: ett avslutat jobb kan bära en kvarglömd köposition, och `tagFor` ignorerar den ändå för terminala statusar — att skicka den vore att antyda att den betyder något här.

Lägg hjälpfunktionen ovanför `Overview`-komponenten, bredvid `tickTone`:

```tsx
/**
 * Age of a finished job, for the WHEN column. Reads updated_at, which is the
 * completion stamp for DONE and FAILED — MarkJobFailed is guarded against
 * re-failing an already-terminal job and the wanted-sync metadata backfill
 * leaves updated_at alone past WANTED, so it never moves again once set.
 * Returns an em dash for a missing or unparseable value rather than a
 * misleading "0s".
 */
function finishedAge(updatedAt: string): string {
  if (!updatedAt) return '—';
  const ms = Date.now() - new Date(updatedAt).getTime();
  if (Number.isNaN(ms)) return '—';
  return formatAge(Math.max(0, Math.floor(ms / 1000)));
}
```

Lägg `formatAge` i import-satsen från `'../format'`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd web && npx vitest run src/routes/Overview.test.tsx src/strings.test.ts && npx tsc --noEmit`
Expected: PASS.

- [ ] **Step 7: Run the full web suite and build**

Run: `cd web && npm test && npm run build`
Expected: PASS. Bara `Settings.test.tsx` får fallera under full svit (#242).

- [ ] **Step 8: Commit**

```bash
git add web/src/routes/Overview.tsx web/src/routes/Overview.module.css web/src/routes/Overview.test.tsx web/src/strings.ts
git commit -m "feat(ui): add the recently finished panel to Overview (#287)"
```

---

### Task 9: ta bort `transferring` och verifiera i browser

**Files:**
- Modify: `internal/store/dashboard.go` — `validateDashboardJobsQuery`, `dashboardJobsWhere`
- Modify: `internal/observ/observ.go` — `parsePagedJobsQuery`
- Modify: `web/src/api/types.ts` — `JobStatusFilter`
- Test: `internal/store/dashboard_test.go`, `internal/observ/observ_test.go`, `web/src/routes/Overview.test.tsx`

**Interfaces:**
- Consumes: allt ovan
- Produces: `transferring` finns inte längre i någon lager. `inflight` är den enda vägen till TRANSFERS-mängden.

Detta är sista task av en anledning: fram till nu har `transferring` funnits kvar så varje mellanliggande commit är grön.

- [ ] **Step 1: Confirm nothing sends it any more**

Run:
```bash
grep -rn "transferring" --include=*.go --include=*.ts --include=*.tsx . | grep -v _test
```
Expected: träffar bara i `internal/store/dashboard.go`, `internal/observ/observ.go` och `web/src/api/types.ts` — alltså bara definitionerna, ingen anropare. Finns någon annan träff: stanna och utred innan du tar bort något.

- [ ] **Step 2: Delete the store filter**

I `validateDashboardJobsQuery`, ta bort `"transferring"` ur filter-switchen:

```go
	switch q.Filter {
	case "all", "active", "importing", "queued", "stalled", "failed", "parked", "done", "inflight", "finished":
	default:
		return fmt.Errorf("invalid dashboard jobs filter %q", q.Filter)
	}
```

I `dashboardJobsWhere`, ta bort hela `case "transferring":`-grenen med dess kommentar.

- [ ] **Step 3: Delete it from observ and from the TS type**

I `parsePagedJobsQuery`:

```go
	if !oneOf(query.Filter, "all", "active", "importing", "queued", "stalled", "failed", "parked", "done", "inflight", "finished") {
		return PagedJobsQuery{}, errors.New("invalid filter")
	}
```

I `web/src/api/types.ts`, ta bort `| 'transferring'` ur `JobStatusFilter`. Uppdatera doc-kommentaren ovanför (rad 25-29) som beskriver `'transfer'` och `'transferring'` så den nämner `inflight`/`finished` istället.

- [ ] **Step 4: Update the tests that still name it**

Byt de befintliga `transferring`-testerna till `inflight`:

- `internal/store/dashboard_test.go:1243` `TestListDashboardJobsFilterTransferringUnion` — **ta bort**. Task 1:s `TestListDashboardJobsFilterInflightSelectsByStateNotStatus` täcker samma mängd plus de fyra fall unionen aldrig kunde välja, så täckningen ökar snarare än minskar. Läs igenom det gamla testet först och bekräfta att varje påstående det gör finns i det nya.
- `internal/observ/observ_test.go:446` `TestPagedJobsEndpointParsesTransferringFilterSortAndPageSize` — **ta bort**. Task 5:s `TestPagedJobsEndpointParsesInflightFinishedAndFacets` ersätter den och täcker båda de nya filtren.
- Lägg `"?filter=transferring"` i `TestPagedJobsEndpointRejectsInvalidQueries`' lista, så borttagningen är låst.

- [ ] **Step 5: Run both suites**

Run: `go test ./... && cd web && npm test && npx tsc --noEmit && npm run build`
Expected: PASS. Bara känt brus får fallera (#171, #242, #250).

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add -u internal/store internal/observ web/src
git commit -m "refactor: drop the transferring filter now that inflight replaces it (#287)"
```

- [ ] **Step 7: Verify in a real browser**

`web/`-sviten kör i jsdom, som varken beräknar layout eller ritar — ingenting nedan kan fällas av ett test.

```bash
./testenv/lab.sh up          # backend, en gång
make dev                     # http://localhost:5173
```

Följ `verifying-ui-in-browser`-skillen. Kontrollera:

1. **`finishedGrid` håller ihop** med långa albumtitlar och tomt peer-värde (`—`). Titeln ska trunkeras, inte trycka WHEN-kolumnen ur bild.
2. **Radhöjd och träffyta** i den nya panelen matchar TRANSFERS. Klickbara rader inuti `role="cell"`-wrappers har en känd fälla där en wrapper degraderar ett grid-item till inline och halverar träffytan — här klickar hela raden, men mät höjden i devtools och jämför med en TRANSFERS-rad.
3. **Brytpunkt 900px**: `mainGrid` kollapsar till en kolumn. Den nya panelen är full bredd och ska vara oförändrad.
4. **Brytpunkt 640px**: `statGrid` går till två kolumner. Kontrollera att `finishedGrid`s `28px + minmax(150px,2fr) + minmax(76px,1fr) + 62px` inte tvingar horisontell scroll på sidan — det får bara scrolla inuti sin egen container, aldrig `body`.
5. **Tom-vy** syns när inget avslutats senaste timmen, och texten namnger inte fönstret.
6. **Sidans totalhöjd**: med båda paneler ifyllda, hamnar `mainGrid` orimligt långt ner? Om ja, notera det — det är i så fall ett designbeslut att ta med dig, inte något att tysta fixa.

- [ ] **Step 8: Verify against the lab and open the PR**

Kontrollera i labbet att `filter=inflight` faktiskt visar jobb som väntar på import, och att ett jobb som blir klart flyttar från TRANSFERS till RECENTLY FINISHED inom en pollcykel (15 s).

```bash
tea pulls create --head feat/overview-nulage-287 --base main \
  --title "feat: Overview som nulägesbild med inflight- och finished-regioner (#287)" \
  --description "$(cat pr-body.md)"
tea pulls <n> --output json    # verifiera att head är rätt gren
```

PR-beskrivningen ska innehålla `Closes #287` **i klartext, inte inuti backticks** — Gitea parsar inte nyckelordet i kodformat.

---

## Vad denna plan medvetet inte gör

| Inte detta | Varför |
|---|---|
| Index på `(state, updated_at)` | Sidfrågan är ~1,5 ms mot prod; facettfrågan i samma request är ~85 ms. Att optimera 1 % med en oåterkallelig migration är inte försvarbart. |
| Fixar facettfrågans `SubPlan`/JIT-kostnad | Rör `currentCandidateSubquery`/`jobViewFrom`, sanningskällan för hela dashboarden. Egen risk, egen PR — #286. |
| Retention för `album_jobs` | Noterat i #286. |
| Config-nyckel för fönstret | Strikt config + auto-deploy gör en ny obligatorisk nyckel till en startrisk. |
| SELECTING i region 1 | Väntrummet saknar `MaxActive`-tak och skulle svämma över panelen med kö. |
| `failReason` eller accordion i Overview | Finns i jobbdetaljen; att lägga en detaljquery per rad drar mot #274. |
| Ändringar i `dashboardJobStatusSQL` | Single source of truth sedan #269. Urvalet läggs på `j.state` istället. |
