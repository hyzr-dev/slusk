import { useUploads } from '../api/queries';
import EmptyState from '../components/tui/EmptyState';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import Ticks from '../components/tui/Ticks';
import tagStyles from '../components/tui/Tag.module.css';
import { formatSize, formatVirtualPath, percent } from '../format';
import { t } from '../strings';
import styles from './Shares.module.css';

// A separate component rather than inline JSX in Shares, deliberately: hooks
// cannot be conditional, so a useUploads() call at the top of Shares would
// keep polling /api/uploads every 3s even in the `!data.enabled` early-return
// branch above it. Mounting <UploadsPanel /> only inside the enabled branch
// ties this query's lifetime to the panel actually being on screen.
export default function UploadsPanel() {
  const uploadsQuery = useUploads();
  const { data } = uploadsQuery;
  const phase = queryPhase(uploadsQuery);

  // The one case that still renders nothing: the server has answered and
  // said it is not serving uploads. That is a settled fact about a feature
  // that is off, and Shares renders its own disabledNotice when the whole
  // native-sharing feature is off. Unresolved and failed no longer land here
  // (see the QueryNotice below), so an absent panel now means exactly this
  // and nothing else (issue #201).
  if (data && !data.enabled) return null;

  return (
    <Panel className={styles.uploadsSection}>
      <SectionHeader
        label={t.uploads.panelTitle}
        // active/slots are unknown until the report arrives; "0 of 0 slots"
        // would be a claim rather than a placeholder.
        meta={data && <span className={styles.slotsBadge}>{t.uploads.slotsInUse(data.active, data.slots)}</span>}
      />

      <QueryNotice phase={phase} />

      {hasData(phase) && data && data.uploads.length === 0 && <EmptyState message={t.uploads.empty} />}

      {hasData(phase) && data && data.uploads.map((u, i) => {
        // UploadEntry (internal/observ/uploads.go) carries no speed field —
        // unlike a download job there is nothing truthful to show there, so
        // the head line carries only the marker, filename and a real,
        // computed percentage. A queued entry is not transferring at all: it
        // gets tone="queued" and no `live`, so its bar never flares as if
        // bytes were moving.
        const pct = u.active ? percent(u.bytesWritten, u.size) : 0;
        // Filename is not a stable per-row key on its own: the same peer can
        // requeue the same file, and truncation can put two distinct entries
        // momentarily side by side with equal (username, filename). Position
        // in the already-ordered list (active first, then queue order) is
        // stable enough for a polled, append-only-looking list like this.
        return (
          <div className={styles.uploadRow} key={`${u.username}-${u.filename}-${i}`}>
            <div className={styles.uploadHead}>
              <span className={`${tagStyles.tag} ${u.active ? tagStyles.neutral : tagStyles.quiet}`}>
                {u.active ? t.tag.UL : t.tag.QU}
              </span>
              <span className={styles.uploadFile} title={u.filename}>
                {formatVirtualPath(u.filename)}
              </span>
              <span className={`${styles.uploadPct} ${u.active ? styles.pctBar : styles.pctQueued}`}>
                {u.active ? `${pct}%` : '—'}
              </span>
            </div>
            <div className={styles.uploadTicks}>
              <Ticks percent={pct} tone={u.active ? 'bar' : 'queued'} live={u.active} height={12} />
            </div>
            <div className={styles.uploadSub}>
              <span>
                {t.uploads.toPeerPrefix} <span className={styles.mono}>{u.username}</span>
              </span>
              <span>
                {u.active
                  ? // size is 0 between dispatch marking the job active and
                    // runUpload resolving the share entry. Rendering that
                    // window would claim a 0-byte file, so the caption is
                    // suppressed until size resolves — the bar itself stays.
                    u.size > 0 && `${formatSize(u.bytesWritten)} / ${formatSize(u.size)}`
                  : t.uploads.queuePlace(u.position)}
              </span>
            </div>
          </div>
        );
      })}

      {data && data.truncated > 0 && <div className={styles.footerNote}>{t.uploads.truncated(data.truncated)}</div>}
    </Panel>
  );
}
