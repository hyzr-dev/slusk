import { Fragment, useState } from 'react';
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

// The row's own cell count (state tag, filename+caret, peer, speed), for the
// expansion's single-cell aria-colspan — mirrors Jobs.tsx's JobExpansion
// wrapper, see JobRowImpl's comment on why a row with a single cell in an
// N-column table has to say so.
const HISTORY_COLUMNS = 4;

// Only one hue in the whole list, and it is on the rows worth noticing.
// 'completed' is the overwhelming majority — painting it --ok would make the
// panel a wall of green and cost the hue exactly the scarcity that makes it
// read (CLAUDE.md, "colour is signal"). 'aborted' is a transfer that broke
// mid-flight, which is the thing someone opens this list to find, so it takes
// --bad. 'rejected' is our own refusal, expected rather than wrong, and its
// reason is already spelled out in the expansion below.
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
 * line, because the gap between the two IS the diagnostic. Rendered in the
 * expansion (issue #371); the guards move with it, they do not disappear.
 */
function transferred(e: UploadHistoryEntry): string {
  if (e.status === 'rejected') return DASH;
  if (e.status === 'aborted') {
    if (e.bytesSent === 0) return DASH;
    return e.size > 0 ? `${formatSize(e.bytesSent)} / ${formatSize(e.size)}` : formatSize(e.bytesSent);
  }
  return e.size > 0 ? formatSize(e.size) : DASH;
}

interface HistoryRowProps {
  entry: UploadHistoryEntry;
  expanded: boolean;
  onToggle: (id: number) => void;
}

// One row per finished upload (issue #371): state, filename, peer and avg
// speed stay on the row — the fields worth scanning a long list for — while
// size, finished-at and the failure detail move behind the caret, following
// Jobs.tsx's JobRowImpl expansion idiom.
function HistoryRow({ entry, expanded, onToggle }: HistoryRowProps) {
  const expansionId = `upload-history-expansion-${entry.id}`;
  return (
    <Fragment>
      <div
        role="row"
        className={`${styles.historyRow} ${expanded ? styles.historyRowExpanded : ''}`}
        onClick={() => onToggle(entry.id)}
      >
        <div className={styles.historyLine}>
          <div role="cell" className={styles.historyHead}>
            <span className={`${tagStyles.tag} ${TONE[entry.status]}`}>{t.uploads.historyStatus[entry.status]}</span>
            {/* The 24x24 hit area (Jobs.module.css's .caretButton, #222) is
                centred on the glyph and out of flow, so the row keeps its
                height and the filename keeps its position. This button sits
                inside a role="cell" wrapper — see JobRowImpl's .albumCell
                comment for why that wrapper, not the button, becomes the grid
                item, and verify the hit target in a browser; jsdom cannot
                fail on it. */}
            <button
              type="button"
              className={styles.historyCaretButton}
              onClick={(e) => {
                // Without stopPropagation the click also reaches the row
                // handler below and toggles a second time.
                e.stopPropagation();
                onToggle(entry.id);
              }}
              aria-expanded={expanded}
              aria-controls={expansionId}
              aria-label={expanded ? t.uploads.historyHideDetails : t.uploads.historyShowDetails}
            >
              <span aria-hidden className={styles.historyCaret}>{expanded ? '▾' : '▸'}</span>
            </button>
            <span className={styles.uploadFile} title={entry.filename}>{formatVirtualPath(entry.filename)}</span>
          </div>
          <span role="cell" className={styles.historyPeer}>
            {t.uploads.toPeerPrefix} <span className={styles.mono}>{entry.username}</span>
          </span>
          <span role="cell" className={styles.historySpeed}>
            {/* formatSpeed already answers '—' for 0, which is what a
                rejected row's avgBytesPerSecond is; no guard needed here. */}
            {formatSpeed(entry.avgBytesPerSecond)}
          </span>
        </div>
      </div>
      {expanded && (
        <div id={expansionId} role="row" className={styles.historyExpansionWrap}>
          {/* One cell spanning every column on the row above — an expansion
              is a row in the table, not a sibling of it. */}
          <div role="cell" aria-colspan={HISTORY_COLUMNS} className={styles.historyExpansionBody}>
            <div className={styles.historyExpansionRows}>
              <div className={styles.historyExpansionRow}>
                <span className={styles.historyExpansionKey}>{t.uploads.historySizeLabel}</span>
                <span className={styles.historyExpansionValue}>{transferred(entry)}</span>
              </div>
              <div className={styles.historyExpansionRow}>
                <span className={styles.historyExpansionKey}>{t.uploads.historyFinishedLabel}</span>
                <span className={styles.historyExpansionValue}>{formatDateTime(entry.finishedAt)}</span>
              </div>
            </div>
            {entry.detail && <div className={styles.historyDetail}>{entry.detail}</div>}
          </div>
        </div>
      )}
    </Fragment>
  );
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
  // One row open at a time, matching Jobs.tsx's expandedId.
  const [expandedId, setExpandedId] = useState<number | null>(null);
  const toggleExpanded = (id: number) => setExpandedId((prev) => (prev === id ? null : id));

  return (
    <Panel>
      <SectionHeader label={t.uploads.historyTitle} />
      <QueryNotice phase={phase} />
      {hasData(phase) && entries.length === 0 && <EmptyState message={t.uploads.historyEmpty} />}
      {hasData(phase) && entries.length > 0 && (
        <div role="table">
          {entries.map((e) => (
            <HistoryRow key={e.id} entry={e} expanded={expandedId === e.id} onToggle={toggleExpanded} />
          ))}
        </div>
      )}
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
