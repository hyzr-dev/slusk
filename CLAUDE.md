# slskdarr

A Go rewrite of `soularr`: a bridge between Lidarr and Soulseek. It polls Lidarr for
wanted albums, searches Soulseek, downloads candidates, and hands finished albums back
to Lidarr for import. Unlike soularr it keeps persistent state, so a restart does not
strand in-flight downloads.

Go 1.26.3. React 19 + TypeScript in `web/`. Postgres only — `internal/store` opens `pgx`
and nothing else, and the migration runner takes a `pg_advisory_lock`. SQLite survives
solely in `cmd/sqlite2pg`, the one-off tool that reads a legacy SQLite file and writes it
into Postgres.

## Build and test

```bash
make build     # builds the web UI, then the Go binary
make ui        # web UI only (npm ci + vite build → internal/observ/web/dist)
make test      # go test ./... && npm test
make dev       # vite dev server
go test ./...            # ~717 tests; run this before claiming anything works
go test ./... -race      # required for anything touching concurrency
```

`go vet ./...` and `gofmt -l .` should both be clean.

## Merging to main deploys to production

This is the single most important thing to know about this repo.

`.gitea/workflows/release.yml` runs on every push to `main`, reads conventional commit
prefixes since the last tag, and pushes a new `v*` tag. That tag triggers `deploy.yml`,
which builds and pushes the image and tells the homelab updater to redeploy.

| Prefix | Effect |
|---|---|
| `feat:` | minor bump → **deploys to prod within minutes** |
| `fix:` | patch bump → **deploys to prod within minutes** |
| `!:` or `BREAKING CHANGE` | major bump → deploys |
| `chore:`, `docs:`, `ci:`, `refactor:`, `style:`, `test:` | no bump, no deploy |

There is no staging step. "Merge it and see" is a production action.

## The local PR lab is the substitute for staging

`testenv/` runs the full stack — this checkout's slskdarr, plus Lidarr, slskd and
Postgres — against real Soulseek searches, with no production data involved. Use it to
verify a PR before merging, since merging is the deploy.

```bash
cp testenv/.env.example testenv/.env   # first time: fill in two Soulseek test accounts
./testenv/lab.sh reset                 # clean run of the current checkout
./testenv/lab.sh info                  # addresses, accounts, listen ports
./testenv/lab.sh logs slskdarr
./testenv/lab.sh down                  # stop, keep state; `destroy` wipes volumes too
```

`reset` rebuilds from the working tree, wipes all state and seeds Lidarr with exactly
150 wanted albums from a fixed artist list, so two runs are comparable. `up` keeps state.

- **Two distinct Soulseek accounts are required.** Soulseek permits one login per
  account and both clients log in regardless of backend. Never use your own account.
- The lab defaults to `SLSKDARR_BACKEND=soulseek` (the native client), which is the
  opposite of the app's own default in `config.example.toml`. That is deliberate — the
  native backend is the experimental one and the lab exists to exercise it.
- Results are not hermetic: peer availability and transfer speed vary between runs, so
  a `FAILED` job is evidence to investigate, not proof of a regression.
- Container logs echo the Soulseek usernames. Don't paste lab output verbatim into
  issues or PRs.
- `testenv/.env` and `testenv/runtime/` are gitignored and hold real credentials.

The observable surfaces are the dashboard on `:9090`, `/status`, `/api/events` (plain
JSON, polled — there is no SSE anywhere yet; that is #161) and the Postgres
database — `album_jobs` is the pipeline's only contact surface between modules,
so `SELECT state, count(*) FROM album_jobs GROUP BY state` is a literal snapshot
of the state machine, and `job_events` is the per-job history.

## Running the UI against the lab

The dashboard the lab serves on `:9090` is the *embedded* build — `lab.sh up` rebuilds the
image from the checkout, so seeing a frontend change there costs a full rebuild and a fresh
Soulseek login. That is far too slow to iterate on, and only one lab can run at a time.

For frontend work, run Vite from the checkout and let the lab be the backend:

```bash
./testenv/lab.sh up          # backend, once
make dev                     # http://localhost:5173, /api and /status proxied to :9090
```

`web/vite.config.ts` proxies `/api` and `/status` to `SLSKDARR_DEV_API` (default
`http://localhost:9090`) and injects the observ bearer token from `SLSKDARR_DEV_TOKEN`
(default: the lab's fixed token), so the browser never sees an auth prompt. The token is read
via `process.env` in the config file, which runs in Node — it never reaches the client bundle.
Keep it that way; a `VITE_` prefix would ship it to the browser.

Point `SLSKDARR_DEV_API` at another port when verifying a git worktree whose backend runs
somewhere else, and give Vite its own `--port` per worktree — two dev servers on one port
silently serve the same code and you verify the wrong branch.

Frontend changes need a browser before they are believable: `web/`'s tests run in jsdom, which
computes no layout and paints nothing, so overflow, popover placement, contrast and tap-target
size cannot fail the suite. Render the change and look at it. Agents should follow the
`verifying-ui-in-browser` skill.

## Configuration is strict

`internal/config` rejects unknown keys at startup (`unknown config keys: ...`) and has
no silent defaults for required fields. Combined with auto-deploy this means:

**If a change adds a required config key, that key must exist in production's
`config.toml` BEFORE the PR is merged.** Otherwise the container fails to start on the
next deploy.

`config.toml` in the repo root is gitignored and holds real credentials — never commit
it, never paste its contents. `config.example.toml` is the tracked template; update it
whenever you add a key.

## Database migrations

Migrations live in `internal/store/migrations/` as `%04d_description.sql`, embedded via
`go:embed` and applied in strictly increasing order inside their own transaction,
recorded in `schema_migrations`. A Postgres advisory lock prevents two instances racing
on startup.

- **A merged migration is immutable.** Never edit one after it has shipped; fix forward
  with a new migration.
- Anything that could lose data during a rolling deploy (dropping a column an older
  running instance still reads) is named `%04d_description_destructive.sql`. Those are
  never applied automatically — they need `slskdarr -migrate-destructive`.

## Issue tracker is Gitea, not GitHub

`origin` is `ssh://git@gitea.shcizo.se:2223/shcizo/slskdarr.git`. Use `tea`, not `gh`.

```bash
tea issues <n> --comments --output json
tea pulls create --head <branch> --base main --title "..." --description "$(cat body.md)"
```

Three traps that have each cost a round:

- **`tea pulls create` uses your currently checked-out branch as the PR head**, not the
  branch you name elsewhere. Always pass `--head` explicitly and verify afterwards with
  `tea pulls <n> --output json`.
- **`Closes #N` inside backticks does not auto-close the issue.** Gitea only parses the
  keyword in plain text.
- **`tea pulls merge` failing with `"failed to merge PR, is it still open?"` is a raw
  405 with the body swallowed.** It almost always means `main` moved and auto-merge no
  longer applies — check `mergeable` in `tea pulls <n> --output json` before anything
  else. Trying a different `--style` never helps.

## Git conventions

- Branch per issue: `feat/<description>-<issue>`, `fix/<description>-<issue>`,
  `chore/<description>`. Never work directly on `main`.
- Commit subject: `<type>: <description> (#<issue>)`.
- **Never `git add -A` in this repo.** Agent tooling drops untracked directories here
  (`.pi-subagents/`, `.remnic/`, `.serena/`, `.claude/worktrees/`) and a blanket add has
  already swept 278 unrelated files into a commit. Stage explicit paths.
- Deferred work becomes a Gitea issue or a comment on the issue that inherits it —
  never only a line in a spec.

## Layout

```
cmd/slskdarr/        main, wiring, signal handling, lifecycle
cmd/sqlite2pg/       one-off SQLite → Postgres migration tool
internal/core/       protocol-neutral domain types shared across adapters
internal/config/     strict TOML loading and validation
internal/store/      persistence, migrations, job/transfer state
internal/pipeline/   the state machine: WantedSync, Discovery, Selecting,
                     Downloading, Importing — each its own goroutine, DB is the
                     only contact surface
internal/soulseek/   native Soulseek protocol client (server, peer, distributed,
                     downloads, uploads/shares)
internal/slskd/      slskd HTTP adapter (alternative download backend)
internal/lidarr/     Lidarr HTTP adapter
internal/matcher/    candidate ranking
internal/observ/     HTTP server: /metrics, /status, dashboard APIs, embedded UI
internal/app/        use cases shared between HTTP and pipeline (Jobs.Cancel etc.)
web/                 React SPA source, built into internal/observ/web/dist
```

Adapters map their wire types to `internal/core` at the boundary. `internal/pipeline`
owns every interface it consumes — `DownloadingStore`, `PeerSearcher`, `MetricsSink`
and the rest are declared next to their consumer, and `cmd/slskdarr/main.go` injects
the concrete types — so backends can be swapped without touching use cases. It never
imports `internal/observ`, but the reverse wiring is normal and already in place:
`observ.Metrics` satisfies `pipeline.MetricsSink`. A new observation port follows that
same shape (nil sink = no-op), not a new import.

`internal/observ` deliberately does not import `internal/soulseek` — it declares its
own transport types and `main.go` adapts between them.

## Frontend build chain

- `internal/observ/web/dist/` is gitignored except the tracked `placeholder.html`. Vite
  overwrites `index.html` on every build, so tracking it makes the tree permanently
  dirty.
- `make ui` clears `dist/assets` and `dist/index.html` but keeps the placeholder —
  without that, orphaned hashed bundles accumulate into every binary via `go:embed all:`.
- `.dockerignore` must keep `web/node_modules/` excluded, or the host's darwin binaries
  overwrite the container's linux ones.

## Style

- Comments and identifiers in English. Doc comments explain *why* and what a caller
  needs to know, not what the signature already says.
- Match the surrounding file's style over any external guide.
- Exported Go symbols get a doc comment.

## Known noise

Two known failures. Neither is a regression, and neither may be "fixed" by weakening the
test. A failure that is *not* on this list means the branch broke something.

| Failure | Profile | Tracked as |
|---|---|---|
| `internal/store` `TestOpenRecyclesIdleConnections` | Fails under load. `go test ./internal/store/ -count=5` reproduces it; a single `go test ./...` usually does not, which is why agent runs report green | #171 |
| `internal/soulseek` `TestConnectPeerIndirectSuccess` | Fails only in container (Gitea act_runner). Invisible locally — a green local run is not evidence about it | #250 |

Both entries state where the failure is *visible*, because a green run under the wrong
conditions is not evidence. Verified 2026-07-30 on `main`: full `go test ./...` exits 0,
`npm test` is 362/362, and #171 still reproduces under `-count=5`.

Keep the list keyed on issue numbers: when one is closed, the entry stops being an excuse
and starts being a stale claim that hides a real failure. #242 (`Settings.test.tsx`
timing out under the full suite) was on this list until 2026-07-30 and is no longer —
it did not reproduce, and the issue is a candidate to close.
