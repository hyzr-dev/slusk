# slusk

A bridge between Lidarr and Soulseek. Lidarr says which albums are wanted; slusk
finds them on Soulseek, downloads them, and hands the finished album back to Lidarr for
import. Everything the system does is a state transition on one album job.

## Language

### The pipeline

**Album job**:
One album being carried from wanted to imported. The only unit of work the system
schedules, and the only contact surface between pipeline steps.
_Avoid_: task, item, download (a download is one part of a job)

**Job state**:
The lifecycle value stored on the job row — `WANTED`, `SELECTING`, `DOWNLOADING`,
`IMPORTING`, `DONE`, `PARKED`, `FAILED`, `CANCELLED`, `NOT_IMPORTED`, `IMPORT_REFUSED` —
and the only vocabulary the pipeline itself reads or writes. Not the same as dashboard status:
several statuses are derived from transfer activity rather than the job row, and one
job state can present as more than one status.
_Avoid_: status on its own (ambiguous with dashboard status)

**Source**:
What caused an album job to exist — a Lidarr wanted-sync, or a person creating one by
hand. A manual job skips discovery because its candidate is already chosen.
_Avoid_: origin, type

**Candidate**:
One peer's offer of a complete album, ranked against the others. A job may try several
in turn; rejecting one does not fail the job.
_Avoid_: result, match, option

**Transfer**:
One file moving from one peer to this machine. A candidate becomes many transfers.
_Avoid_: download (ambiguous between the file and the whole job)

**Remote folder**:
The directory a peer shares a candidate's files from. A name slusk observes and copies,
never chooses — and one that says nothing about the album, since a peer is free to call
it `cd1` or `FLAC (24bit-44.1kHz)`.
_Avoid_: album folder, source folder

**Download folder**:
The directory a job owns beneath the local download root, named after the remote folder
it copies. Only one live job may own one at a time: owning it is what confers the right
to write into it and, later, to delete it. It is what Lidarr is pointed at to scan, and
it carries whatever name the peer chose — so it is not a promise that one album, or only
one album, is inside it.
_Avoid_: album folder (it holds what a peer put in one directory, which is not the same
as one album — reading it the other way is what #280 lived in), leaf, target folder,
complete dir

**Backend**:
The Soulseek implementation in use — the native protocol client, or slskd over HTTP.
A deployment picks exactly one; both satisfy the same ports.
_Avoid_: provider, driver, client (client also means the dashboard)

**Parked**:
A job set aside because no candidate could satisfy it, distinct from failed. Parked
jobs await a person's decision; failed jobs await a retry.
_Avoid_: stalled, blocked, held

**Not imported**:
A job whose download completed but that never reached Lidarr at all — either it was
never identified against a MusicBrainz release group, or the identified release group is
not in Lidarr's library. Manual-jobs-only, and not a failure: nothing went wrong, Lidarr
was simply never asked. Never retried, never treated as an error.
_Avoid_: failed, rejected, import refused (that term is for a job Lidarr *did* see and
turned away — see Import refused)

**Import refused**:
A job whose download was complete and correct, and which Lidarr permanently refused to
accept. The files are kept; the job awaits a person.
_Avoid_: not imported (that term is for a job that never reached Lidarr — see Not
imported), failed, rejected

### The dashboard

**Dashboard status**:
What the interface calls a job. Derived from job state and, for some statuses, transfer
activity — never stored. Not a rename of job state: several statuses come from
transfer activity rather than the job row, and one job state can present as more than
one status.
_Avoid_: state on its own (that is the database's word, see Job state)

**Wanted**:
Dashboard status for a job never yet searched — no candidates cached.
_Avoid_: new, unstarted

**Selecting**:
Dashboard status for a job with candidates cached, waiting for a `max_active` slot to
open.
_Avoid_: ranking, picking

**Queued**:
Dashboard status for a job with a candidate chosen but no file from it completed yet —
the request is sitting in a peer's queue waiting for the first file to arrive.
Deliberately the narrow, literal sense: what a Soulseek user already expects "queued"
to mean.
_Avoid_: preparing, starting, initializing (nothing is happening on our side; the peer
holds the initiative)

**Waiting**:
Dashboard status for a job with at least one file from the candidate already arrived,
and none moving right now.
_Avoid_: pending (collides with `core.TransferPending`, a real per-file transfer state
rendered in the job detail panel)

**Manual search**:
A search a person starts by hand against Soulseek, independent of any wanted album.
Its results are grouped per peer and can be turned into a manual album job.
_Avoid_: query, lookup

**Transport**:
The mechanism that carries live data to connected dashboards — connection lifetime,
subscription scope, delivery under load. It carries payloads; it does not know what
any of them mean.
_Avoid_: stream (names the wire, not the concept), channel, hub

**Publisher**:
The owner of one kind of live payload, which hands it to the transport. A publisher
knows its own domain; the transport knows none.
_Avoid_: emitter, producer, sender

**Slice**:
Everything belonging to one feature — its handlers, its rules, its payload shape —
kept together rather than split across a layer per concern. A slice owns what it
publishes and what it serves.
_Avoid_: module (too general here), feature folder, vertical

### Access

Two unrelated credentials reach the same endpoints, so a bare "token" is always
ambiguous — name which one.

**Session**:
A person's proven login, held as an opaque value in their browser and as a row the
server owns. It belongs to the one account and can be revoked. Never say "session
token" for it — that invites the assumption it is signed, which it is not (ADR-0001).
_Avoid_: token, JWT, auth token

**Machine token**:
The optional shared secret in `observ.auth_token`, for callers that are not a browser —
curl, Prometheus, the Vite dev proxy. It proves no identity and belongs to no account.
_Avoid_: API key, password, token

**Setup**:
The one-time creation of the only account, offered while none exists and closed
permanently once one does. There is deliberately no second account and no password
change; recovery means deleting the account and doing setup again.
_Avoid_: registration, sign-up, onboarding

### Delivery

Building a version, running it, and releasing it are three separate events. A single
word for "it shipped" hides which one happened.

**Canary**:
The maintainer's own instance — the only one running builds before anyone else does. It
holds a real library and real Soulseek accounts, so it is production for exactly one
person and is allowed to break. It is the sole detector of faults that only appear under
real use.
_Avoid_: staging, test instance, dev (none of them run real data)

**Edge**:
Every build from `main`, promoted or not, published as `:edge`. What the canary runs.
_Avoid_: nightly, unstable, dev build

**Promoted**:
A version whose digest `:latest` points at, chosen by hand after it has proven itself on
the canary. The receipt is a `promoted/vX.Y.Z` tag. Promotion re-points an existing
image; it never produces a new one.
_Avoid_: released, stable, published (a build is published the moment it exists)
