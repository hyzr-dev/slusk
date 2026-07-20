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

Beyond the import path rewrite, the codec layer was hardened against
malicious or corrupted input, and its error handling convention was made
stricter:

- `soul.go`: added sentinel `ErrMessageTooLarge`.
- `internal/internal.go`:
  - Added `MaxMessageSize = 64 << 20` (64 MiB).
  - `MessageRead` now rejects any declared message size larger than
    `MaxMessageSize` before allocating or reading the message body — in both
    the plain and obfuscated (`deobfuscate`) code paths.
  - `ReadString` and `ReadBytes` reject a declared size larger than
    `MaxMessageSize` before calling `make` for the buffer.
  - Removed the upstream convention of treating `io.EOF` as a "soft" error
    to be swallowed at each read-helper call site. Every read helper
    (`ReadUint8`, `ReadUint32`, `ReadUint64`, `ReadInt32`, `ReadInt32ToInt`,
    `ReadUint32ToInt`, `ReadUint32ToToken`, `ReadUint64ToInt`, `ReadBool`,
    `ReadBytes`, `ReadString`) now returns `(zero, err)` on any error and
    `(val, nil)` on success — there is no longer a case where a non-nil error
    is returned alongside a "valid" zero value.
  - In `MessageRead`, a short copy while filling the message buffer is now a
    hard error: `ErrDifferentPacketSize` wrapping `io.ErrUnexpectedEOF` (a
    clean `io.EOF` from the underlying reader is normalized to
    `io.ErrUnexpectedEOF`, matching `io.ReadFull`'s convention), instead of
    being silently tolerated.
  - Also added a declared-size-vs-already-read-prefix sanity check in
    `MessageRead` (`ErrDifferentPacketSize` if the declared size is smaller
    than the code prefix already consumed).
- `server/`, `peer/`, `file/`, `distributed/`: mechanically swept all
  `if err != nil && !errors.Is(err, io.EOF)` call-site carve-outs down to
  plain `if err != nil` checks, now that the read helpers never return a
  soft `io.EOF`. No behavioral change beyond removing the now-impossible
  "successful read that also returns io.EOF" case.

`peer/`, `file/`, and `distributed/` have no test coverage yet — they are
unused by the rest of this module until a later issue wires up peer
connections.

- `internal/internal.go` (`deobfuscate`): hardened against malicious or
  corrupted declared sizes in the obfuscated frame path.
  - The initial 4-byte obfuscation key read now returns immediately on a
    short copy instead of setting an error that a later loop iteration could
    silently overwrite.
  - The body-length accounting (`size - readSoFar`, deciding how many bytes
    remain to read in the last chunk) now uses signed arithmetic and is
    computed before every body read, not just applied retroactively after the
    default-case body write. Previously: (1) an unsigned underflow when
    `readSoFar` exceeded a corrupted `size` made the remaining-bytes check
    always false, so the loop kept requesting 4-byte chunks from the
    connection indefinitely (buffering until the connection's EOF); (2) the
    adjustment only ran in the default (body) case, so a body shorter than 4
    bytes was mis-read as a full 4-byte chunk on its first iteration. Both are
    fixed: a `size` smaller than what has already been read now fails fast
    with `ErrDifferentPacketSize`, and a short first body chunk is sized
    correctly from the start.
  - A declared `size` of 0 (a frame with no code at all) is now rejected with
    `ErrDifferentPacketSize` immediately after the size field is parsed.
  - A short copy while filling a body chunk is now normalized to
    `ErrDifferentPacketSize` (wrapping `io.ErrUnexpectedEOF` for a clean
    `io.EOF`), matching `MessageRead`'s convention for the non-obfuscated
    path.

## Optional trailing field policy

Deserialize is strict by default: any read error is a hard error. The single
documented exception is a trailing, protocol-optional field truncated
exactly at its boundary — i.e. `binary.Read`/`internal.ReadString` returning
a *clean* `io.EOF` because zero bytes remained, not `io.ErrUnexpectedEOF`
from a partial read mid-field. That specific case is treated as "field
absent" rather than an error, since real Soulseek peers are known to omit
some trailing fields.

- `peer/transferresponse.go` (`TransferResponse.Deserialize`): the `Reason`
  string, only present when `Allowed` is false, is now optional under this
  policy. A clean `io.EOF` reading it leaves `Reason` nil; a truncation
  partway through the string is still a hard error.
  - Refinement: "absent" is judged strictly from the length prefix, not the
    whole call. `internal.ReadString` used to be called as one step, so a
    frame ending immediately after a complete 4-byte length prefix declaring
    a nonzero-length body also produced a clean `io.EOF` (from
    `io.ReadFull` reading zero of N bytes) and was misclassified as "reason
    absent" instead of a truncated field. `Deserialize` now reads the length
    prefix (`internal.ReadStringLen`) and body (`internal.ReadStringBody`)
    as separate steps: only a clean `io.EOF` from the length-prefix read
    itself - meaning zero bytes remained in the frame at all - counts as
    "absent"; any error reading the body, even a clean `io.EOF`, is a hard
    error.
- `peer/filesearchresponse.go`: resolved for #54 from Nicotine+ master commit
  [`6d88c63a`](https://github.com/nicotine-plus/nicotine-plus/blob/6d88c63a1a6ac83ee67539cb4473c97bc9784e5f/pynicotine/slskmessages.py#L3499-L3509),
  inspected 2026-07-20, and the full-tail layout in vendored `bh90210/soul`
  [`5890ce2`](https://github.com/bh90210/soul/blob/5890ce2/peer/filesearchresponse.go)
  (2025-04-20). Clean decompressed EOF is accepted after `Queue` and,
  independently, after the following unknown uint32; partial fields/files,
  invalid zlib endings, and extra decompressed data are rejected.
