# Spike: bh90210/soul as Soulseek protocol base — decision

**Date:** 2026-07-19
**Status:** Decided
**Issue:** #51
**Decision:** Fork/vendor the message layer of [bh90210/soul](https://github.com/bh90210/soul) into `internal/soulseek`; do not use it as a direct dependency; do not adopt its `client/` package.

## Question

Can bh90210/soul serve as the message layer for slusk's native Soulseek
protocol implementation (`internal/soulseek`, issues #52–#57), replacing the
slskd dependency?

## Evaluation summary

Analyzed at HEAD `5890ce2` (2025-04-20). Note that the latest release v1.1.0
lags HEAD by 15 commits and has an older, worse API — any adoption must be
based on HEAD, which has never been released.

### License

**The Unlicense** (public domain). Vendoring, forking, and modifying is
legally frictionless — no attribution or copyleft obligations.

### Protocol coverage

Essentially complete for all non-deprecated messages, named 1:1 with the
Nicotine+ SLSKPROTOCOL documentation:

- **Server (63 codes):** Login, SetListenPort, GetPeerAddress, ConnectToPeer,
  FileSearch, Ping, distributed-parent messages (HaveNoParent, BranchLevel/Root,
  PossibleParents), wishlist, rooms, private rooms, ExcludedSearchPhrases.
- **Peer init:** PeerInit, PierceFireWall.
- **Peer (P):** full browse/search-reply/transfer flow including the modern
  QueueUpload-based transfer negotiation, with rejection reasons mapped to
  sentinel errors.
- **File (F):** TransferInit + Offset (the codeless file handshake).
- **Distributed (D):** Search, BranchLevel, BranchRoot, EmbeddedMessage.
- **Obfuscation:** rotate-XOR implemented both directions for peer connections.
- **zlib:** correctly applied to SharedFileListResponse, FileSearchResponse,
  FolderContentsResponse.

### Architecture

The message codec is genuinely standalone: packages `server`, `peer`, `file`,
`distributed` and the framing code in `internal/` are **stdlib-only**, one file
per message, generic `Read`/`Write` per package. All eight external
dependencies are confined to the high-level `client/` package (zerolog,
nanoid, etc.), which duplicates concerns slusk already owns (logging,
state, config) and is not a fit.

### Practical verification

A throwaway prototype (connect + login against `server.slsknet.org:2242` with
a fresh auto-registered account) succeeded on the first run: Login response
deserialized, `SetListenPort` accepted, and the ~4 KB RoomList message
(213 rooms) framed and parsed correctly.

```go
conn, _ := net.DialTimeout("tcp", "server.slsknet.org:2242", 10*time.Second)
server.Write(conn, &server.Login{Username: user, Password: pass})
r, _, code, _ := server.Read(conn) // code == server.CodeLogin
login := new(server.Login)
login.Deserialize(r) // login.Greet, login.IP populated; sentinel errors on failure
```

### Why not a direct dependency

- **Abandoned:** single author, 17 commits, dormant since 2025-04, and HEAD
  ships a non-compiling test package (`peer/foldercontentsresponse_test.go`
  references a renamed type). No upstream to merge fixes into.
- **No release of the current API:** we would pin a pseudo-version forever.
- **Missing hardening we need immediately:**
  - No max-message-size guard — a malicious peer declaring a 4 GiB frame
    drives unbounded allocation (`io.CopyN` / `make([]byte, size)` from
    untrusted uint32). Unacceptable for a long-running daemon.
  - `ReadUint32` et al. return `io.EOF` alongside valid values, forcing
    `if err != nil && !errors.Is(err, io.EOF)` at every call site — a footgun
    we want to fix at the source.
  - Framing primitives live in `internal/`, unreachable from outside the
    module.
- **Uneven tests:** `server/` has one unit test for 63 messages (upstream CI
  relied on a soulfind integration container); `go vet` fails at HEAD.

### Why not reference-only

The codec is largely correct and complete, and was integration-tested against
a real server implementation (soulfind) in upstream CI. Rewriting 80+ message
serializers from scratch is exactly the toil it saves; our prototype confirms
the wire format works against the live network.

## Decision

**Vendor a forked subset** — `soul.go`, `internal/`, `server/`, `peer/`,
`file/`, `distributed/` (drop `client/`, `cmd/`) — into slusk under
`internal/soulseek/`, as the starting point for the protocol core (#52).
Unlicense means no attribution is required, but we keep a provenance note
pointing at the upstream commit.

Hardening owed in #52 (tracked there, not here):

1. Max-message-size caps on framing and string/array reads.
2. Clean up the `io.EOF`-as-soft-error convention.
3. Fix the broken peer test; make `go vet ./...` clean.
4. Unit tests for the server messages we actually use.
5. Consider latin-1/CP1252 fallback for non-UTF-8 filenames from legacy
   clients (Nicotine+ does this); bytes round-trip safely either way.

## Consequences

- #52 starts from a working codec instead of a blank page; its scope shifts
  toward connection lifecycle (reconnect/backoff, keepalive, status for
  `internal/observ`) plus the hardening list above.
- We own ~6 kLOC of vendored codec code. Mitigated by the one-file-per-message
  layout mapping 1:1 to SLSKPROTOCOL docs, which keeps audits mechanical.
- Prototype code is deliberately not committed; this document and the snippet
  above are the record. The evaluation clone and prototype lived in a
  scratchpad and are disposable.
