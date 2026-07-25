import { Link } from 'react-router-dom';
import { ApiError } from '../api/client';
import { useRescanShares, useShares } from '../api/queries';
import StatCard from '../components/StatCard';
import table from '../components/Table.module.css';
import { formatDateTime, formatSize } from '../format';
import { t } from '../strings';
import styles from './Shares.module.css';

export default function Shares() {
  const { data } = useShares();
  const rescan = useRescanShares();

  // `data` is undefined on first paint and `enabled` has no safe default to
  // branch on meanwhile — rendering the disabled notice (or the empty-shares
  // warning) before the real report arrives would flash the wrong state, so
  // render nothing until the query settles (the sticky header already shows
  // the "Shares" title regardless of load state). Unlike Peers/Overview,
  // there is no empty-array fallback that is also a valid rendered state here.
  if (!data) {
    return null;
  }

  if (!data.enabled) {
    return <div className={styles.notice}>{t.shares.disabledNotice}</div>;
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

  return (
    <>
      {data.folders.length === 0 && (
        <div className={styles.warningCard}>
          <svg
            width="22"
            height="22"
            viewBox="0 0 24 24"
            fill="none"
            stroke="var(--orphaned)"
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

      <div className={styles.cards}>
        <StatCard label={t.shares.statFiles} value={data.files} />
        <StatCard label={t.shares.statSize} value={formatSize(data.totalBytes)} />
        <StatCard
          label={t.columns.lastIndexed}
          value={data.indexedAt ? formatDateTime(data.indexedAt) : t.shares.statNever}
        />
      </div>

      <div className={styles.panel}>
        <div className={styles.panelHeader}>
          <div className={styles.panelTitle}>{t.shares.panelTitle}</div>
          <button
            className={styles.rescanButton}
            disabled={scanning}
            onClick={() => rescan.mutate()}
          >
            {scanning ? (
              <span className={styles.spinner} />
            ) : (
              <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <path d="M20 11a8 8 0 1 0-2.3 5.7" />
                <path d="M20 4v6h-6" />
              </svg>
            )}
            {scanning ? t.shares.rescanning : t.shares.rescan}
          </button>
        </div>
        {/* The live region stays mounted and only takes on styling once it has
            content: a role="status" node inserted at the same moment as its
            text is unreliably announced, and .rescanError carries padding and
            a background that would otherwise show as an empty bar. */}
        <div className={rescanMessage ? styles.rescanError : undefined} role="status">
          {rescanMessage}
        </div>

        <table className={`${table.table} ${styles.tableInPanel}`}>
          <thead>
            <tr>
              <th className={table.th}>{t.columns.path}</th>
              <th className={`${table.th} ${styles.alignRight}`}>{t.columns.files}</th>
              <th className={`${table.th} ${styles.alignRight}`}>{t.columns.size}</th>
              <th className={`${table.th} ${styles.alignRight}`}>{t.columns.lastIndexed}</th>
            </tr>
          </thead>
          <tbody>
            {data.folders.length === 0 && (
              <tr><td className={table.empty} colSpan={4}>{t.shares.empty}</td></tr>
            )}
            {data.folders.map((f) => (
              <tr key={f.path}>
                <td className={`${table.td} ${table.mono}`}>{f.path}</td>
                <td className={`${table.td} ${table.mono} ${styles.alignRight}`}>{f.files}</td>
                <td className={`${table.td} ${table.mono} ${styles.alignRight}`}>{formatSize(f.totalBytes)}</td>
                <td className={`${table.td} ${styles.alignRight}`}>
                  {data.indexedAt ? formatDateTime(data.indexedAt) : t.shares.statNever}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className={styles.footerNote}>
          {t.shares.footerNotePrefix}{' '}
          <Link to="/settings">{t.nav.settings}</Link>.{' '}
          {t.shares.footerNoteReadOnlyFallback}{' '}
          <span className={styles.mono}>{t.shares.footerNoteConfigFile}</span>.
        </div>
      </div>
    </>
  );
}
