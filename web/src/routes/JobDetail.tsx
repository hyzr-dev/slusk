import { Link, useParams } from 'react-router-dom';
import {
  useCancelJob,
  useJobDetail,
  useJobEvents,
  useJobs,
  useRetryJob,
} from '../api/queries';
import PageHeading from '../components/PageHeading';
import StatusPill from '../components/StatusPill';
import table from '../components/Table.module.css';
import { formatBytes, formatDateTime, formatShortTime } from '../format';
import { candidateStateLabel, eventLabel, t } from '../strings';
import styles from './JobDetail.module.css';

export default function JobDetail() {
  const id = Number(useParams().id);
  const { data: jobs = [] } = useJobs();
  const {
    data: detail,
    isLoading: detailLoading,
    isPlaceholderData: detailIsPlaceholder,
  } = useJobDetail(id);
  const {
    data: events,
    isLoading: eventsLoading,
    isPlaceholderData: eventsIsPlaceholder,
  } = useJobEvents(id);
  const cancel = useCancelJob(id);
  const retry = useRetryJob(id);

  const job = jobs.find((j) => j.id === id);

  // The QueryClient sets `placeholderData: keepPreviousData` globally, so
  // navigating to a job id never fetched before doesn't put `useJobDetail`/
  // `useJobEvents` into a loading state — it instead returns the *previous*
  // job's data with `isPlaceholderData: true`. Without this guard the page
  // would show job 41's attempt history while the URL says job 42. Anywhere
  // `detail`/`events` inform what's rendered, treat placeholder data as "not
  // actually loaded yet".
  const detailReady = detail !== undefined && !detailIsPlaceholder;

  // Retry is offered when either source reports FAILED — the polled list and
  // the detail response can disagree briefly, and both are authoritative
  // enough. The detail side only counts once it's confirmed to belong to
  // this job (see detailReady above) — otherwise a stale placeholder from a
  // previously viewed failed job could show Retry on an unrelated job.
  const isFailed = job?.state === 'FAILED' || (detailReady && detail?.state === 'FAILED');

  // A notBefore in the past has no display relevance.
  const sleepingUntil =
    job?.notBefore && new Date(job.notBefore) > new Date() ? job.notBefore : '';

  return (
    <>
      <Link to="/jobs" className={styles.back}>{t.jobs.back}</Link>

      {/* Three-tier fallback: live job, then cached detail, then loading. The
          middle tier keeps the page useful after a job ages out of /api/jobs. */}
      {job ? (
        <>
          <PageHeading>{job.title}</PageHeading>
          <div className={styles.meta}>
            <StatusPill status={job.status} state={job.state} />
            <span>{job.artist}</span>
            <span>{job.peer || '—'}</span>
            <span className={table.mono}>
              {formatBytes(job.bytesDone)} / {formatBytes(job.bytesTotal)}
            </span>
            {sleepingUntil && (
              <span className={styles.sleeping}>
                {t.jobs.sleepingUntil(formatShortTime(sleepingUntil))}
              </span>
            )}
            {job.nextAttemptAt && <span>{t.jobs.nextAttempt(formatDateTime(job.nextAttemptAt))}</span>}
          </div>
          {(job.status === 'queued' || job.status === 'failed') && job.maxCandidates > 0 && (
            <div className={styles.candidates}>
              {t.jobs.candidates(job.candidatesTried, job.maxCandidates)}
            </div>
          )}
          {job.retries > 0 && (
            <div className={styles.candidates}>{t.jobs.retries(job.retries)}</div>
          )}
          {job.failReason && <div className={styles.failReason}>{job.failReason}</div>}
        </>
      ) : detailReady && detail ? (
        <>
          <PageHeading>{detail.title}</PageHeading>
          <div className={styles.meta}>
            <span>{detail.artist}</span>
            {/* jobDetailDTO carries no `status` field (internal/observ/jobdetail.go),
                so there's nothing to degrade to here — unlike stateLabel()'s
                state->status->raw chain used elsewhere. This mirrors the legacy
                dashboard's `STATE_LABEL[detail.state] || detail.state`: an
                unrecognised state falls straight back to the raw value. `t.state`'s
                keys are exactly the `JobState` union, so indexing it with
                `detail.state` type-checks without a helper. */}
            <span>{t.state[detail.state] ?? detail.state}</span>
          </div>
        </>
      ) : (
        <PageHeading>{t.jobs.loading}</PageHeading>
      )}

      <div className={styles.actions}>
        <button
          className={styles.action}
          disabled={cancel.isPending}
          onClick={() => cancel.mutate()}
        >
          {t.jobs.cancel}
        </button>
        {isFailed && (
          <button
            className={styles.action}
            disabled={retry.isPending}
            onClick={() => retry.mutate()}
          >
            {t.jobs.retry}
          </button>
        )}
      </div>

      {/* The legacy dashboard never checked res.ok on these actions, so a
          failed cancel/retry was silently invisible; we surface it here. */}
      {cancel.isError && <div className={styles.error}>{t.jobs.cancelFailed}</div>}
      {retry.isError && <div className={styles.error}>{t.jobs.retryFailed}</div>}

      <h2 className={styles.section}>{t.jobs.attemptHistory}</h2>
      {detailLoading || detailIsPlaceholder ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !detail?.attempts.length ? (
        <div className={styles.placeholder}>{t.jobs.noAttempts}</div>
      ) : (
        detail.attempts.map((a) => (
          <div key={a.id} className={styles.attempt}>
            <div>
              <strong>{a.username}</strong> {candidateStateLabel(a.state)}
              {a.failReason && ` (${a.failReason})`}
            </div>
            <div className={styles.attemptMeta}>
              {formatDateTime(a.createdAt)} — {a.fileCount} files
            </div>
            {a.transfers.map((tr) => (
              <div key={tr.filename} className={styles.transfer}>
                {tr.filename} — {tr.state} {formatBytes(tr.bytesDone)} /{' '}
                {formatBytes(tr.bytesTotal)}
                {tr.retries > 0 && ` (${tr.retries} retries)`}
              </div>
            ))}
          </div>
        ))
      )}

      <h2 className={styles.section}>{t.jobs.events}</h2>
      {eventsLoading || eventsIsPlaceholder ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !events?.length ? (
        <div className={styles.placeholder}>{t.jobs.noEvents}</div>
      ) : (
        <table className={table.table}>
          <thead>
            <tr>
              <th className={table.th}>{t.columns.time}</th>
              <th className={table.th}>{t.columns.event}</th>
              <th className={table.th}>{t.columns.detail}</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.id}>
                <td className={`${table.td} ${table.mono}`}>{formatDateTime(e.createdAt)}</td>
                <td className={table.td}>{eventLabel(e.event)}</td>
                <td className={table.td}>{e.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
