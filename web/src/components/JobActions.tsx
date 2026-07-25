import { useEffect, useState } from 'react';
import {
  useCancelJob,
  useDeleteJob,
  useForceSearchJob,
  useRetryJob,
} from '../api/queries';
import type { JobState } from '../api/types';
import { t } from '../strings';
import styles from './JobActions.module.css';

interface Props {
  jobId: number;
  state: JobState;
  /** Called after a successful delete, so the caller can collapse/navigate away. */
  onDeleted?: () => void;
}

const TERMINAL_STATES: JobState[] = ['DONE', 'FAILED', 'CANCELLED'];

// Shared action bar for both the Jobs list expansion row and the job detail
// page (issue #60), so the backend's validity rules (which actions are legal
// for which state) are interpreted in exactly one place instead of drifting
// between the two call sites.
export default function JobActions({ jobId, state, onDeleted }: Props) {
  const cancel = useCancelJob(jobId);
  const retry = useRetryJob(jobId);
  const forceSearch = useForceSearchJob(jobId);
  const del = useDeleteJob(jobId);

  // Two-click inline confirm for delete: the first click arms it, the second
  // fires. Reset on blur so a stray click elsewhere doesn't leave the button
  // primed, and on unmount/collapse via the empty-deps cleanup below.
  const [deleteArmed, setDeleteArmed] = useState(false);
  useEffect(() => () => setDeleteArmed(false), []);

  function handleDeleteClick() {
    if (!deleteArmed) {
      setDeleteArmed(true);
      return;
    }
    del.mutate(undefined, { onSuccess: () => onDeleted?.() });
  }

  const canRetry = state === 'FAILED' || state === 'ORPHANED';
  const canCancel = !TERMINAL_STATES.includes(state);

  return (
    <div>
      <div className={styles.actions}>
        {canRetry && (
          <button
            className={styles.retry}
            disabled={retry.isPending}
            onClick={() => retry.mutate()}
          >
            {t.jobs.retry}
          </button>
        )}
        <button
          className={styles.neutral}
          disabled={forceSearch.isPending}
          onClick={() => forceSearch.mutate()}
        >
          {t.jobs.forceSearch}
        </button>
        {canCancel && (
          <button
            className={styles.neutral}
            disabled={cancel.isPending}
            onClick={() => cancel.mutate()}
          >
            {t.jobs.cancel}
          </button>
        )}
        <button
          className={styles.delete}
          disabled={del.isPending}
          onBlur={() => setDeleteArmed(false)}
          onClick={handleDeleteClick}
        >
          {deleteArmed ? t.jobs.deleteConfirm : t.jobs.delete}
        </button>
      </div>

      {/* Every action can 409 (e.g. retry on a non-failed job, force search on
          an active job, delete on an importing job); surface the server's
          message rather than swallowing it. */}
      {cancel.isError && <div className={styles.error}>{t.jobs.cancelFailed}</div>}
      {retry.isError && <div className={styles.error}>{t.jobs.retryFailed}</div>}
      {forceSearch.isError && <div className={styles.error}>{t.jobs.forceSearchFailed}</div>}
      {del.isError && <div className={styles.error}>{t.jobs.deleteFailed}</div>}
    </div>
  );
}
