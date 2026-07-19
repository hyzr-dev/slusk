# Vendored: bh90210/soul

This directory contains a partial, vendored copy of the Soulseek protocol
message layer from https://github.com/bh90210/soul, pinned at commit
`5890ce2` (2025-04-20).

License: Unlicense (public domain). The original `LICENSE` file is retained
unmodified in this directory.

## Import rewrite

All imports of `github.com/bh90210/soul` (and its subpackages) were
mechanically rewritten to `github.com/samuelenocsson/slskdarr/internal/soulseek/soul`
so the vendored code compiles as part of this module. No package clauses or
other code was changed as part of this rewrite.

## What was copied

- `soul.go`
- `LICENSE`
- `internal/`
- `server/`
- `peer/`
- `file/`
- `distributed/`

## What was dropped

- `client/` — high-level client implementation we are not using; we build our
  own connection layer on top of the message types instead.
- `cmd/` — example CLI, not needed.
- `go.mod` / `go.sum` — the vendored code is compiled as part of this module,
  not as a separate dependency.
- `README.md`, `.github/`, `.gitignore`, `testdata/` — project metadata not
  relevant to a vendored subset.
- All upstream `*_test.go` files, in every copied package. They depend on
  `testify`, which we do not want as a dependency, and at least one of them
  (`peer/foldercontentsresponse_test.go`) does not compile against the
  upstream code as-is. Every `*_test.go` file under this tree is ours, added
  after vendoring, using only the standard library's `testing` package.

## Local modifications

None beyond the import path rewrite described above.
