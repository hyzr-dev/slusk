import { Fragment, useState } from 'react';
import { Link } from 'react-router-dom';
import { useJobs } from '../api/queries';
import SourceBadge from '../components/SourceBadge';
import pill from '../components/StatusPill.module.css';
import StatusPill from '../components/StatusPill';
import table from '../components/Table.module.css';
import { formatEta, formatSpeed, percent } from '../format';
import PageHeading from '../components/PageHeading';
import { t } from '../strings';
import { countByStatus, matchesFilters, type SourceFilter, type StatusFilter } from './jobFilter';
import JobExpansion from './JobExpansion';
import styles from './Jobs.module.css';

const STATUS_CHIPS: { key: StatusFilter; label: string }[] = [
  { key: 'all', label: t.jobs.statusAll },
  { key: 'active', label: t.status.active },
  { key: 'queued', label: t.status.queued },
  { key: 'importing', label: t.jobs.statusImporting },
  { key: 'stalled', label: t.status.stalled },
  { key: 'failed', label: t.status.failed },
  { key: 'orphaned', label: t.status.orphaned },
  { key: 'done', label: t.status.done },
];

const SOURCE_CHIPS: { key: SourceFilter; label: string }[] = [
  { key: 'all', label: t.jobs.sourceAll },
  { key: 'manual', label: t.jobs.sourceManual },
  { key: 'lidarr', label: t.jobs.sourceLidarr },
];

export default function Jobs() {
  const { data: jobs = [] } = useJobs();
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState<StatusFilter>('all');
  const [source, setSource] = useState<SourceFilter>('all');
  const [expandedId, setExpandedId] = useState<number | null>(null);

  const filtered = jobs.filter((j) => matchesFilters(j, search, status, source));
  const counts = countByStatus(jobs, search, source);

  const filtersActive = search.trim() !== '' || status !== 'all' || source !== 'all';
  const summaryParts: string[] = [];
  if (source !== 'all') summaryParts.push(source === 'manual' ? t.jobs.sourceManual : t.jobs.sourceLidarr);
  if (status !== 'all') summaryParts.push(status === 'importing' ? t.jobs.statusImporting : t.status[status]);
  if (search.trim()) summaryParts.push(`"${search.trim()}"`);

  function clearFilters() {
    setSearch('');
    setStatus('all');
    setSource('all');
  }

  function toggleExpanded(id: number) {
    setExpandedId((prev) => (prev === id ? null : id));
  }

  return (
    <>
      <PageHeading>{t.nav.jobs}</PageHeading>

      <div className={styles.controlsRow}>
        <input
          className={styles.input}
          type="text"
          placeholder={t.jobs.searchPlaceholder}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <div className={styles.chipGroup}>
          <span className={styles.chipGroupLabel}>{t.jobs.sourceLabel}</span>
          {SOURCE_CHIPS.map((c) => (
            <button
              key={c.key}
              type="button"
              // The row itself is also a role="button" whose accessible name
              // is its full text content, which can start with the same word
              // as a chip label (e.g. a "Failed" status pill inside a row);
              // an explicit aria-label keeps each chip's accessible name
              // exactly its label, not the label plus the count span's text.
              aria-label={c.label}
              className={`${styles.sourceChip} ${source === c.key ? styles.chipSelected : ''} ${
                c.key === 'manual' ? styles.chipManual : c.key === 'lidarr' ? styles.chipLidarr : ''
              }`}
              onClick={() => setSource(c.key)}
            >
              {c.label}
            </button>
          ))}
        </div>
      </div>

      <div className={styles.chipGroup}>
        <span className={styles.chipGroupLabel}>{t.columns.status}</span>
        {STATUS_CHIPS.map((c) => (
          <button
            key={c.key}
            type="button"
            aria-label={c.label}
            className={`${styles.statusChip} ${status === c.key ? styles.chipSelected : ''} ${
              c.key !== 'all' ? styles[`chip_${c.key}`] : ''
            }`}
            onClick={() => setStatus(c.key)}
          >
            {c.label}
            <span className={styles.chipCount}>
              {c.key === 'all' ? jobs.length : counts[c.key]}
            </span>
          </button>
        ))}
        {filtersActive && (
          <button type="button" className={styles.clearButton} onClick={clearFilters}>
            {t.jobs.clearFilters(summaryParts.join(' · '))}
          </button>
        )}
      </div>

      <div className={styles.tableWrap}>
        <table className={table.table}>
          <colgroup>
            <col style={{ width: 112 }} />
            <col />
            <col style={{ width: 126 }} />
            <col style={{ width: 74 }} />
            <col style={{ width: 180 }} />
            <col style={{ width: 96 }} />
            <col style={{ width: 64 }} />
            <col style={{ width: 56 }} />
            <col style={{ width: 34 }} />
          </colgroup>
          <thead>
            <tr>
              <th className={table.th}>{t.columns.status}</th>
              <th className={table.th}>{t.columns.album}</th>
              <th className={table.th}>{t.columns.peer}</th>
              <th className={table.th}>{t.columns.format}</th>
              <th className={table.th}>{t.columns.progress}</th>
              <th className={table.th}>{t.columns.speed}</th>
              <th className={table.th}>{t.columns.eta}</th>
              <th className={table.th}>{t.columns.retries}</th>
              <th className={table.th} aria-hidden />
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 ? (
              <tr>
                <td className={table.empty} colSpan={9}>{t.jobs.noMatch}</td>
              </tr>
            ) : (
              filtered.map((j) => {
                const expanded = expandedId === j.id;
                const inQueue = (j.queuePosition ?? 0) > 0;
                return (
                  <Fragment key={j.id}>
                    <tr
                      className={table.rowClickable}
                      tabIndex={0}
                      role="button"
                      aria-expanded={expanded}
                      onClick={() => toggleExpanded(j.id)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          toggleExpanded(j.id);
                        }
                      }}
                    >
                      <td className={table.td}>
                        {inQueue ? (
                          <span className={`${pill.pill} ${pill.stalled}`}>{t.jobs.inPeerQueue}</span>
                        ) : (
                          <StatusPill status={j.status} state={j.state} />
                        )}
                      </td>
                      <td className={table.td}>
                        <div className={styles.titleRow}>
                          <Link
                            to={`/jobs/${j.id}`}
                            className={styles.idLink}
                            onClick={(e) => e.stopPropagation()}
                          >
                            {j.title}
                          </Link>
                          <SourceBadge source={j.source} />
                        </div>
                        <div className={styles.sub}>
                          {j.year ? `${j.artist} · ${j.year}` : j.artist}
                        </div>
                      </td>
                      <td className={`${table.td} ${table.mono} ${styles.ellipsis}`}>{j.peer || '—'}</td>
                      <td className={`${table.td} ${table.mono}`}>{j.format ?? '—'}</td>
                      <td className={table.td}>
                        <div className={styles.progressRow}>
                          <div className={styles.progressBar}>
                            <div
                              className={`${styles.progressFill} ${inQueue ? styles.progressHatched : ''}`}
                              style={{ width: inQueue ? '100%' : `${percent(j.bytesDone, j.bytesTotal)}%` }}
                            />
                          </div>
                          <span className={table.mono}>
                            {j.status === 'queued'
                              ? '—'
                              : inQueue
                                ? t.jobs.queuePosition(j.queuePosition!)
                                : `${percent(j.bytesDone, j.bytesTotal)}%`}
                          </span>
                        </div>
                        <div className={styles.progressSub}>
                          {inQueue
                            ? t.jobs.inPeerQueue
                            : j.state === 'IMPORTING'
                              ? t.jobs.verifying
                              : j.state}
                        </div>
                      </td>
                      <td className={`${table.td} ${table.mono} ${styles.right}`}>{formatSpeed(j.speed)}</td>
                      <td className={`${table.td} ${table.mono} ${styles.right}`}>{formatEta(j.etaSeconds)}</td>
                      <td className={`${table.td} ${styles.center}`}>
                        {j.retries > 0 ? (
                          <span className={styles.retryPill}>{j.retries}</span>
                        ) : (
                          <span className={styles.retryDim}>{j.retries}</span>
                        )}
                      </td>
                      <td className={`${table.td} ${styles.center}`}>
                        <span className={`${styles.chevron} ${expanded ? styles.chevronExpanded : ''}`}>
                          ›
                        </span>
                      </td>
                    </tr>
                    {expanded && (
                      <tr>
                        <td colSpan={9} className={styles.expansionCell}>
                          <JobExpansion job={j} onCollapse={() => setExpandedId(null)} />
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}
