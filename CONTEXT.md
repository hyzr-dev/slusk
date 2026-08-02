# slskdarr

A bridge between Lidarr and Soulseek. Lidarr says which albums are wanted; slskdarr
finds them on Soulseek, downloads them, and hands the finished album back to Lidarr for
import. Everything the system does is a state transition on one album job.

## Language

### The pipeline

**Album job**:
One album being carried from wanted to imported. The only unit of work the system
schedules, and the only contact surface between pipeline steps.
_Avoid_: task, item, download (a download is one part of a job)

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

**Backend**:
The Soulseek implementation in use — the native protocol client, or slskd over HTTP.
A deployment picks exactly one; both satisfy the same ports.
_Avoid_: provider, driver, client (client also means the dashboard)

**Parked**:
A job set aside because no candidate could satisfy it, distinct from failed. Parked
jobs await a person's decision; failed jobs await a retry.
_Avoid_: stalled, blocked, held

**Not imported**:
A job whose files arrived but which Lidarr would not take. The download succeeded and
the album is still absent from the library.
_Avoid_: failed, rejected

### The dashboard

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
