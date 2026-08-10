import type { ReactNode } from 'react';
import { useState } from 'react';
import {
  useCancelJob,
  useDeleteJob,
  useForceSearchJob,
  useRetryJob,
} from '../api/queries';
import { ApiError } from '../api/client';
import type { JobSource, JobState } from '../api/types';
import { useFlash } from './chrome/FlashContext';
import Button from './tui/Button';
import { t } from '../strings';
import styles from './JobActions.module.css';

interface Props {
  jobId: number;
  state: JobState;
  /** Which pipeline created the job. Only Re-run pipeline reads it — see
   * canForceSearch. */
  source: JobSource;
  /** Called after a successful delete, so the caller can collapse/navigate away. */
  onDeleted?: () => void;
  /** Extra control rendered in the left-hand group, alongside Retry/Re-run
   * pipeline/Cancel and before the spacer that pushes Delete right. Only the
   * job detail page fills this (issue #376) — the Jobs list expansion row
   * (JobExpansion.tsx) never passes it, so it stays absent there. */
  extra?: ReactNode;
}

// Cancel is hidden for these. Store.prepareJobCancellation updates
// album_jobs.state unconditionally — it carries no terminal-state guard of
// its own — so this list is the only thing stopping an already-finished job
// from being rewritten to CANCELLED. NOT_IMPORTED (issue #59) belongs here
// for exactly that reason: the download completed, there is nothing left to
// cancel. IMPORT_REFUSED (issue #470) is the same story — the download is
// done and Lidarr has already given its final answer.
const TERMINAL_STATES: JobState[] = ['DONE', 'FAILED', 'CANCELLED', 'NOT_IMPORTED', 'IMPORT_REFUSED'];

/** FAILED or PARKED are the two states Retry is offered for; exported so
 * JobDetail can share this rule instead of re-declaring it. */
export function isRetryEligible(state: JobState | undefined): boolean {
  return state === 'FAILED' || state === 'PARKED';
}

// Pulls the server's own message out of a 409 (or other) ApiError when one is
// present, falling back to the given canned string otherwise — the backend
// answers with plain text (http.Error, see internal/observ/observ.go), not
// JSON, so this relies on client.ts's text fallback rather than `body.error`.
function serverMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError && error.body?.error ? error.body.error : fallback;
}

// Shared action bar for both the Jobs list expansion row and the job detail
// page (issue #60), so the backend's validity rules (which actions are legal
// for which state) are interpreted in exactly one place instead of drifting
// between the two call sites.
export default function JobActions({ jobId, state, source, onDeleted, extra }: Props) {
  const cancel = useCancelJob(jobId);
  const retry = useRetryJob(jobId);
  const forceSearch = useForceSearchJob(jobId);
  const del = useDeleteJob(jobId);
  const flash = useFlash();

  // Two-click inline confirm for delete: the first click arms it, the second
  // fires. Reset on blur so a stray click elsewhere doesn't leave the button
  // primed, and on a failed delete so the button doesn't keep prompting
  // "click again" next to the error message.
  const [deleteArmed, setDeleteArmed] = useState(false);

  function handleDeleteClick() {
    if (!deleteArmed) {
      setDeleteArmed(true);
      return;
    }
    del.mutate(undefined, {
      onSuccess: () => {
        onDeleted?.();
        flash(t.jobs.deleteFlash(jobId));
      },
      onError: () => setDeleteArmed(false),
    });
  }

  const canRetry = isRetryEligible(state);
  const canCancel = !TERMINAL_STATES.includes(state);
  // A manual job has no lidarr_album_id, so there is nothing for a search to
  // be about; app.Jobs.ForceSearch rejects it with ErrJobNotSearchable (issue
  // #347). Hiding the button is the honest form of a 409 that can never not
  // happen — unlike the state-driven guards above, no retry or state change
  // can ever make this one legal.
  const canForceSearch = source !== 'manual';

  return (
    <div>
      <div className={styles.actions}>
        {canRetry && (
          <Button
            variant="primary"
            disabled={retry.isPending}
            onClick={() => retry.mutate(undefined, { onSuccess: () => flash(t.jobs.retryFlash(jobId)) })}
          >
            {t.jobs.retry}
          </Button>
        )}
        {canForceSearch && (
          <Button
            variant="ghost"
            disabled={forceSearch.isPending}
            onClick={() => forceSearch.mutate(undefined, { onSuccess: () => flash(t.jobs.forceSearchFlash(jobId)) })}
          >
            {t.jobs.forceSearch}
          </Button>
        )}
        {canCancel && (
          <Button
            variant="ghost"
            disabled={cancel.isPending}
            onClick={() => cancel.mutate(undefined, { onSuccess: () => flash(t.jobs.cancelFlash(jobId)) })}
          >
            {t.jobs.cancel}
          </Button>
        )}
        {extra}
        <span className={styles.spacer} />
        <Button
          variant="danger"
          disabled={del.isPending}
          onBlur={() => setDeleteArmed(false)}
          onClick={handleDeleteClick}
        >
          {deleteArmed ? t.jobs.deleteConfirm : t.jobs.delete}
        </Button>
      </div>

      {/* Every action can 409 (e.g. retry outside FAILED or PARKED, force search
          on an active job, delete on an importing job); surface the server's
          message rather than swallowing it. */}
      {cancel.isError && (
        <div className={styles.error}>{serverMessage(cancel.error, t.jobs.cancelFailed)}</div>
      )}
      {retry.isError && (
        <div className={styles.error}>{serverMessage(retry.error, t.jobs.retryFailed)}</div>
      )}
      {forceSearch.isError && (
        <div className={styles.error}>
          {serverMessage(forceSearch.error, t.jobs.forceSearchFailed)}
        </div>
      )}
      {del.isError && (
        <div className={styles.error}>{serverMessage(del.error, t.jobs.deleteFailed)}</div>
      )}
    </div>
  );
}
