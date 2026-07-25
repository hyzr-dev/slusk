import { useJobDetail } from '../api/queries';
import type { AttemptDetail, Job } from '../api/types';
import JobActions from '../components/JobActions';
import SourceBadge from '../components/SourceBadge';
import { formatBytes, formatDateTime } from '../format';
import { t } from '../strings';
import styles from './JobExpansion.module.css';

const MAX_FILES_SHOWN = 7;

// Not every candidate state maps to a real accent color — SUCCEEDED/others
// fall back to the neutral file-row dot rather than inventing a new palette
// entry for a state this box never needs to distinguish further.
const TRANSFER_DOT: Record<string, string> = {
  IN_PROGRESS: styles.dotActive ?? '',
  PENDING: styles.dotQueued ?? '',
  ERRORED: styles.dotFailed ?? '',
  CANCELLED: styles.dotFailed ?? '',
};

function basename(path: string): string {
  const idx = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'));
  return idx === -1 ? path : path.slice(idx + 1);
}

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

export default function JobExpansion({ job, onCollapse }: { job: Job; onCollapse: () => void }) {
  const { data: detail, isLoading } = useJobDetail(job.id);
  const candidate = detail ? currentCandidate(detail.attempts) : undefined;

  return (
    <div className={styles.wrap}>
      {job.failReason && (
        <div className={`${styles.reasonBox} ${styles[job.status] ?? ''}`}>
          <span className={styles.reasonTitle}>{t.status[job.status]}</span> — {job.failReason}
        </div>
      )}

      {isLoading ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !candidate ? (
        <div className={styles.placeholder}>{t.jobs.noCandidate}</div>
      ) : (
        <div className={styles.columns}>
          <div>
            <div className={styles.colTitle}>{t.jobs.peerAndTransfer}</div>
            <div className={styles.detailRows}>
              <div className={styles.detailRow}>
                <span className={styles.detailKey}>{t.jobs.peerLabel}</span>
                <span className={styles.detailValue}>{candidate.username || '—'}</span>
              </div>
              <div className={styles.detailRow}>
                <span className={styles.detailKey}>{t.jobs.sourceLabel}</span>
                <SourceBadge source={job.source} />
              </div>
              {job.queuePosition ? (
                <div className={styles.detailRow}>
                  <span className={styles.detailKey}>{t.jobs.queuePositionLabel}</span>
                  <span className={styles.detailValue}>{t.jobs.queuePosition(job.queuePosition)}</span>
                </div>
              ) : null}
              <div className={styles.detailRow}>
                <span className={styles.detailKey}>{t.jobs.qualityLabel}</span>
                <span className={styles.detailValue}>{job.format ?? '—'}</span>
              </div>
              <div className={styles.detailRow}>
                <span className={styles.detailKey}>{t.jobs.sizeLabel}</span>
                <span className={styles.detailValue}>{formatBytes(job.bytesTotal)}</span>
              </div>
              <div className={styles.detailRow}>
                <span className={styles.detailKey}>{t.jobs.downloadedLabel}</span>
                <span className={styles.detailValue}>{formatBytes(job.bytesDone)}</span>
              </div>
              <div className={styles.detailRow}>
                <span className={styles.detailKey}>{t.jobs.jobIdLabel}</span>
                <span className={styles.detailValue}>#{job.id}</span>
              </div>
            </div>
          </div>

          <div>
            <div className={styles.colTitle}>{t.jobs.files}</div>
            <div className={styles.fileList}>
              {candidate.transfers.slice(0, MAX_FILES_SHOWN).map((tr) => (
                <div key={tr.filename} className={styles.fileRow}>
                  <span className={`${styles.dot} ${TRANSFER_DOT[tr.state] ?? ''}`} />
                  <span className={styles.fileName}>{basename(tr.filename)}</span>
                  <span className={styles.fileSize}>{formatBytes(tr.bytesTotal)}</span>
                </div>
              ))}
              {candidate.transfers.length > MAX_FILES_SHOWN && (
                <div className={styles.moreFiles}>
                  {t.jobs.moreFiles(candidate.transfers.length - MAX_FILES_SHOWN)}
                </div>
              )}
            </div>
            <div className={styles.candidateMeta}>{formatDateTime(candidate.updatedAt)}</div>
          </div>
        </div>
      )}

      <div className={styles.actionsRow}>
        <JobActions jobId={job.id} state={job.state} onDeleted={onCollapse} />
      </div>
    </div>
  );
}
