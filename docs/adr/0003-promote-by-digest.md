# `:latest` is moved by hand onto an already-built digest

Until #401 a merge to `main` was one event with two audiences: it deployed to the
maintainer's instance and it published `:latest`, which every other self-hoster tracks
through `docker-compose.example.yml`. That was defensible while nobody else ran slusk. It
stopped being defensible when the image went public, because it meant a `fix:` merged at
midnight reached strangers before anyone had seen it run.

The split is deliberately *not* staging → prod. There is no second stack and no plan for
one: the maintainer's instance keeps running real data, keeps being first, and is
expected to break. What changed is only that it is now the *sole* consumer of a fresh
build. `:latest` names the newest build that has survived there, and moves only when
`promote.yml` is dispatched by hand. Builds from `main` land on `:edge` for anyone who
wants them sooner.

## Promotion re-points a digest and must never rebuild

`docker buildx imagetools create -t …:latest …:vX.Y.Z` is a registry-side manifest copy:
no layers move, no builder runs, and the digest behind `:latest` is byte-identical to the
one that has been running on the canary.

Rebuilding from the same git tag looks equivalent and is not. `Dockerfile` resolves a
base image and `npm ci` fetches from the network, so a rebuild two weeks later is a
different artifact — and the soak that justified the promotion was evidence about the
*old* artifact only. The temptation to rebuild will recur, usually phrased as "so we pick
up base-image security fixes for free". That trade is not available here: taking those
fixes means promoting something unrun, which is the exact thing this workflow exists to
prevent. Fix forward with a commit instead, and let it soak like anything else.

A second reason is specific to this repo. `deploy.yml` bakes `VERSION` into the binary at
build time and `/status` reports it, so a re-pointed `:latest` truthfully answers "which
release am I running" with the version that was tested. A rebuild could make a user's
`/status` name something no one has ever run.

## The trigger is a human, not a timer

An automatic "promote anything that has been up N days" rule was considered and rejected.
It cannot read this system's health. slusk can hold its process open, serve the
dashboard, pass any liveness probe, and still have stopped importing — the only honest
signal is whether `album_jobs` is still moving through its states, which takes a person
looking. A timer would faithfully promote a build that had quietly done nothing for a
week.

For the same reason there is no guard rail checking that the requested version ever ran
on the canary. It was specified and then dropped: the only failure it caught was a typo
in the dispatch input, which shows up immediately as the wrong version on `/status` and
costs a second click. The machinery was not worth the one mistake it prevented.

## Consequences

- `:latest` was redefined rather than replaced by a new `:stable`. The change can only
  make an existing user's instance more conservative, and needs no action from them. A
  new tag would have left the unsafe channel as the default, which is where nobody looks.
- The GitHub release created at promotion is the only channel to the people running
  slusk. Its changelog spans previous-promoted → target, often several versions, because
  that is the jump a user actually takes. `internal/config` rejects unknown and missing
  required keys, so a release that adds one stops their container from starting, and
  these notes are the entire warning system for that.
- Rollback is the same workflow with a lower version. It moves the digest and the
  `promoted/` receipt but skips the release: a release announces what is new and cannot
  express a withdrawal. Say that in the broken version's existing release instead.
- The `promoted/vX.Y.Z` receipts are annotated tags, because a lightweight tag inherits
  its target commit's date — which would order the receipts by when the code was written
  rather than by when it was promoted. Those two orders disagree precisely after a
  rollback, which is when the lookup matters.
- The loop is one-way for now. `github.com/hyzr-dev/slusk` is the public face and has
  Issues enabled, but nothing carries a report from there into Gitea (#402).
