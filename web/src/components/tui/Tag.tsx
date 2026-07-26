import type { JobState, JobStatus } from '../../api/types';
import { t } from '../../strings';
import styles from './Tag.module.css';

export type TagKind = 'DL' | 'QU' | 'ST' | 'OR' | 'FA' | 'OK' | 'IM';

const BY_STATUS: Record<JobStatus, TagKind> = {
  queued: 'QU',
  active: 'DL',
  stalled: 'ST',
  done: 'OK',
  failed: 'FA',
  orphaned: 'OR',
};

const TONE: Record<TagKind, string> = {
  DL: styles.neutral,
  IM: styles.neutral,
  QU: styles.quiet,
  OK: styles.ok,
  ST: styles.bad,
  FA: styles.bad,
  OR: styles.bad,
};

/**
 * The two-letter tag for a job row.
 *
 * Reads three inputs because no single one of them is sufficient: `status` is
 * the coarse bucket, `state` is the only place importing appears, and a
 * non-zero `queuePosition` means an "active" job is in fact waiting in a
 * peer's queue with no bytes moving. Terminal statuses ignore the queue
 * position — a finished job may still carry a stale one.
 */
export function tagFor(
  status: JobStatus,
  state: JobState,
  queuePosition?: number,
): TagKind {
  if (status === 'active') {
    if (state === 'IMPORTING') return 'IM';
    if (queuePosition && queuePosition > 0) return 'QU';
  }
  return BY_STATUS[status] ?? 'QU';
}

interface Props {
  status: JobStatus;
  state: JobState;
  queuePosition?: number;
  /** Omit the border, for dense grids where the box is visual noise. */
  bare?: boolean;
}

export default function Tag({ status, state, queuePosition, bare = false }: Props) {
  const kind = tagFor(status, state, queuePosition);
  return (
    <span
      className={`${bare ? styles.bare : styles.tag} ${TONE[kind]}`}
      title={t.tagTitle[kind]}
    >
      {t.tag[kind]}
    </span>
  );
}
