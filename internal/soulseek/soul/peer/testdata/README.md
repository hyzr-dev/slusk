# FileSearchResponse protocol evidence

The focused code-9 tests generate compact frames in memory rather than checking
in opaque captures. Their layouts are based on these source snapshots inspected
on 2026-07-20:

- Nicotine+ `pynicotine/slskmessages.py`, master commit
  [`6d88c63a1a6ac83ee67539cb4473c97bc9784e5f`](https://github.com/nicotine-plus/nicotine-plus/blob/6d88c63a1a6ac83ee67539cb4473c97bc9784e5f/pynicotine/slskmessages.py#L3432-L3535)
  (local snapshot SHA-256
  `c13cffc73da84adba681d13b5156762b52556788f6abe773f1a309b93e1be78f`).
  `_parse_remaining_network_message` checks for remaining content before the
  unknown uint32 and checks again before the private-result list.
- Vendored upstream `bh90210/soul`, commit
  [`5890ce2`](https://github.com/bh90210/soul/blob/5890ce2/peer/filesearchresponse.go)
  dated 2025-04-20. It always writes the unknown uint32 and private-result
  count, establishing the full-tail layout.

Chosen contract: after the mandatory `Queue` field, accept clean decompressed
EOF; if bytes remain, require a complete unknown uint32; then independently
accept clean EOF or require a complete private count/list. EOF inside a uint32,
declared file, string, or attribute is truncation. Every accepted layout must
end at clean zlib EOF with a valid checksum and no unrecognized decompressed
bytes. Private results remain represented in the wire type; issue #54's higher
layer intentionally does not consume them.
