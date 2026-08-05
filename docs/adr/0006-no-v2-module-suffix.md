# The module path stays `github.com/hyzr-dev/slusk`, without a `/v2` suffix

`go.mod` declares `module github.com/hyzr-dev/slusk` and has since v1. The repo has since
tagged v2 and beyond, which Go's semantic import versioning says is wrong: from v2
onward, the major version belongs in the module path (`.../slusk/v2`), and every import
across the module must carry it too. slusk does not do this, and will not.

## Why the convention does not pay for itself here

Semantic import versioning exists to let two versions of the same library coexist in one
build, so an importer pulling in v1 through one dependency and v2 through another gets
both without a conflict. That protects *importers* across a breaking change. slusk has
none — it is a deployed binary (`cmd/slusk`), not a library anyone vendors. There is
nobody for the `/v2` suffix to protect.

Complying anyway costs a real refactor, not a formality: adding the suffix touches 421
import lines across 237 files — `grep -rlE '"github\.com/hyzr-dev/slusk' --include='*.go' .`
counts them, and the number only grows — and the same cost recurs at every major bump.
Paying it now buys nothing, since it defends a scenario — a second party importing two
majors of this module at once — that cannot occur.

## Considered options

**Add `/v2` (and `/v3`, ...) to the module path.** Rejected. The refactor cost above is
real and recurring, in service of a convention whose only beneficiary is an importer that
does not exist.

**Stop tagging majors, cap the version at v1 forever.** Rejected. It distorts the
versioning scheme rather than fixing the mismatch: `!:` and `BREAKING CHANGE` commit
prefixes exist specifically to trigger a major bump, and freezing the major makes that
prefix meaningless. The problem is the suffix, not the tag.

## Consequences

- `go install github.com/hyzr-dev/slusk/cmd/slusk@latest` silently installs the last v1
  tag. Verified against the real proxy: it resolves to v1.59.2 with no error and no
  mention that later versions exist, because the suffixless path's version list never
  contains the v2+ tags in the first place. This is the module proxy working exactly as
  specified for a path that never opted into major-version suffixes — not a bug in slusk
  or in `go install`.
- A *pinned* v2+ version fails loudly instead of silently. `go list -m
  github.com/hyzr-dev/slusk@v2.5.0` errors with `invalid version: module contains a
  go.mod file, so module path must match major version ("github.com/hyzr-dev/slusk/v2")`.
  Go's own major-compatibility check rejects the fetch once it reads `go.mod`.
- `go.mod`, every import path, and the release workflow's version-bump logic
  (`.gitea/workflows/release.yml`) are deliberately left unchanged by this decision.
- `go install`/`go get` are documented as unsupported; the README points self-hosters at
  `make build` or a tagged container image instead.
- If a real importer ever shows up — something other than `cmd/slusk` depending on this
  module from outside the repo — that is the trigger to revisit, not a preference stated
  in an issue.
