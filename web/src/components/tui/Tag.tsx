import type { JobStatus } from '../../api/types';
import { t } from '../../strings';
import styles from './Tag.module.css';

export type TagKind = 'DL' | 'QU' | 'ST' | 'PA' | 'FA' | 'OK' | 'IM' | 'NI' | 'WA' | 'SE' | 'WT';

const BY_STATUS: Record<JobStatus, TagKind> = {
  wanted: 'WA',
  selecting: 'SE',
  queued: 'QU',
  waiting: 'WT',
  active: 'DL',
  stalled: 'ST',
  importing: 'IM',
  done: 'OK',
  failed: 'FA',
  parked: 'PA',
  notImported: 'NI',
};

// NI (issue #59) is neither --ok nor --bad: the download succeeded and the
// files are on disk, it just never reached Lidarr — reading it as a failure
// would be exactly the invented-certainty the design brief forbids.
//
// WA/SE/WT (issue #416) get the same quiet tone as QU: none of them have
// bytes moving, and per CLAUDE.md's design rules status carries no per-state
// hue — only --ok and --bad are ever coloured. WT rather than WA for waiting:
// wanted is by far the more common row and keeps the natural code.
const TONE: Record<TagKind, string> = {
  DL: styles.neutral,
  IM: styles.neutral,
  NI: styles.neutral,
  QU: styles.quiet,
  WA: styles.quiet,
  SE: styles.quiet,
  WT: styles.quiet,
  OK: styles.ok,
  ST: styles.bad,
  FA: styles.bad,
  PA: styles.bad,
};

/**
 * The two-letter tag for a job row. `status` alone carries the whole picture
 * (issue #269 — the backend used to serialize an IMPORTING job as status
 * 'active', so this function used to take `state` too just to recover it).
 *
 * As of issue #416 the backend is the sole source of truth for whether a job
 * is sitting in a peer's queue: it now reports that as its own 'queued'
 * status rather than 'active' plus a non-zero queuePosition, so this
 * function no longer derives QU from queuePosition itself — a client-side
 * derivation of the same fact the backend already computes is exactly the
 * Go/TS double-track issue #269 removed elsewhere.
 */
export function tagFor(status: JobStatus): TagKind {
  return BY_STATUS[status] ?? 'QU';
}

interface Props {
  status: JobStatus;
  /** Omit the border, for dense grids where the box is visual noise. */
  bare?: boolean;
}

export default function Tag({ status, bare = false }: Props) {
  const kind = tagFor(status);
  return (
    <span
      className={`${bare ? styles.bare : styles.tag} ${TONE[kind]}`}
      title={t.tagTitle[kind]}
    >
      {t.tag[kind]}
    </span>
  );
}
