# slusk

A bridge between [Lidarr](https://lidarr.audio/) and [Soulseek](https://www.slsknet.org/).

slusk polls Lidarr for wanted albums, searches Soulseek for them, downloads the
candidates that look best, and hands finished albums back to Lidarr for import. It speaks
the Soulseek protocol itself, so it needs no
[slskd](https://github.com/slskd/slskd) — though it can still drive one if you already
run it.

It also works the other way round: search Soulseek yourself from the dashboard, pick the
peer and the files, and let the same pipeline download them.

Licensed under [AGPL-3.0-or-later](LICENSE).

## How it compares to soularr + slskd

[soularr](https://github.com/mrusse/soularr) solves the same problem and is the reason
this project exists. Every row below was checked against soularr's current source rather
than its reputation.

| | soularr + slskd | slusk |
|---|---|---|
| Pipeline state | re-derived from Lidarr and slskd on every run; nothing tracks a transfer the script itself was watching when it died | a state machine in Postgres (`album_jobs`), so a restart picks up in-flight jobs instead of stranding them |
| Soulseek client | slskd required | its own Soulseek client, no slskd needed; slskd still supported for those already running it |
| Dashboard | edits the config, tails the log, lists failed imports; job and transfer state you watch in slskd's UI | jobs, candidates, peers, events, throughput and writable settings in one place |
| Candidate choice | sequential filters plus a filename-similarity ratio | weighted scoring over format, bitrate, file count and peer reliability, including a decayed per-artist history of which peers actually delivered |
| Manual downloads | none; Lidarr's wanted list is the only input | search Soulseek yourself, pick the peer and files, optionally identify the result against MusicBrainz first |
| Processes | slskd, plus soularr's own interval loop and its web UI | one binary and a Postgres |
| Licence | GPL-3.0 | AGPL-3.0-or-later |

Where soularr is ahead, or where slusk costs you something:

- **soularr is proven and slusk is not.** soularr has been run by a lot of people for a
  long time. slusk has not.
- **Postgres is a real dependency.** soularr needs a `config.ini` and a place to run.
  slusk needs a database, which is more to operate and more to back up.
- **slusk's Soulseek client is young.** slskd has years of use behind it; slusk's own
  protocol implementation does not, and it is the path this README recommends.
- **A manual download still imports only into an album Lidarr already knows.** slusk
  will download anything you point it at, but importing it needs the release to exist in
  your Lidarr library first.

## Setup

### Prerequisites

- Lidarr, reachable from the container, and its API key.
- Docker with Compose.
- slskd and its API key — unless you use the native Soulseek backend, in which case you
  need Soulseek account credentials instead.
- Postgres. The compose file below bundles one; point it at your own if you'd rather.

### 1. Copy the compose file and the config

```bash
cp docker-compose.example.yml docker-compose.yml
mkdir -p config && cp config.example.toml config/config.toml
```

`docker-compose.example.yml` is the only compose file in the repo, and every deployment
variant it supports — external Postgres, an existing arr network, gluetun with VPN port
forwarding, building from source — is a commented block at the bottom of it. Your
`docker-compose.yml` copy is gitignored, so local edits stay yours.

`config/config.toml` must be a **directory** mount, not a single-file mount, if you want
to edit settings from the dashboard: slusk writes the file back with an atomic rename,
which a bind-mounted single file cannot survive. Mounting one file read-only still
works and simply makes settings read-only in the UI.

### 2. Fill in the config

At minimum: `lidarr.url` and `lidarr.api_key`, the `[slskd]` section (or `[soulseek]`,
see below), and `store.dsn`. The default DSN already matches the Postgres the compose
file bundles.

**Configuration is strict.** slusk rejects unknown keys at startup with
`unknown config keys: ...` and has no silent defaults for required fields. A typo in a
key name stops the container rather than being quietly ignored. That is deliberate, and
worth knowing before a startup failure looks like a bug.

Keep `config/config.toml` out of source control. It holds API keys.

### 3. Choose a backend

`pipeline.backend` decides how slusk reaches Soulseek:

- `"soulseek"` — **the recommended setting.** slusk's own client connects to the
  Soulseek server directly and no slskd is involved at all. Requires a `[soulseek]`
  section; the `[slskd]` section becomes unnecessary.
- `"slskd"` — slskd does the searching and transferring. Requires the `[slskd]` section.
  Worth choosing if you already run slskd and want to keep it. slusk's slskd adapter is
  experimental.

`pipeline.backend` is **required and has no default.** Leaving it out is a startup error,
not a silent choice — it decides which client performs every search and transfer, which
is too consequential to inherit from a key you never wrote.

The native client is the newer of the two and the one under active development — it is
also what this project's own pre-merge test lab exercises by default, so in practice it
sees more real Soulseek traffic than the slskd path does. The slskd adapter is a much
smaller, more static piece of code: it does everything the pipeline asks of it, but it
carries a fraction of the test coverage and gets no new feature work. Neither of those
is a claim that the native client is finished; it is the honest ordering of which path
is better exercised.

With the native backend, slusk writes the completed downloads itself, so
`paths.slskd_complete_dir` must be a **writable** volume shared with Lidarr at the path
Lidarr expects. With the slskd backend it only ever reads that path.

The native backend also needs incoming peer connections to reach it. Forward
`soulseek.listen_addr`'s port — the published host port **must equal** the container
port, because there is no separate "advertised port" concept and a mismatched mapping
advertises a port nobody can reach. If you route Soulseek through a VPN instead, see
[Running the native backend behind gluetun](#running-the-native-backend-behind-gluetun).

### 4. Start it

```bash
docker compose up -d
```

Then open `http://localhost:9090`. The first visit asks you to create an account.

### 5. Machine access, and `observ.auth_token`

`/healthz`, `/readyz` and the dashboard shell are public. Everything else — the API,
`/status`, `/metrics` — needs either a browser session or `observ.auth_token`.

The token is optional and exists for non-browser clients: `curl`, a Prometheus scrape,
the Vite dev proxy. Leave it blank to disable machine access entirely. Generate one with
`openssl rand -hex 32`, send it as `Authorization: Bearer <token>`, and keep it out of
URLs and logs. `/metrics` accepts the token only — never a session cookie — so blanking
it breaks an existing Prometheus scrape.

Terminate TLS at a trusted reverse proxy before exposing the listener beyond a private
network. The proxy must preserve `Host`, discard any client-supplied
`X-Forwarded-Proto`, and set exactly one trusted value; that header also decides whether
the session cookie is marked `Secure`.

### Running the native backend behind gluetun

Soulseek needs peers to be able to open connections *to* you, which is the awkward part
of putting it behind a VPN: you need a provider and a gluetun setup that supports port
forwarding, and the port you get is assigned dynamically rather than chosen.

slusk handles that by asking gluetun what the port is. Set `[soulseek.gluetun]` in the
config and, at startup, it fetches `GET {control_url}/v1/portforward` and listens on the
port gluetun reports. Only the port half of `soulseek.listen_addr` is replaced — the host
half is still used — so the static port you configured is ignored in this mode.

```toml
[soulseek.gluetun]
control_url = "http://127.0.0.1:8000"
api_key = "CHANGEME"
```

This only works if slusk shares gluetun's network namespace, so that `127.0.0.1` reaches
the control server and inbound peer connections arrive on the forwarded port. In compose
that means `network_mode: "service:gluetun"` on the slusk service, no `ports:` section of
its own, and 9090 published on the gluetun service instead. The compose file has the
whole thing as a commented block.

`network_mode: "service:<name>"` is **not supported by Docker Swarm.** This is a plain
`docker compose` arrangement only.

Two things worth knowing before you debug it at 2am:

- **gluetun ≥ v3.40 requires auth configuration on its own side** for the
  `/v1/portforward` route. Without it every fetch gets 401 forever and slusk never starts
  listening. Grant the route to the same key you put in `soulseek.gluetun.api_key`; on
  older gluetun no auth is needed and the key is simply unused. slusk names the config
  key in the error when it sees a 401 or 403, so the log will say which knob to turn.
- **The port is fetched once, at startup, and never again.** If gluetun's forwarded port
  changes while slusk is running — a reconnect, a server change — slusk keeps listening
  on the old one and quietly stops being reachable. Restart slusk to pick up the new port.

Starting slusk and gluetun together is fine. A control server that isn't up yet, or a
gluetun that reports port 0 because forwarding hasn't been established, are both treated
as transient: slusk retries with exponential backoff (from 5s, capped at 10 minutes,
each attempt bounded at 5s) and logs a warning per attempt until it gets a real port.

### If you lock yourself out

There is deliberately no password reset and no password change, and there is exactly one
account, permanently — the setup window closes for good once the first account is
created, and nothing in the UI can add a second user.

The only way back in is to delete the account and run first-time setup again:

```sql
DELETE FROM users;
```

That is safe. `user_sessions.user_id` cascades on delete, so active sessions go with it,
and no other table references `users` — none of your pipeline data is touched. After it,
slusk reports setup as required again and the dashboard asks for a new account.

## Manual downloads

The dashboard is not only a window onto the automatic pipeline. From `/search` you can
search Soulseek directly, browse what peers are offering, and create a job from a
specific peer and file selection. That job then runs through the same download stage as
everything else, with the same retries and the same visibility.

Before creating the job you can identify a result against MusicBrainz — it resolves the
canonical artist and album, lists the editions of a release group with their track
counts so you can judge whether a folder is actually complete, and tells you whether the
release is already in your Lidarr library. This needs a `[musicbrainz]` section in the
config; without one, identification is simply unavailable.

Import is the one place a manual job differs. slusk imports a completed manual download
into Lidarr only when it can resolve it to an album already in your library, via the
MusicBrainz id attached at creation. If there is no id, or Lidarr doesn't know that
release, the job ends as `NOT_IMPORTED` and the files stay on disk for you to deal with.
The dashboard can add an *artist* to Lidarr for you, but nothing in the manual flow
creates library entries on its own.

## Architecture

The pipeline is five stages, each its own goroutine on its own interval:

```
WantedSync → Discovery → Selecting → Downloading → Importing
```

`WantedSync` refreshes the wanted list from Lidarr. `Discovery` searches Soulseek, ranks
the results and caches them as candidates. `Selecting` activates the next cached
candidate for a job, or backs the job off when its cache is exhausted or stale.
`Downloading` transfers the files, keeping only a few in flight per peer so a peer's own
queue limit doesn't reject the burst. `Importing` hands the finished folder to Lidarr and
confirms the import actually landed.

No stage imports another. The `album_jobs` table is the only contact surface between
them, which means the state of the entire system is a query:

```sql
SELECT state, count(*) FROM album_jobs GROUP BY state;
```

`job_events` holds the per-job history behind that snapshot.

Adapters — Lidarr, slskd, the native Soulseek client — map their wire types to a
protocol-neutral domain at the boundary, so a backend can be swapped without touching
the pipeline.

`CLAUDE.md` goes deeper, including the local PR lab in `testenv/` that runs the whole
stack against real Soulseek searches.

## Building from source

Go 1.26.3 and Node 22.

```bash
make build   # builds the web UI, then the binary
make test    # go test ./... && npm test
```

To build the container instead, replace the `image:` line in your compose file with
`build: .` and run `docker compose up -d --build`.
