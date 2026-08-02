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

Edit the right-hand column to match whatever vocabulary you actually use.

## Existing labels in this repo

All five roles above exist in Gitea (created 2026-08-02). Apply them with
`tea issues edit <n> --add-labels "..."`; don't create them again.

`bug`, `enhancement`, `tech-debt`, `1.0` and `Public` also exist and are *categories*,
not triage states — they are orthogonal to the table above and must not be consumed as
triage labels. An issue normally carries one of each kind.
