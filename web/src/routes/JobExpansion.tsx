import { useJobDetail } from '../api/queries';
import type { AttemptDetail, Job } from '../api/types';
import JobActions from '../components/JobActions';
import ParkedExplanation from '../components/ParkedExplanation';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import { basename, compareFileNames, formatEta, formatSize } from '../format';
import { t } from '../strings';
import styles from './JobExpansion.module.css';

const MAX_FILES_SHOWN = 7;

// The current candidate is the in-progress attempt, or — once nothing is
// active (e.g. the job just failed or succeeded) — whichever attempt was
// touched most recently. Falls to undefined for a job that hasn't reached
// SELECTING yet, i.e. has no attempts at all.
function currentCandidate(attempts: AttemptDetail[]): AttemptDetail | undefined {
  if (attempts.length === 0) return undefined;
  const active = attempts.find((a) => a.state === 'ACTIVE');
  if (active) return active;
  return [...attempts].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))[0];
}

// Seconds elapsed since the job last changed state. album_jobs.updated_at is
// only touched by SetState (internal/store/jobs.go), never by per-byte
// progress on the transfers table, so this is genuinely "time in state" and
// not "time since the last poll saw movement".
function secondsInState(job: Job): number | undefined {
  if (!job.updatedAt) return undefined;
  const ms = Date.now() - new Date(job.updatedAt).getTime();
  if (Number.isNaN(ms)) return undefined;
  return Math.max(0, Math.floor(ms / 1000));
}

export default function JobExpansion({ job, onCollapse }: { job: Job; onCollapse: () => void }) {
  const detailQuery = useJobDetail(job.id);
  const { data: detail } = detailQuery;
  const phase = queryPhase(detailQuery);
  // hasData() also closes a latent hole the bare `isLoading` check left: with
  // keepPreviousData, expanding row B right after row A returned the *A*
  // detail with isLoading false, so B's file list showed A's files.
  const candidate = hasData(phase) && detail ? currentCandidate(detail.attempts) : undefined;
  const queued = job.status === 'active' && (job.queuePosition ?? 0) > 0;
  const elapsed = secondsInState(job);

  return (
    <div className={styles.wrap}>
      {job.failReason && (
        <div className={styles.reasonBox}>
          <span className={styles.reasonTitle}>{t.status[job.status]}</span> — {job.failReason}
        </div>
      )}
      <ParkedExplanation state={job.state} className={styles.reasonBox} />

      <div className={styles.columns}>
        {/* The meta tree only reads fields already on `job`, so it renders
            immediately rather than waiting on useJobDetail — only the FILES
            column below depends on that fetch. */}
        <div className={styles.metaRows}>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>├</span>
            <span className={styles.metaKey}>{t.jobs.peerLabel}</span>
            <span className={styles.metaValue}>{job.peer || '—'}</span>
          </div>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>├</span>
            <span className={styles.metaKey}>{t.jobs.sourceLabel}</span>
            <span className={styles.metaValueDim}>{job.source === 'manual' ? t.source.manual : t.source.lidarr}</span>
          </div>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>├</span>
            <span className={styles.metaKey}>{t.jobs.queuePositionLabel}</span>
            <span className={queued ? styles.metaValueDim : styles.metaValueFaint}>
              {queued ? t.jobs.queuePositionMeta(job.queuePosition!) : '—'}
            </span>
          </div>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>├</span>
            <span className={styles.metaKey}>{t.jobs.timeInStateLabel}</span>
            <span className={styles.metaValueDim}>{elapsed === undefined ? '—' : formatEta(elapsed)}</span>
          </div>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>├</span>
            <span className={styles.metaKey}>{t.jobs.qualityLabel}</span>
            <span className={styles.metaValueDim}>{job.format ?? '—'}</span>
          </div>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>├</span>
            <span className={styles.metaKey}>{t.jobs.transferredLabel}</span>
            <span className={styles.metaValueDim}>{`${formatSize(job.bytesDone)} / ${formatSize(job.bytesTotal)}`}</span>
          </div>
          <div className={styles.metaRow}>
            <span className={styles.glyph}>└</span>
            <span className={styles.metaKey}>{t.jobs.jobIdLabel}</span>
            <span className={styles.metaValueFaint}>#{job.id}</span>
          </div>
        </div>

        <div>
          <div className={styles.colTitle}>{t.jobs.files}</div>
          <QueryNotice phase={phase} />
          {hasData(phase) &&
            (!candidate ? (
              <div className={styles.placeholder}>{t.jobs.noCandidate}</div>
            ) : (
              <div className={styles.fileList}>
                {/* Sorted before truncating: the API returns transfers in
                    arbitrary order, so "the first five" would otherwise be five
                    arbitrary files rather than the first five tracks. Copied
                    because the array belongs to the query cache. */}
                {[...candidate.transfers]
                  .sort((x, y) => compareFileNames(x.filename, y.filename))
                  .slice(0, MAX_FILES_SHOWN)
                  .map((tr) => (
                  <div key={tr.filename} className={styles.fileRow}>
                    <span className={tr.state === 'COMPLETED' ? styles.markDone : styles.markPending}>
                      {tr.state === 'COMPLETED' ? '✓' : '·'}
                    </span>
                    <span className={styles.fileName}>{basename(tr.filename)}</span>
                    <span className={styles.fileSize}>{formatSize(tr.bytesTotal)}</span>
                  </div>
                ))}
                {candidate.transfers.length > MAX_FILES_SHOWN && (
                  <div className={styles.moreFiles}>
                    {t.jobs.moreFiles(candidate.transfers.length - MAX_FILES_SHOWN)}
                  </div>
                )}
              </div>
            ))}
        </div>
      </div>

      <div className={styles.actionsRow}>
        <JobActions jobId={job.id} state={job.state} onDeleted={onCollapse} />
      </div>
    </div>
  );
}
