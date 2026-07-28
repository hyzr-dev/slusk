import { Link, useNavigate, useParams } from 'react-router-dom';
import { useJobDetail, useJobEvents } from '../api/queries';
import type { JobState, TransferDetail } from '../api/types';
import JobActions from '../components/JobActions';
import ParkedExplanation from '../components/ParkedExplanation';
import EmptyState from '../components/tui/EmptyState';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
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
  const detailQuery = useJobDetail(id);
  const eventsQuery = useJobEvents(id);
  const { data: detail } = detailQuery;
  const { data: events } = eventsQuery;
  const detailPhase = queryPhase(detailQuery);
  const eventsPhase = queryPhase(eventsQuery);

  // hasData() is exactly the old `detail !== undefined && !detailIsPlaceholder`
  // — see ownPhase() in QueryNotice.tsx, which carries the keepPreviousData
  // rationale that used to live here. `detail.job` is now the single source
  // of everything the header renders (issue #268) — jobDetailDTO carries a
  // whole jobDTO, built by the same toJobDTO the REST job list and the
  // stream's own detail frame use, so there is no second, independently
  // polled source left to reconcile against.
  const detailReady = hasData(detailPhase);
  const job = detailReady ? detail?.job : undefined;

  const actionState: JobState = job?.state ?? 'WANTED';
  const parkedState = job?.state === 'PARKED' ? 'PARKED' : undefined;

  // A notBefore in the past has no display relevance.
  const sleepingUntil =
    job?.notBefore && new Date(job.notBefore) > new Date() ? job.notBefore : '';

  return (
    <>
      <Link to="/jobs" className={styles.back}>{t.jobs.back}</Link>

      {/* Two-tier fallback: the job header (from detail.job), or loading/error
          via QueryNotice — collapsed from three tiers now that detail.job is
          a whole jobDTO rather than a hand-picked subset (issue #268). The
          middle tier this replaces existed only to keep the page useful
          after a job aged out of the now-deleted GET /api/jobs/all; that
          concern is gone along with the endpoint. QueryNotice still reports
          "loading" or "failed" (e.g. a genuinely missing job id) here, so
          this never silently renders a blank page — see hasData/QueryNotice. */}
      {job ? (
        <>
          <SectionHeader label={job.title} meta={job.artist} prominent />
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
      ) : (
        <QueryNotice phase={detailPhase} />
      )}

      <ParkedExplanation state={parkedState} className={styles.failReason} />

      <div className={styles.actionsWrap}>
        <JobActions jobId={id} state={actionState} onDeleted={() => navigate('/jobs')} />
      </div>

      <SectionHeader label={t.jobs.attemptHistory} />
      <QueryNotice phase={detailPhase} />
      {hasData(detailPhase) &&
        (!detail?.attempts.length ? (
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
        ))}

      <SectionHeader label={t.jobs.events} />
      <QueryNotice phase={eventsPhase} />
      {hasData(eventsPhase) &&
        (!events?.length ? (
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
        ))}
    </>
  );
}
