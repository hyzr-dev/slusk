import { Link } from 'react-router-dom';
import { ApiError } from '../api/client';
import { useRescanShares, useShares } from '../api/queries';
import { useFlash } from '../components/chrome/FlashContext';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import { formatDateTime, formatSize } from '../format';
import { t } from '../strings';
import styles from './Shares.module.css';
import UploadHistory from './UploadHistory';
import UploadsPanel from './UploadsPanel';

// The header's INDEXED reading turns --bad once the last scan is at least
// this old, matching the mock's treatment of an overdue index
// (docs/design/slusk-tui.dc.html:~430). No scan having ever completed is
// at least as stale as one, so it gets the same treatment. This is a
// report-level timestamp (SharesReport.indexedAt) — ShareFolder carries no
// per-folder equivalent, so it belongs only in the summary line, not in the
// folder grid (see internal/observ/shares.go ShareFolderStats).
const STALE_INDEX_MS = 24 * 60 * 60 * 1000;

export default function Shares() {
  const sharesQuery = useShares();
  const { data } = sharesQuery;
  const phase = queryPhase(sharesQuery);
  const rescan = useRescanShares();
  const flash = useFlash();

  // `data` is undefined on first paint and `enabled` has no safe default to
  // branch on meanwhile — rendering the disabled notice (or the empty-shares
  // warning) before the real report arrives would flash the wrong state, so
  // show only the query notice until the query settles. Unlike
  // Peers/Overview, there is no empty-array fallback that is also a valid
  // rendered state here.
  //
  // Truthiness is enough to mean "never answered" here and narrows `data` for
  // everything below: queryKeys.shares is a constant key, so keepPreviousData
  // can never substitute another key's response into it.
  if (!data) {
    return (
      <Page title={t.page.shares.title} subtitle={t.page.shares.subtitle}>
        <QueryNotice phase={phase} />
      </Page>
    );
  }

  if (!data.enabled) {
    return (
      <Page title={t.page.shares.title} subtitle={t.page.shares.subtitle}>
        <div className={styles.notice}>{t.shares.disabledNotice}</div>
      </Page>
    );
  }

  const scanning = data.scanning || rescan.isPending;

  let rescanMessage = '';
  if (rescan.isError) {
    const status = rescan.error instanceof ApiError ? rescan.error.status : 0;
    if (status === 409) {
      rescanMessage = t.shares.rescanConflict;
    } else if (status === 503) {
      rescanMessage = t.shares.rescanUnavailable;
    } else {
      rescanMessage = t.shares.rescanFailed;
    }
  }

  const stale = !data.indexedAt || Date.now() - new Date(data.indexedAt).getTime() > STALE_INDEX_MS;
  const indexedLabel = data.indexedAt ? formatDateTime(data.indexedAt) : t.shares.statNever;

  return (
    <Page title={t.page.shares.title} subtitle={t.page.shares.subtitle}>
      <QueryNotice phase={phase} />
      {data.folders.length === 0 && (
        <div className={styles.warningCard}>
          <svg
            width="22"
            height="22"
            viewBox="0 0 24 24"
            fill="none"
            stroke="var(--bad)"
            strokeWidth="2"
            strokeLinecap="round"
            className={styles.warningIcon}
            aria-hidden="true"
          >
            <path d="M12 9v4" />
            <path d="M12 17h.01" />
            <path d="M10.3 3.9 2 18a2 2 0 0 0 1.7 3h16.6a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z" />
          </svg>
          <div>
            <div className={styles.warningTitle}>{t.shares.emptyTitle}</div>
            <div className={styles.warningBody}>
              {t.shares.emptyBodyPrefix}{' '}
              <Link to="/settings">{t.nav.settings}</Link>
              {t.shares.emptyBodySuffix}
            </div>
            <pre className={styles.warningSnippet}>{t.shares.emptyConfigSnippet}</pre>
          </div>
        </div>
      )}

      <Panel>
        <SectionHeader
          label={t.shares.panelTitle}
          meta={
            <span className={styles.headerActions}>
              <span>{t.shares.summary(data.folders.length, data.files, formatSize(data.totalBytes))}</span>
              <span className={stale ? styles.indexedBad : styles.indexedOk}>
                {t.shares.indexedAt(indexedLabel)}
              </span>
              {scanning && (
                <span className={styles.indexing}>
                  <span className={styles.spinner} aria-hidden="true" />
                  {t.shares.indexing}
                </span>
              )}
              <button
                type="button"
                className={styles.rescanButton}
                disabled={scanning}
                onClick={() =>
                  rescan.mutate(undefined, { onSuccess: () => flash(t.shares.rescanStarted) })
                }
              >
                {t.shares.rescan}
              </button>
            </span>
          }
        />
        {/* The live region stays mounted and only takes on styling once it has
            content: a role="status" node inserted at the same moment as its
            text is unreliably announced, and .rescanError carries padding and
            a background that would otherwise show as an empty bar. */}
        <div className={rescanMessage ? styles.rescanError : undefined} role="status">
          {rescanMessage}
        </div>

        <div role="table">
          <div role="row" className={styles.folderGridHead}>
            <span role="columnheader">{t.shares.gridHead.path}</span>
            <span role="columnheader" className={styles.alignRight}>{t.shares.gridHead.files}</span>
            <span role="columnheader" className={styles.alignRight}>{t.shares.gridHead.size}</span>
          </div>
          {data.folders.map((f) => (
            <div key={f.path} role="row" className={styles.folderRow}>
              <span role="cell" className={styles.folderPath}>{f.path}</span>
              <span role="cell" className={styles.folderDim}>{f.files}</span>
              <span role="cell" className={styles.folderDim}>{formatSize(f.totalBytes)}</span>
            </div>
          ))}
        </div>
        {/* Outside the table: `role="table"` admits only rows, so an empty
            state nested inside would be invalid ARIA. */}
        {data.folders.length === 0 && <EmptyState message={t.shares.empty} />}
      </Panel>

      <UploadsPanel />
      <UploadHistory />
    </Page>
  );
}
