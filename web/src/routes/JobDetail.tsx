import { Link, useNavigate, useParams } from 'react-router-dom';
import { useJobDetail, useJobEvents, useJobs } from '../api/queries';
import type { JobState, TransferDetail } from '../api/types';
import JobActions, { isRetryEligible } from '../components/JobActions';
import EmptyState from '../components/tui/EmptyState';
import SectionHeader from '../components/tui/SectionHeader';
import Tag from '../components/tui/Tag';
import Ticks, { type TickTone } from '../components/tui/Ticks';
import table from '../components/Table.module.css';
import {
  basename,
  compareFileNames,
  formatBytes,
  formatDateTime,
  formatShortTime,
  formatSize,
  formatSpeed,
  percent,
} from '../format';
import { candidateStateLabel, eventLabel, t } from '../strings';
import styles from './JobDetail.module.css';

// Per-transfer tick resolution, matching the mock's TRANSFERS panel
// (docs/design/slskdarr-tui.dc.html:~102) rather than the coarser 26 ticks
// used in the dense Jobs grid — a single job's own detail page has room for
// the finer bar.
const TRANSFER_TICKS = 104;

// Tick colour and live-flare eligibility for one transfer. A non-zero
// queuePosition means the transfer is waiting in the peer's queue with no
// bytes moving, so it must never flare — that is the one failure mode in
// these views that actively misinforms (a flashing bar reads as "data is
// arriving" when it isn't).
function transferTone(tr: TransferDetail): { tone: TickTone; live: boolean } {
  const queued = (tr.queuePosition ?? 0) > 0;
  if (queued) return { tone: 'queued', live: false };
  if (tr.state === 'COMPLETED') return { tone: 'ok', live: false };
  if (tr.state === 'ERRORED' || tr.state === 'CANCELLED') return { tone: 'bad', live: false };
  return { tone: 'bar', live: tr.state === 'IN_PROGRESS' };
}

export default function JobDetail() {
  const id = Number(useParams().id);
  const navigate = useNavigate();
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

  const job = jobs.find((j) => j.id === id);

  // The QueryClient sets `placeholderData: keepPreviousData` globally, so
  // navigating to a job id never fetched before doesn't put `useJobDetail`/
  // `useJobEvents` into a loading state — it instead returns the *previous*
  // job's data with `isPlaceholderData: true`. Without this guard the page
  // would show job 41's attempt history while the URL says job 42. Anywhere
  // `detail`/`events` inform what's rendered, treat placeholder data as "not
  // actually loaded yet".
  const detailReady = detail !== undefined && !detailIsPlaceholder;

  // JobActions decides button visibility from a single `state`, but the
  // polled list and the detail response can disagree briefly and both are
  // authoritative enough — so pick whichever source reports a retry-eligible
  // state (FAILED/ORPHANED) first. The detail side only counts once it's
  // confirmed to belong to this job (see detailReady above) — otherwise a
  // stale placeholder from a previously viewed failed job could show Retry
  // on an unrelated job.
  const actionState: JobState = isRetryEligible(job?.state)
    ? job!.state
    : detailReady && isRetryEligible(detail?.state)
      ? detail!.state
      : (job?.state ?? detail?.state ?? 'WANTED');

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
          <SectionHeader label={job.title} meta={job.artist} />
          <div className={styles.meta}>
            <Tag status={job.status} state={job.state} queuePosition={job.queuePosition} />
            {job.source === 'manual' && (
              <span className={styles.sourceDot} title={t.source.manual}>●</span>
            )}
            <span className={table.mono}>{job.peer || '—'}</span>
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
            <div className={styles.subline}>
              {t.jobs.candidates(job.candidatesTried, job.maxCandidates)}
            </div>
          )}
          {job.retries > 0 && (
            <div className={styles.subline}>{t.jobs.retries(job.retries)}</div>
          )}
          {job.failReason && <div className={styles.failReason}>{job.failReason}</div>}
        </>
      ) : detailReady && detail ? (
        <>
          <SectionHeader label={detail.title} meta={detail.artist} />
          <div className={styles.meta}>
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
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      )}

      <div className={styles.actionsWrap}>
        <JobActions jobId={id} state={actionState} onDeleted={() => navigate('/jobs')} />
      </div>

      <SectionHeader label={t.jobs.attemptHistory} />
      {detailLoading || detailIsPlaceholder ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !detail?.attempts.length ? (
        <EmptyState message={t.jobs.noAttempts} />
      ) : (
        detail.attempts.map((a) => (
          <div key={a.id} className={styles.attempt}>
            <div className={styles.attemptHead}>
              <strong className={styles.attemptUser}>{a.username}</strong>
              <span>{candidateStateLabel(a.state)}</span>
              {a.failReason && <span className={styles.attemptFail}>{a.failReason}</span>}
              <span className={styles.attemptMeta}>
                {formatDateTime(a.createdAt)} — {t.jobs.fileCount(a.fileCount)}
              </span>
            </div>
            {/* Copied before sorting: this array belongs to the query cache,
                and sorting in place would mutate it for every other reader. */}
            {[...a.transfers]
              .sort((x, y) => compareFileNames(x.filename, y.filename))
              .map((tr) => {
              const { tone, live } = transferTone(tr);
              const pct = percent(tr.bytesDone, tr.bytesTotal);
              return (
                <div key={tr.filename} className={styles.transfer}>
                  <span className={styles.transferName}>{basename(tr.filename)}</span>
                  <div className={styles.ticksWrap}>
                    <Ticks percent={pct} count={TRANSFER_TICKS} tone={tone} live={live} height={7} />
                  </div>
                  <span className={`${table.mono} ${styles.transferBytes}`}>
                    {formatSize(tr.bytesDone)} / {formatSize(tr.bytesTotal)}
                  </span>
                  <span className={styles.transferExtra}>
                    {/* speed and queue position are live-only and mutually exclusive
                        in practice (downloading vs waiting in the peer's queue); each
                        is absent unless the native backend reported it. */}
                    {tr.speed ? formatSpeed(tr.speed) : ''}
                    {tr.speed && tr.queuePosition ? ' · ' : ''}
                    {tr.queuePosition ? t.jobs.queuePosition(tr.queuePosition) : ''}
                  </span>
                  <span className={styles.transferRetries}>
                    {tr.retries > 0 ? t.jobs.transferRetries(tr.retries) : ''}
                  </span>
                </div>
              );
            })}
          </div>
        ))
      )}

      <SectionHeader label={t.jobs.events} />
      {eventsLoading || eventsIsPlaceholder ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !events?.length ? (
        <EmptyState message={t.jobs.noEvents} />
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
