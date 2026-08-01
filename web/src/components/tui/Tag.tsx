import type { JobStatus } from '../../api/types';
import { t } from '../../strings';
import styles from './Tag.module.css';

export type TagKind = 'DL' | 'QU' | 'ST' | 'PA' | 'FA' | 'OK' | 'IM' | 'NI';

const BY_STATUS: Record<JobStatus, TagKind> = {
  queued: 'QU',
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
const TONE: Record<TagKind, string> = {
  DL: styles.neutral,
  IM: styles.neutral,
  NI: styles.neutral,
  QU: styles.quiet,
  OK: styles.ok,
  ST: styles.bad,
  FA: styles.bad,
  PA: styles.bad,
};

/**
 * The two-letter tag for a job row.
 *
 * `status` alone now carries importing (issue #269 — the backend used to
 * serialize an IMPORTING job as status 'active', so this function used to
 * take `state` too just to recover it); the only remaining special case is a
 * non-zero `queuePosition` on an 'active' job, meaning it's in fact waiting
 * in a peer's queue with no bytes moving. Terminal statuses ignore the queue
 * position — a finished job may still carry a stale one.
 */
export function tagFor(status: JobStatus, queuePosition?: number): TagKind {
  if (status === 'active' && queuePosition && queuePosition > 0) return 'QU';
  return BY_STATUS[status] ?? 'QU';
}

interface Props {
  status: JobStatus;
  queuePosition?: number;
  /** Omit the border, for dense grids where the box is visual noise. */
  bare?: boolean;
}

export default function Tag({ status, queuePosition, bare = false }: Props) {
  const kind = tagFor(status, queuePosition);
  return (
    <span
      className={`${bare ? styles.bare : styles.tag} ${TONE[kind]}`}
      title={t.tagTitle[kind]}
    >
      {t.tag[kind]}
    </span>
  );
}
