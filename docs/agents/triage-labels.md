# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker.

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

Edit the right-hand column to match whatever vocabulary you actually use — but two pieces
of code now hardcode these exact strings and will not follow you:

- `.claude/workflows/backlog-triage.js` lists all five in `TRIAGE_LABELS`, and the three
  category labels in `CATEGORY_LABELS`, as judgement-schema enums. A renamed label makes
  every judge agent fail schema validation on that field.
- `scripts/triage/waves.mjs` matches `wontfix` and `needs-info` by literal string in
  `isDeferred()` to keep those issues out of the waves. A rename here fails *silently*:
  the label stops matching, and issues nobody intends to work start being scheduled again.

The second is the dangerous one, and it is why this paragraph exists rather than a comment
in one of the files. Rename a label and grep for the old string in both before assuming the
triage still behaves.

## Existing labels in this repo

All five roles above exist in Gitea (created 2026-08-02). Apply them with
`tea issues edit <n> --add-labels "..."`; don't create them again.

`bug`, `enhancement`, `tech-debt`, `1.0` and `Public` also exist and are *categories*,
not triage states — they are orthogonal to the table above and must not be consumed as
triage labels. An issue normally carries one of each kind.
