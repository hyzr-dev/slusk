import { Fragment, useId, useState } from 'react';
import { Link } from 'react-router-dom';
import type { Job } from '../api/types';
import { useJobs } from '../api/queries';
import SourceBadge from '../components/SourceBadge';
import pill from '../components/StatusPill.module.css';
import StatusPill from '../components/StatusPill';
import table from '../components/Table.module.css';
import { formatEta, formatSpeed, percent } from '../format';
import PageHeading from '../components/PageHeading';
import { stateLabel, t } from '../strings';
import { countByStatus, matchesFilters, type SourceFilter, type StatusFilter } from './jobFilter';
import JobExpansion from './JobExpansion';
import styles from './Jobs.module.css';

const STATUS_CHIPS: { key: StatusFilter; label: string }[] = [
  { key: 'all', label: t.jobs.all },
  { key: 'active', label: t.status.active },
  { key: 'queued', label: t.status.queued },
  { key: 'importing', label: t.jobs.statusImporting },
  { key: 'stalled', label: t.status.stalled },
  { key: 'failed', label: t.status.failed },
  { key: 'orphaned', label: t.status.orphaned },
  { key: 'done', label: t.status.done },
];

const SOURCE_CHIPS: { key: SourceFilter; label: string }[] = [
  { key: 'all', label: t.jobs.all },
  { key: 'manual', label: t.source.manual },
  { key: 'lidarr', label: t.source.lidarr },
];

// The progress percentage's colour: done overrides everything else once a
// job hits 100%, a job waiting in a peer's queue reads with the same accent
// as its pill, an actively downloading job reads brightest, everything else
// (queued/stalled/failed/orphaned) is dim. Mirrors the mock's pctColorRaw.
function pctClass(job: Job, inQueue: boolean, pct: number): string {
  if (pct >= 100) return styles.pctDone;
  if (inQueue) return styles.pctQueued;
  if (job.status === 'active' && job.state !== 'IMPORTING') return styles.pctActive;
  return styles.pctOther;
}

// The progress bar fill colour by status, matching the mock's barColor().
// IMPORTING is a state refinement of the "active" status (see jobFilter.ts),
// so it's checked before falling through to the status switch.
function fillClass(job: Job): string {
  if (job.state === 'IMPORTING') return styles.fillImporting;
  switch (job.status) {
    case 'done':
      return styles.fillDone;
    case 'active':
      return styles.fillActive;
    case 'stalled':
      return styles.fillStalled;
    case 'failed':
    case 'orphaned':
      return styles.fillFailed;
    default:
      return styles.fillQueued;
  }
}

export default function Jobs() {
  const { data: jobs = [] } = useJobs();
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState<StatusFilter>('all');
  const [source, setSource] = useState<SourceFilter>('all');
  const [expandedId, setExpandedId] = useState<number | null>(null);
  const sourceLabelId = useId();
  const statusLabelId = useId();

  const filtered = jobs.filter((j) => matchesFilters(j, search, status, source));
  const counts = countByStatus(jobs, search, source);
  // What the "All" chip would show if clicked: every job matching source and
  // search regardless of status, i.e. the sum of every bucket above — not
  // jobs.length, which ignores source/search and so can disagree with what
  // actually renders when the chip is clicked.
  const allCount = Object.values(counts).reduce((sum, n) => sum + n, 0);

  const filtersActive = search.trim() !== '' || status !== 'all' || source !== 'all';
  const summaryParts: string[] = [];
  if (source !== 'all') summaryParts.push(source === 'manual' ? t.source.manual : t.source.lidarr);
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
        <div className={styles.chipGroup} role="group" aria-labelledby={sourceLabelId}>
          <span id={sourceLabelId} className={styles.chipGroupLabel}>{t.jobs.sourceLabel}</span>
          {SOURCE_CHIPS.map((c) => (
            <button
              key={c.key}
              type="button"
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

      <div className={styles.chipGroup} role="group" aria-labelledby={statusLabelId}>
        <span id={statusLabelId} className={styles.chipGroupLabel}>{t.columns.status}</span>
        {STATUS_CHIPS.map((c) => (
          <button
            key={c.key}
            type="button"
            className={`${styles.statusChip} ${status === c.key ? styles.chipSelected : ''} ${
              c.key !== 'all' ? styles[`chip_${c.key}`] : ''
            }`}
            onClick={() => setStatus(c.key)}
          >
            {c.label}
            {/* An explicit space, not just JSX layout, so the accessible
                name reads "Failed 1" rather than "Failed1" for assistive
                tech now that this button relies on its text content (no
                aria-label — see the removed workaround in issue #60's
                review) rather than an override. */}
            {' '}
            <span className={styles.chipCount}>
              {c.key === 'all' ? allCount : counts[c.key]}
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
        <table className={`${table.table} ${styles.jobsTable}`}>
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
              <th className={`${table.th} ${styles.right}`}>{t.columns.speed}</th>
              <th className={`${table.th} ${styles.right}`}>{t.columns.eta}</th>
              <th className={`${table.th} ${styles.center}`}>{t.columns.retries}</th>
              <th className={`${table.th} ${styles.center}`} aria-hidden />
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
                const expansionId = `job-expansion-${j.id}`;
                // Only status: 'active' jobs are actually mid-transfer with a
                // live queue slot — a stalled/failed job can still carry a
                // stale queuePosition from its last attempt, and must keep
                // showing its real status instead of "In peer's queue".
                const inQueue = j.status === 'active' && (j.queuePosition ?? 0) > 0;
                const pct = percent(j.bytesDone, j.bytesTotal);
                return (
                  <Fragment key={j.id}>
                    <tr
                      className={table.rowClickable}
                      onClick={() => toggleExpanded(j.id)}
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
                              className={`${styles.progressFill} ${
                                inQueue ? styles.progressHatched : fillClass(j)
                              }`}
                              style={{ width: `${inQueue ? 100 : Math.max(2, pct)}%` }}
                            />
                          </div>
                          <span className={`${table.mono} ${styles.pct} ${pctClass(j, inQueue, pct)}`}>
                            {j.status === 'queued' ? '—' : inQueue ? t.jobs.queuePosition(j.queuePosition!) : `${pct}%`}
                          </span>
                        </div>
                        <div className={styles.progressSub}>
                          {inQueue
                            ? t.jobs.queuedAtPeer
                            : j.state === 'IMPORTING'
                              ? t.jobs.verifying
                              : stateLabel(j.state, j.status)}
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
                        <button
                          type="button"
                          className={styles.chevronButton}
                          aria-expanded={expanded}
                          aria-controls={expansionId}
                          aria-label={expanded ? t.jobs.hideDetails : t.jobs.showDetails}
                        >
                          <span
                            aria-hidden
                            className={`${styles.chevron} ${expanded ? styles.chevronExpanded : ''}`}
                          >
                            ›
                          </span>
                        </button>
                      </td>
                    </tr>
                    {expanded && (
                      <tr id={expansionId}>
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
