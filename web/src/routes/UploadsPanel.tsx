import { useUploads } from '../api/queries';
import ProgressBar from '../components/ProgressBar';
import { formatSize, formatVirtualPath } from '../format';
import { t } from '../strings';
import styles from './Shares.module.css';

// A separate component rather than inline JSX in Shares, deliberately: hooks
// cannot be conditional, so a useUploads() call at the top of Shares would
// keep polling /api/uploads every 3s even in the `!data.enabled` early-return
// branch above it. Mounting <UploadsPanel /> only inside the enabled branch
// ties this query's lifetime to the panel actually being on screen.
export default function UploadsPanel() {
  const { data } = useUploads();

  if (!data || !data.enabled) return null;

  return (
    <div className={`${styles.panel} ${styles.uploadsPanel}`}>
      <div className={styles.panelHeader}>
        <div className={styles.panelTitle}>{t.uploads.panelTitle}</div>
        <div className={styles.slotsBadge}>{t.uploads.slotsInUse(data.active, data.slots)}</div>
      </div>

      {data.uploads.length === 0 && <div className={styles.uploadsEmpty}>{t.uploads.empty}</div>}

      {data.uploads.map((u, i) => (
        // Filename is not a stable per-row key on its own: the same peer can
        // requeue the same file, and truncation can put two distinct entries
        // momentarily side by side with equal (username, filename). Position
        // in the already-ordered list (active first, then queue order) is
        // stable enough for a polled, append-only-looking list like this.
        <div className={styles.uploadRow} key={`${u.username}-${u.filename}-${i}`}>
          <span className={`${styles.uploadDot} ${u.active ? '' : styles.uploadDotQueued}`} />
          <div className={styles.uploadInfo}>
            <div className={styles.uploadFile} title={u.filename}>
              {formatVirtualPath(u.filename)}
            </div>
            <div className={styles.uploadPeer}>
              {t.uploads.toPeerPrefix} <span className={styles.mono}>{u.username}</span>
            </div>
          </div>
          {u.active ? (
            <div className={styles.uploadMeter}>
              <ProgressBar done={u.bytesWritten} total={u.size} />
              {/* size is 0 between dispatch marking the job active and
                  runUpload resolving the share entry. Rendering that window
                  would claim a 0-byte file; gated the same way Jobs.tsx gates
                  its byte caption on bytesTotal. */}
              {u.size > 0 && (
                <div className={styles.uploadMeterCaption}>
                  {formatSize(u.bytesWritten)} / {formatSize(u.size)}
                </div>
              )}
            </div>
          ) : (
            <div className={styles.uploadQueuePlace}>{t.uploads.queuePlace(u.position)}</div>
          )}
        </div>
      ))}

      {data.truncated > 0 && <div className={styles.footerNote}>{t.uploads.truncated(data.truncated)}</div>}
    </div>
  );
}
