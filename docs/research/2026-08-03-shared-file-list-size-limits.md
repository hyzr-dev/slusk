# Shared file list size limits: what the protocol and other implementations actually do

Issue: #409

## Why this note exists

slusk refuses to publish its own share once it exceeds roughly 170,000 files. The cause
is in `internal/soulseek/soul/peer/sharedfileistresponse.go:16-23`:

```go
const (
	maxSharedFileListFrameSize        = 16 << 20 // declared size, excluding its 4-byte prefix
	maxSharedFileListDecompressedSize = 16 << 20
	maxSharedFileListDirectories      = 100_000
	maxSharedFileListFiles            = 1_000_000
	maxSharedFileListAttributes       = 32
	maxSharedFileListStringSize       = 1 << 20
)
```

`maxSharedFileListDecompressedSize` (16 MiB) is checked via `sharedFileListLimitWriter`
(same file, lines 25-38) against the **uncompressed** payload while `Serialize` is
building `SharedFileListResponse` — i.e. it is applied to slusk's own outgoing share,
not just to a peer's incoming one. At ~170,000 files the wire frame (after zlib) is only
about 1.09 MiB; the uncompressed size is what trips `ErrMessageTooLarge`, so the actual
constraint is this self-imposed constant, not anything the network enforces.

The same constant (`maxSharedFileListDecompressedSize`) is reused, unchanged, by
`Deserialize` (line 230, via `readBoundedBrowsePayload`) to bound a peer's incoming list.
So today slusk applies **one identical 16 MiB cap in both directions** — this is exactly
the question worth checking against other implementations.

## Q1: Does the protocol itself define a size limit for SharedFileListResponse (code 5)?

No. Nicotine+'s protocol documentation — the closest thing to a spec that exists for
Soulseek — describes the wire format for peer code 5 with no mention of any size bound:

> `## Peer Code 5` / `### SharedFileListResponse` ... Send: 1. uint32 *number of
> directories* 2. Iterate... 6. zlib compress / Receive: 1. zlib decompress 2. uint32
> *number of directories* ...

(`doc/SLSKPROTOCOL.md`, `nicotine-plus/nicotine-plus`, section "Peer Code 5", fetched
from `raw.githubusercontent.com/nicotine-plus/nicotine-plus/master/doc/SLSKPROTOCOL.md`).
No maximum directory count, file count, string length, or decompressed size is
documented anywhere in that file for this message or its siblings (`FileSearchResponse`
code 9, `FolderContentsResponse` code 37 — same zlib-wrapped shape, same silence on
limits).

The only structural bound the protocol format itself implies is the **outer frame's**
4-byte length prefix (`uint32`), which caps the compressed-on-the-wire frame at ~4 GiB —
a limit on the compressed envelope, not on the payload it decompresses to. zlib output
size is unbounded by input size in principle (compression ratio dependent), so nothing
in the wire format caps the decompressed size at all.

**Conclusion: the 16 MiB figure is entirely slusk's own invention.** Nothing in the
protocol, in Nicotine+'s documentation of it, or in the two independent client
implementations checked below (Nicotine+, Soulseek.NET) imposes any comparable ceiling.

## Q2: What does Nicotine+ actually do when building its own outgoing share list?

It imposes **no limit at all**, and does not split or truncate. The full send path:

- `Shares.create_compressed_shares_message()` in
  `pynicotine/shares.py` (fetched from
  `raw.githubusercontent.com/nicotine-plus/nicotine-plus/master/pynicotine/shares.py`,
  lines 386-407) builds one `SharedFileListResponse` per permission level directly from
  the full `public_streams` / `buddy_streams` / `trusted_streams` share databases and
  calls `.make_network_message()` on it unconditionally — no size check, no chunking.
- `SharedFileListResponse.make_network_message()` in `pynicotine/slskmessages.py`
  (lines 3315-3349) iterates every directory in every share group, packs it into one
  `bytearray`, and at the very end does:

  ```python
  self.built = zlib.compress(msg, ZLIB_COMPRESSION_LEVEL)
  return self.built
  ```

  There is no size check anywhere before, during, or after that call. A share with
  170,000, 500,000, or 5,000,000 files is built and compressed as a single message
  regardless of size; the only failure mode in that method is a caught exception from
  reading the shares database itself (line 3308-3311), which logs and substitutes an
  empty list — not a size-triggered refusal.

**Conclusion: Nicotine+ neither truncates, splits across multiple messages, nor refuses
based on size when sending its own file list.** Whatever the true size turns out to be,
it goes out as one zlib-compressed peer message.

## Q3: How does Nicotine+ handle zlib compression, and is the same limit used both ways? (the crux question)

**No — the limit is asymmetric, and outbound is effectively unlimited.**

- **Outbound (encode):** as shown above, zero size check.
- **Inbound (decode):** `SharedFileListResponse.parse_network_message()`
  (`pynicotine/slskmessages.py`, lines 3351-3357):

  ```python
  def parse_network_message(self):
      decompressor = zlib.decompressobj()
      max_uncompressed_size = 2147483648  # 2 GiB
      self._message = memoryview(decompressor.decompress(self._message, max_uncompressed_size))

      if not decompressor.unconsumed_tail:
          self._parse_network_message()
  ```

  This uses `zlib.decompressobj().decompress(data, max_length)` — Python's built-in
  decompression-bomb guard, which stops decompressing once `max_length` output bytes
  have been produced and leaves the rest in `unconsumed_tail`. Nicotine+'s bound here is
  **2 GiB**, 128× slusk's 16 MiB. Critically, hitting the cap is not treated as an error:
  if `unconsumed_tail` is non-empty (i.e. the bomb guard tripped), Nicotine+ silently
  skips parsing (`if not decompressor.unconsumed_tail:`) rather than raising — the
  message is quietly dropped, not rejected with a hard failure.
- The sibling messages use the same asymmetric shape at different inbound thresholds:
  `FileSearchResponse.parse_network_message()` (lines 3482-3498) and
  `FolderContentsResponse.parse_network_message()` (lines 3650-3669) both cap inbound
  decompression at `max_uncompressed_size = 134217728` (**128 MiB**), again with no
  corresponding cap on their own `make_network_message()` encode paths
  (lines 3460-3480, 3700-3715).

So the pattern across all three zlib-wrapped peer messages in Nicotine+ is consistent:
**strict-ish inbound decompression-bomb guard (128 MiB–2 GiB depending on message
type), unlimited outbound.** This directly contradicts slusk's current design of one
identical 16 MiB ceiling applied to both directions.

## Q4: How large are real Soulseek shares in practice?

Evidence here is weaker and mostly anecdotal — flagged as such throughout.

- **Weak/anecdotal, GitHub issue:** Nicotine+ issue
  [#2000](https://github.com/nicotine-plus/nicotine-plus/issues/2000) ("share scan crash
  (size/levels, unicode)") states plainly that "some peer-to-peer filesharers have
  100+TB" of shared content and asks for directory-depth limits to be raised to
  accommodate them. No file count is given, but it corroborates that shares far larger
  than a casual user's are a known real-world case Nicotine+ maintainers have had to
  design around.
- **Weak/anecdotal:** Nicotine+ issue
  [#3199](https://github.com/nicotine-plus/nicotine-plus/issues/3199) ("Slowness when
  uploading large amount of files") mentions a user with roughly 900 GB+ in a single
  transfer context — a size datapoint, not a file-count one, and about transfer
  performance rather than the browse/share-list path specifically.
- **Weak/anecdotal, corroborates the OTHER side of this problem (receiving a huge
  list):** slskd issue [#1372](https://github.com/slskd/slskd/issues/1372) ("Out of
  memory / Aborted (core dumped) for browse user") reports an `OutOfMemoryException` in
  `BrowseResponseFactory.FromByteArray()` when browsing one specific user, with slskd's
  container OOMing at 2 GiB allocated (crashing around 650 MiB reported usage on first
  attempt) — evidence that some real shares are large enough to be actively painful for
  *other people's* clients to parse, not proof of an exact file count.
- **Weak/anecdotal:** Soulseek.NET issue
  [#503](https://github.com/jpdillingham/Soulseek.NET/issues/503) ("Browsing some
  users' files seems broken") — a commenter speculates "maybe it has something to do
  with the size of the file list" and references a Reddit post claiming the official
  SoulseekQt client itself "has trouble serializing very big responses." This is
  double-hearsay (a GitHub comment paraphrasing a Reddit post) and is included only
  because it is the one datapoint suggesting even the reference client may not handle
  huge shares gracefully — it is not confirmed against SoulseekQt source, which is
  closed and was not inspected for this note.

**No source found gives a clean, sourced "N files is a normal/large/broken share"
number.** The evidence supports "some real users have shares far larger than 170,000
files (up to 100+ TB in scale)" and "browsing a large share can OOM a receiving client
regardless of implementation" — but not a specific file-count threshold at which things
start failing in practice for any given client.

## What the sources do NOT settle

- No implementation checked (Nicotine+, Soulseek.NET) publishes an explicit maximum
  outbound file/directory count for a share list. "Unlimited" here means "no code path
  found that limits it," not "confirmed to work at every scale" — nothing was found
  documenting the largest share ever successfully browsed end-to-end.
- SoulseekQt (the original, closed-source official client) was not inspected — there is
  no way to check its behavior directly, only the secondhand Reddit claim relayed in
  Soulseek.NET issue #503.
- No source gives a rationale for *why* Nicotine+ chose 2 GiB / 128 MiB specifically for
  their inbound decompression caps, beyond "big enough not to matter, small enough to
  stop a true zlib bomb." Whether those specific numbers were chosen deliberately or are
  historical accidents is not documented anywhere found.
- Real-world file-count numbers for large shares (as opposed to storage size in TB)
  were not found in any primary source. The 170,000-file, ~1.09 MiB-compressed
  measurement in this issue is the most concrete data point available in this research;
  it did not come from an external source, it was given as background for this task.

## Implications for slusk

The current code applies **one constant, `maxSharedFileListDecompressedSize` (16 MiB),
symmetrically** to both slusk's own outgoing share (a size-bomb risk only to slusk's own
peers, not to slusk itself) and to a peer's incoming list (a real decompression-bomb
risk to slusk). Every other implementation checked treats these as different problems
with different risk profiles: unbounded (or effectively unbounded) outbound, a much
larger and non-fatal inbound guard.

Options available to the maintainer, without endorsing one:

1. **Split the single constant into two**, mirroring Nicotine+/Soulseek.NET: a
   permissive (or no) bound on the outbound encode path in `Serialize`, and a separate,
   independently-tunable bound on the inbound decode path in `Deserialize` where the
   decompression-bomb risk actually lives. This is the change that would let slusk
   publish shares beyond ~170,000 files without touching the peer-facing protection at
   all.
2. **Raise the outbound bound to a much larger number** (e.g. on the order of Nicotine+'s
   128 MiB–2 GiB inbound caps) while keeping symmetry, if a single shared constant is
   preferred for simplicity.
3. **Keep the current symmetric 16 MiB and instead cap the number of files/directories
   slusk will share**, accepting that very large libraries cannot be fully published —
   this preserves the existing conservative posture but caps a real product feature.
4. **Adopt a different inbound behavior on cap-exceeded**, matching Nicotine+'s
   "silently skip parsing, don't error" choice, versus slusk's current
   `ErrMessageTooLarge` — a separate design question from the size number itself, not
   settled by this research either way (no failure-mode preference is implied by any
   source; it's a product/robustness choice).

The maintainer decides the actual constant(s) and error-handling behavior; this note
only establishes what the protocol requires (nothing) and what two independent
implementations chose (asymmetric, effectively unbounded outbound).
