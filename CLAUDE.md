# slusk

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
go test ./...            # run this before claiming anything works
go test ./... -race      # required for anything touching concurrency
```

`go vet ./...` and `gofmt -l .` should both be clean.

## Merging to main deploys to the canary

This is the single most important thing to know about this repo.

`.gitea/workflows/release.yml` runs on every push to `main`, reads conventional commit
prefixes since the last tag, and pushes a new `v*` tag. That tag triggers `deploy.yml`,
which builds the image, publishes it as `:vX.Y.Z` and `:edge`, and tells the homelab
updater to redeploy **the maintainer's own instance**.

| Prefix | Effect |
|---|---|
| `feat:` | minor bump → **live on the canary within minutes** |
| `fix:` | patch bump → **live on the canary within minutes** |
| `!:` or `BREAKING CHANGE` | major bump → live on the canary |
| `chore:`, `docs:`, `ci:`, `refactor:`, `style:`, `test:` | no bump, no build |

There is still no staging step, and the canary is not a test rig: it runs a real music
library against real Soulseek accounts, and it is deliberately allowed to break. "Merge
it and see" remains a production action — just a production of one.

Other people's instances do not move on a merge. `:latest` names the newest **promoted**
build and is repointed only by `promote.yml`, dispatched by hand from the Gitea Actions
UI with a version input. Promotion re-points `:latest` at the digest already running on
the canary — it never rebuilds — pushes a `promoted/vX.Y.Z` receipt tag, and creates a
GitHub release on the public `hyzr-dev/slusk` mirror. Rollback is the same workflow with
a lower version; it skips the release. See `docs/adr/0003-promote-by-digest.md`.

Two consequences worth carrying into every change:

- The release notes on a promotion are the **only** channel to the people running slusk.
  A change that adds a required config key stops their container from starting, and
  nothing else will warn them.
- Promoting is a judgement call about whether the canary looks healthy, and health here
  means `album_jobs` still moving — not that the process is up. A build can run for days
  and import nothing.

## The local PR lab is the substitute for staging

`testenv/` runs the full stack — this checkout's slusk, plus Lidarr, slskd and
Postgres — against real Soulseek searches, with no production data involved. Use it to
verify a PR before merging, since merging is the deploy.

```bash
cp testenv/.env.example testenv/.env   # first time: fill in two Soulseek test accounts
./testenv/lab.sh reset                 # clean run of the current checkout
./testenv/lab.sh info                  # addresses, accounts, listen ports
./testenv/lab.sh logs slusk
./testenv/lab.sh down                  # stop, keep state; `destroy` wipes volumes too
```

`reset` rebuilds from the working tree, wipes all state and seeds Lidarr with exactly
150 wanted albums from a fixed artist list, so two runs are comparable. `up` keeps state.

- **Two distinct Soulseek accounts are required.** Soulseek permits one login per
  account and both clients log in regardless of backend. Never use your own account.
- The lab defaults to `SLUSK_BACKEND=soulseek` (the native client), matching the value
  `config.example.toml` ships and the README recommends. `pipeline.backend` itself has
  **no default at all** — it is required (#396), so "the app's default" is not a thing
  to reason about; the example template's value is what a new user actually gets.
  Both backends carry a caveat, for different reasons, and neither should be described
  as the proven one: the native client is young and under active development, and
  slusk's slskd *adapter* (not slskd, a mature project) is a small, static piece of code
  on a fraction of the test coverage that the lab does not exercise by default.
- Results are not hermetic: peer availability and transfer speed vary between runs, so
  a `FAILED` job is evidence to investigate, not proof of a regression.
- Container logs echo the Soulseek usernames. Don't paste lab output verbatim into
  issues or PRs.
- `testenv/.env` and `testenv/runtime/` are gitignored and hold real credentials.

The observable surfaces are the dashboard on `:9090`, `/status`, two separate HTTP
endpoints that both carry job data, and the Postgres database.

- `/api/stream` is server-sent events (`internal/observ/stream.go`, issue #161): live
  per-job frames, throughput samples, manual-search deltas, and an `invalidate` signal
  that tells subscribers *when* to refetch, never what to render. The frontend consumes
  it with one shared `EventSource` in `web/src/api/stream.tsx`.
- `/api/events` is still plain polled JSON, and always was — it backs the event
  timeline. The stream did not replace it; it was added alongside.

In the database, `album_jobs` is the only contact surface *between the pipeline's
stages* — no stage imports another. It is not the only boundary in the system: the SSE
layer is the second contact surface, the one facing the frontend. `SELECT state,
count(*) FROM album_jobs GROUP BY state` is a literal snapshot of the state machine, and
`job_events` is the per-job history.

## Running the UI against the lab

The dashboard the lab serves on `:9090` is the *embedded* build — `lab.sh up` rebuilds the
image from the checkout, so seeing a frontend change there costs a full rebuild and a fresh
Soulseek login. That is far too slow to iterate on, and only one lab can run at a time.

For frontend work, run Vite from the checkout and let the lab be the backend:

```bash
./testenv/lab.sh up          # backend, once
make dev                     # http://localhost:5173, /api and /status proxied to :9090
```

`web/vite.config.ts` proxies `/api` and `/status` to `SLUSK_DEV_API` (default
`http://localhost:9090`) and injects the observ bearer token from `SLUSK_DEV_TOKEN`
(default: the lab's fixed token), so the browser never sees an auth prompt. The token is read
via `process.env` in the config file, which runs in Node — it never reaches the client bundle.
Keep it that way; a `VITE_` prefix would ship it to the browser.

Point `SLUSK_DEV_API` at another port when verifying a git worktree whose backend runs
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
  never applied automatically — they need `slusk -migrate-destructive`.

## Issue tracker is Gitea, not GitHub

`origin` is `ssh://git@gitea.shcizo.se:2223/shcizo/slusk.git`. Use `tea`, not `gh`.

```bash
tea issues <n> --comments --output json
tea pulls create --head <branch> --base main --title "..." --description "$(cat body.md)"
```

**Read `docs/agents/issue-tracker.md` before any tracker operation.** It carries the full
command vocabulary and three traps that have each cost a round — the `--head` flag being
ignored, `Closes #N` in backticks not closing anything, and `tea pulls merge` returning a
body-less 405 when `main` has moved.

## Agent skills

### Issue tracker

Gitea on `gitea.shcizo.se`, driven through the `tea` CLI — never `gh`. See
`docs/agents/issue-tracker.md`.

### Triage labels

The five canonical roles, each label string equal to its name. See
`docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root, both created lazily. See
`docs/agents/domain.md`.

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
cmd/slusk/           main, wiring, signal handling, lifecycle
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
internal/observ/     HTTP server: /metrics, /status, /api/stream (SSE), dashboard
                     APIs, embedded UI
internal/app/        transport-neutral services between the store and the HTTP edge;
                     owns the manual, user-triggered state transitions where pipeline
                     owns the automatic ones — a sibling of pipeline over the same
                     table, not code shared with it
web/                 React SPA source, built into internal/observ/web/dist
```

Adapters map their wire types to `internal/core` at the boundary. `internal/pipeline`
owns every interface it consumes — `DownloadingStore`, `PeerSearcher`, `MetricsSink`
and the rest are declared next to their consumer, and `cmd/slusk/main.go` injects
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

## Design Context

### Users

Other self-hosters running slusk on their own hardware. They know Lidarr and
Soulseek; they do not know slusk's internal state machine, and the interface must
not assume they do — a state name that only makes sense if you have read
`internal/pipeline` is a bug in the interface, not a gap in the user.

They arrive when something is wrong. The dominant job is **active troubleshooting**:
why is this job stuck, why did this album match the wrong release, why has nothing
imported for an hour. Passive glance-at-it monitoring is a secondary use, so density
and information scent beat calm reassurance — the screen should let someone chase a
specific job to its cause without leaving the dashboard.

### Brand Personality

**Terminal with attitude.** Three words: *dense, candid, deliberate.*

- Dense: a TUI's information-per-pixel, not an app's. Whitespace is earned, not default.
- Candid: it reports what actually happened, including failure, in the same flat voice
  it reports success. No reassuring euphemism, no invented certainty
  (`interface-must-not-invent-data` — omit what the backend cannot supply).
- Deliberate: personality is allowed — the pulsing LIVE dot, the tick flare on a
  completed segment, an occasional dry line of copy — but every flourish must be
  attached to a real state change. Motion that means nothing is noise.

What it is *not*: a SaaS admin panel (cards, drop shadows, rounded pills, illustrated
empty states), and not retro-CRT pastiche either (no scanlines, no phosphor glow). The
idiom is a *modern* terminal — htop and k9s, not a VT220 emulator.

### Aesthetic Direction

**Dark only, permanently.** The palette *is* the design. Do not add
`prefers-color-scheme` branches, do not build a light theme, do not add tokens whose
only purpose is to make one possible later.

- The visual spec is `docs/design/slusk-tui.dc.html` (plus the dashboard mock beside
  it) and `docs/superpowers/specs/2026-07-25-tui-reskin-design.md`. Grep the local
  files; they are the source of truth for spacing, weight and hue.
- Every colour lives in `web/src/styles/tokens.css`. A raw hex in a `.module.css` is
  invisible to anything that reasons about the palette — `npm test` runs
  `scripts/check-css-tokens.mjs`, which fails the build on an undefined token, but it
  cannot catch a hardcoded one. Name it or don't use it.
- Colour is a scarce signal, not decoration. Status has no per-state hue: queued,
  active and importing all render in `--fg`/`--dim`. Only `--ok` and `--bad` carry a
  colour, which is exactly why they read at a glance.
- The mock **recesses, never elevates** — nested content goes darker (`--panel-inset`),
  not lighter. There are no shadows.
- One typeface: IBM Plex Mono. Hierarchy comes from weight, size, letter-spacing and
  the `--fg` → `--dim` → `--text-dim` ladder — never from a second family. The ladder is
  three steps, not four: `--faint` is not a token and never has been in `tokens.css`. The
  design mocks use `--faint` freely, so translating one means collapsing it onto this
  ladder — `check-css-tokens.mjs` will fail the build if you copy the name across.

### Accessibility

Desktop-first, and honestly so. Mobile is not a priority: tables must not explode, but
small screens are not optimised for. Keyboard operability is nice-to-have and welcome
where it fits the terminal idiom, not a gate.

WCAG AA contrast is the standing goal for text and is enforced, not aspirational:
`npm test` runs `scripts/check-contrast.mjs`, which fails the build if any text token in
`tokens.css` drops below 4.5:1 against any surface it can land on, or if adjacent steps
of the `--fg` → `--dim` → `--text-dim` ladder sit closer than 8 L\* apart (#335).
`--text-dim`, the bottom of that ladder and the one used for column headers, timestamps
and disabled glyphs, currently clears AA on every surface it's checked against. Keep it
that way: introducing a token that only passes against `--bg` and not the rest of
`SURFACES` in that script is exactly the regression it exists to catch.

`@media (prefers-reduced-motion: reduce)` already exists in `global.css`. Any new
animation must be covered by it.

### Design Principles

1. **Density is the feature.** When in doubt, fit more on the screen, not less. A user
   scanning for a stuck job should not have to scroll to compare two rows.
2. **Colour is signal.** If a hue does not distinguish success from failure or draw the
   eye to something actionable, it does not belong.
3. **Never invent data.** An absent field beats a plausible fabrication. Omit, or say
   plainly that it is unknown.
4. **Motion earns its place.** Animate state changes, nothing else — and always honour
   reduced-motion.
5. **The mock is the spec, not a mandate to delete.** Match its visual language; never
   remove shipped functionality merely because the mock does not draw it.
6. **Layout bugs are invisible to the suite.** jsdom computes no layout and paints
   nothing, so overflow, popover placement, contrast and tap-target size can never fail
   a test. Render the change in a browser before calling it done — follow the
   `verifying-ui-in-browser` skill.

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
conditions is not evidence. Verified 2026-07-30: full `go test ./...` exits 0, `npm test` is
364/364, and #171 still reproduces under `-count=5`. Do not treat a stated test count as
current — it was 362 earlier the same day, before the #285 web tests landed. Run the suite.

Keep the list keyed on issue numbers: when one is closed, the entry stops being an excuse
and starts being a stale claim that hides a real failure. #242 (`Settings.test.tsx`
timing out under the full suite) was on this list until 2026-07-30 and is no longer —
it did not reproduce, and the issue is a candidate to close.
