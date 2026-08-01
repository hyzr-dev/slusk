import { useUploadHistory } from '../api/queries';
import type { UploadHistoryEntry, UploadHistoryStatus } from '../api/types';
import Button from '../components/tui/Button';
import EmptyState from '../components/tui/EmptyState';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import tagStyles from '../components/tui/Tag.module.css';
import { formatDateTime, formatSize, formatSpeed, formatVirtualPath } from '../format';
import { t } from '../strings';
import styles from './Shares.module.css';

const DASH = '—';

// Only one hue in the whole list, and it is on the rows worth noticing.
// 'completed' is the overwhelming majority — painting it --ok would make the
// panel a wall of green and cost the hue exactly the scarcity that makes it
// read (CLAUDE.md, "colour is signal"). 'aborted' is a transfer that broke
// mid-flight, which is the thing someone opens this list to find, so it takes
// --bad. 'rejected' is our own refusal, expected rather than wrong, and its
// reason is already spelled out on the detail line below the row.
const TONE: Record<UploadHistoryStatus, string> = {
  completed: tagStyles.neutral,
  aborted: tagStyles.bad,
  rejected: tagStyles.quiet,
};

/**
 * The bytes actually moved, or nothing at all.
 *
 * formatSize(0) renders "0 MB" — a measurement, not an absence — which on a
 * rejected row (nothing was ever streamed) is precisely the plausible
 * fabrication CLAUDE.md's "never invent data" forbids. Hence the guards:
 * every path that could reach formatSize with a zero returns DASH instead.
 * An aborted row shows sent-of-total, mirroring UploadsPanel's in-flight
 * line, because the gap between the two IS the diagnostic.
 */
function transferred(e: UploadHistoryEntry): string {
  if (e.status === 'rejected') return DASH;
  if (e.status === 'aborted') {
    if (e.bytesSent === 0) return DASH;
    return e.size > 0 ? `${formatSize(e.bytesSent)} / ${formatSize(e.size)}` : formatSize(e.bytesSent);
  }
  return e.size > 0 ? formatSize(e.size) : DASH;
}

// A separate component rather than inline JSX in Shares, for the same reason
// UploadsPanel is one: a hook cannot be conditional, so calling
// useUploadHistory() at the top of Shares would fire it even in the
// `!data.enabled` early-return branch above it — one wasted request per
// Shares mount rather than a poll, since this query has no refetchInterval.
export default function UploadHistory() {
  const historyQuery = useUploadHistory();
  const phase = queryPhase(historyQuery);
  const entries = historyQuery.data?.pages.flatMap((p) => p.uploads) ?? [];

  return (
    <Panel>
      <SectionHeader label={t.uploads.historyTitle} />
      <QueryNotice phase={phase} />
      {hasData(phase) && entries.length === 0 && <EmptyState message={t.uploads.historyEmpty} />}
      {hasData(phase) && entries.map((e) => (
        <div className={styles.historyRow} key={e.id}>
          <div className={styles.uploadHead}>
            <span className={`${tagStyles.tag} ${TONE[e.status]}`}>{t.uploads.historyStatus[e.status]}</span>
            <span className={styles.uploadFile} title={e.filename}>{formatVirtualPath(e.filename)}</span>
          </div>
          <div className={styles.historyMeta}>
            <span>
              {t.uploads.toPeerPrefix} <span className={styles.mono}>{e.username}</span>
            </span>
            <span>{formatDateTime(e.finishedAt)}</span>
            <span>{transferred(e)}</span>
            <span>
              {/* formatSpeed already answers '—' for 0, which is what a
                  rejected row's avgBytesPerSecond is; no guard needed here. */}
              {formatSpeed(e.avgBytesPerSecond)}
            </span>
          </div>
          {e.detail && <div className={styles.historyDetail}>{e.detail}</div>}
        </div>
      ))}
      {historyQuery.hasNextPage && (
        <div className={styles.historyFooter}>
          <Button
            type="button"
            variant="ghost"
            onClick={() => void historyQuery.fetchNextPage()}
            disabled={historyQuery.isFetchingNextPage}
          >
            {t.uploads.historyLoadOlder}
          </Button>
        </div>
      )}
    </Panel>
  );
}
