# Issue tracker: Gitea

Issues and PRDs for this repo live in Gitea at `gitea.shcizo.se:2223/shcizo/slusk`.
Use the `tea` CLI for all operations. **Never `gh`** — there is no GitHub remote.

`tea` infers the repo from the `origin` remote when run inside the clone.

## Conventions

- **Create an issue**: `tea issues create --title "..." --description "$(cat body.md)"`.
  The flag is `--description` / `-d`, *not* `--body`. Write multi-line bodies to a temp
  file first; shell quoting of long markdown through `-d` is a reliable way to lose it.
- **Read an issue**: `tea issues <n> --comments --output json`
- **List issues**: `tea issues list --state open --limit 200 --output json`, with
  `--labels`, `--milestones`, `--author`, `--assignee` as filters. Add
  `--fields index,title,state,labels,body,comments` to widen the default field set.
  **Always pass `--limit`** — see the pagination trap below.
- **Comment**: `tea comment <n> "..."` (shorthand for `tea comments add`).
- **Apply / remove labels**: `tea issues edit <n> --add-labels "..."` /
  `--remove-labels "..."`. Plural. Comma-separated, no repeated flags.
- **Close**: `tea issues close <n>`. It takes **no** comment flag — comment first with
  `tea comment <n> "..."`, then close.

## Traps that have each cost a round

- **`tea issues list` silently stops at 30.** There is no truncation notice, no total
  count, and no next-page hint — a backlog of 46 returns 30 rows that look complete.
  Every conclusion drawn from an unlimited list is therefore suspect: a triage session
  reported "30 open issues" and only discovered the other 16 when an issue it had just
  labelled failed to appear in the listing. Pass `--limit 200` (or higher than the
  backlog can plausibly be) on every `list`, and treat a result of exactly 30 as a
  truncation until proven otherwise. `--labels` filters page independently, so a
  filtered list can surface issues the unfiltered one omits.
- **`tea pulls create` uses the currently checked-out branch as the PR head**, not the
  branch named elsewhere. Always pass `--head` explicitly, then verify with
  `tea pulls <n> --output json`.
- **`Closes #N` inside backticks does not auto-close the issue.** Gitea only parses the
  keyword in plain text.
- **`tea pulls merge` failing with `"failed to merge PR, is it still open?"` is a raw 405
  with the body swallowed.** It almost always means `main` moved and auto-merge no longer
  applies — check `mergeable` in `tea pulls <n> --output json` first. A different
  `--style` never helps.

## Merging a PR deploys to the canary

`main` auto-tags and auto-deploys on `feat:` / `fix:` / breaking-change commits. There is
no staging step, so closing a ticket by merging puts the change on the maintainer's live
instance — see the deploy table in `CLAUDE.md` and verify in `testenv/` first. It does
*not* reach anyone else: `:latest` moves only when `promote.yml` is dispatched by hand.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external PRs as
feature requests; `/triage` reads this flag.)_

Gitea shares one number space across issues and PRs, so a bare `#42` may be either —
resolve with `tea pulls 42` and fall back to `tea issues 42`.

## When a skill says "publish to the issue tracker"

Create a Gitea issue with `tea issues create`.

## When a skill says "fetch the relevant ticket"

Run `tea issues <n> --comments --output json`.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a single issue with **child** issues as tickets.

Gitea has no native sub-issue or issue-dependency API equivalent to GitHub's, so both
relationships are represented in the issue bodies:

- **Map**: an issue labelled `wayfinder:map` holding the Notes / Decisions-so-far / Fog
  body, plus a task list of its children.
- **Child ticket**: an issue whose body opens with `Part of #<map>`, added to the map's
  task list. Labels: `wayfinder:<type>` (`research`/`prototype`/`grilling`/`task`).
  Once claimed, assigned to the driving dev.
- **Blocking**: a `Blocked by: #<n>, #<n>` line at the top of the child body. A ticket is
  unblocked when every listed blocker is closed — check with
  `tea issues <n> --output json` and read `state`.
- **Frontier query**: `tea issues list --state open --labels wayfinder:task ...`, scoped
  to the map's task list; drop any with an open blocker or an assignee; first in map
  order wins.
- **Claim**: `tea issues edit <n> --add-assignees shcizo` — the session's first write.
- **Resolve**: `tea comment <n> "<answer>"`, then `tea issues close <n>`, then append a
  context pointer to the map's Decisions-so-far.

Anything `tea` cannot express is reachable through `tea api` (authenticated raw request
against the Gitea API).
