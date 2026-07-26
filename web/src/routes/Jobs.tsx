import { Fragment, memo, useCallback, useState } from 'react';
import { Link } from 'react-router-dom';
import type { Job } from '../api/types';
import { useJobs } from '../api/queries';
import Chip from '../components/tui/Chip';
import EmptyState from '../components/tui/EmptyState';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import Tag from '../components/tui/Tag';
import Ticks, { type TickTone } from '../components/tui/Ticks';
import { formatEta, formatSpeed, percent } from '../format';
import { t } from '../strings';
import { countByStatus, matchesFilters, type SourceFilter, type StatusFilter } from './jobFilter';
import JobExpansion from './JobExpansion';
import styles from './Jobs.module.css';

// The seven filter chips (mock, docs/design/slskdarr-tui.dc.html:1089). No
// "Importing" chip — the mock has no such bucket, and IMPORTING jobs stay
// visible under ALL and carry their own IM tag; jobFilter.ts's separate
// 'importing' StatusFilter value (used by other, not-yet-reskinned views'
// tests) is simply never selected here.
type ChipKey = Exclude<StatusFilter, 'importing'>;
const CHIP_ORDER: ChipKey[] = ['all', 'active', 'queued', 'stalled', 'failed', 'parked', 'done'];

// A second, orthogonal axis of chips (Manual vs Lidarr-sourced jobs). The
// mock doesn't draw this control — its designer was working against a data
// model that predates the source axis, though the mock does know the
// concept (the small "●" dot on manual rows) — jobFilter.ts's SourceFilter
// machinery would otherwise be unreachable from this view, silently
// regressing a shipped feature. Kept in the same TUI chip idiom as the
// status row, just visually separated by a divider so it reads as a second
// axis rather than more status values.
const SOURCE_CHIP_ORDER: SourceFilter[] = ['all', 'manual', 'lidarr'];

// Per-row tick resolution in the jobs grid, matching the mock exactly.
const ROW_TICKS = 26;

// A non-zero queuePosition only means something while the job is still
// 'active' — a stalled/failed job can carry a stale one from its last
// attempt (see Tag.tagFor, which applies the same guard) and must keep
// reading as its real status.
function inPeerQueue(job: Job): boolean {
  return job.status === 'active' && (job.queuePosition ?? 0) > 0;
}

// Tick/percentage colour for a row (mock's `col`, line ~1052): queued beats
// everything else since no bytes move while waiting in a peer's queue, done
// is the one unambiguous success color, and the three failure-ish statuses
// share the bad tone. Everything else (active, downloading) is the neutral
// bar color.
function rowTone(job: Job): TickTone {
  if (inPeerQueue(job)) return 'queued';
  if (job.status === 'done') return 'ok';
  if (job.status === 'stalled' || job.status === 'failed' || job.status === 'parked') return 'bad';
  return 'bar';
}

function toneClass(tone: TickTone): string {
  switch (tone) {
    case 'ok':
      return styles.pctOk;
    case 'bad':
      return styles.pctBad;
    case 'queued':
      return styles.pctQueued;
    default:
      return styles.pctBar;
  }
}

interface JobRowProps {
  job: Job;
  expanded: boolean;
  onToggle: (id: number) => void;
}

function JobRowImpl({ job, expanded, onToggle }: JobRowProps) {
  const expansionId = `job-expansion-${job.id}`;
  const queued = inPeerQueue(job);
  // "Downloading" in the narrow sense used by the SPEED/ETA cells: bytes are
  // actually moving, as opposed to merely being counted 'active' while
  // sitting in a peer's queue.
  const downloading = job.status === 'active' && !queued;
  const pct = percent(job.bytesDone, job.bytesTotal);
  const tone = rowTone(job);
  const pctLabel = job.status === 'queued' ? '—' : queued ? t.jobs.queueShort(job.queuePosition!) : `${pct}%`;

  return (
    <Fragment>
      <div
        role="row"
        className={`${styles.grid} ${styles.row} ${expanded ? styles.rowExpanded : ''}`}
        onClick={() => onToggle(job.id)}
      >
        {/* Tag renders its own inline span, so the cell role needs a wrapper —
            which then becomes the grid item in its place. Harmless for a
            28px column holding two glyphs, but see .albumCell below for the
            case where it is not. */}
        <span role="cell">
          <Tag status={job.status} state={job.state} queuePosition={job.queuePosition} bare />
        </span>
        <div role="cell" className={styles.albumCell}>
          <button
            type="button"
            className={styles.caretButton}
            onClick={(e) => {
              // Without stopPropagation the click also reaches the row
              // handler below and toggles a second time.
              e.stopPropagation();
              onToggle(job.id);
            }}
            aria-expanded={expanded}
            aria-controls={expansionId}
            aria-label={expanded ? t.jobs.hideDetails : t.jobs.showDetails}
          >
            <span aria-hidden className={styles.caret}>{expanded ? '▾' : '▸'}</span>
          </button>
          <Link
            to={`/jobs/${job.id}`}
            className={styles.title}
            onClick={(e) => e.stopPropagation()}
          >
            {job.title}
          </Link>
          <span className={styles.artist}>{job.artist}</span>
          {job.source === 'manual' && (
            <span className={styles.sourceDot} title={t.source.manual}>●</span>
          )}
        </div>
        <span role="cell" className={`${styles.mono} ${styles.peerCell}`}>{job.peer || '—'}</span>
        <span role="cell" className={`${styles.mono} ${styles.formatCell}`}>{job.format ?? '—'}</span>
        <div role="cell" className={styles.progressCell}>
          <div className={styles.ticksWrap}>
            <Ticks percent={pct} count={ROW_TICKS} tone={tone} live={downloading} height={9} />
          </div>
          <span className={`${styles.pct} ${toneClass(tone)}`}>{pctLabel}</span>
        </div>
        <span role="cell" className={`${styles.mono} ${styles.right}`}>{downloading ? formatSpeed(job.speed) : '—'}</span>
        <span role="cell" className={`${styles.mono} ${styles.right}`}>{downloading ? formatEta(job.etaSeconds) : '—'}</span>
        <span role="cell" className={`${styles.right} ${job.retries > 0 ? styles.triesActive : styles.triesDim}`}>
          {job.retries > 0 ? job.retries : '·'}
        </span>
      </div>
      {expanded && (
        <div id={expansionId} role="row" className={styles.expansionWrap}>
          {/* One cell spanning all eight columns — an expansion is a row in
              the table, not a sibling of it, and a row with a single cell in
              an 8-column table has to say so. */}
          <div role="cell" aria-colspan={8}>
            <JobExpansion job={job} onCollapse={() => onToggle(job.id)} />
          </div>
        </div>
      )}
    </Fragment>
  );
}

// Memoised because the jobs list polls every 3s and can hold ~150 rows of 26
// ticks each — a row whose displayed fields haven't changed must not
// re-render. `job` is a fresh object on every poll response even when its
// content is identical, so reference equality is useless here; every field
// this row actually reads is compared instead (mirrors Ticks's own
// fingerprint comparator, one level up).
function jobRowPropsEqual(prev: JobRowProps, next: JobRowProps): boolean {
  if (prev.expanded !== next.expanded || prev.onToggle !== next.onToggle) return false;
  const a = prev.job;
  const b = next.job;
  return (
    a.id === b.id &&
    a.status === b.status &&
    a.state === b.state &&
    a.title === b.title &&
    a.artist === b.artist &&
    a.source === b.source &&
    a.peer === b.peer &&
    a.format === b.format &&
    a.bytesDone === b.bytesDone &&
    a.bytesTotal === b.bytesTotal &&
    a.speed === b.speed &&
    a.etaSeconds === b.etaSeconds &&
    a.retries === b.retries &&
    a.queuePosition === b.queuePosition &&
    a.failReason === b.failReason &&
    a.updatedAt === b.updatedAt
  );
}

const JobRow = memo(JobRowImpl, jobRowPropsEqual);

export default function Jobs() {
  const jobsQuery = useJobs();
  const jobs = jobsQuery.data ?? [];
  const phase = queryPhase(jobsQuery);
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState<StatusFilter>('all');
  const [source, setSource] = useState<SourceFilter>('all');
  const [expandedId, setExpandedId] = useState<number | null>(null);

  const filtered = jobs.filter((j) => matchesFilters(j, search, status, source));
  const counts = countByStatus(jobs, search, source);
  // What the ALL status chip would show if clicked: every job matching the
  // search and source regardless of status, i.e. the sum of every bucket
  // above — not jobs.length, which ignores those two filters and so can
  // disagree with what actually renders when the chip is clicked.
  const allCount = Object.values(counts).reduce((sum, n) => sum + n, 0);

  // Source counts mirror the same "what would this chip show" contract, but
  // jobFilter.ts has no dedicated helper for this axis — reusing
  // matchesFilters directly here is simpler than adding one for three values.
  // Deliberately NOT `allCount`: that sum respects the current *source*
  // filter (it comes from countByStatus(jobs, search, source)) while
  // ignoring *status* — the two axes must count by the same rule, so this
  // recomputes the source-ALL bucket the same way as manual/lidarr below,
  // filtering by search and status but leaving source itself at 'all'.
  const sourceCounts: Record<SourceFilter, number> = {
    all: jobs.filter((j) => matchesFilters(j, search, status, 'all')).length,
    manual: jobs.filter((j) => matchesFilters(j, search, status, 'manual')).length,
    lidarr: jobs.filter((j) => matchesFilters(j, search, status, 'lidarr')).length,
  };

  // Stable across renders (no deps), so JobRow's memo comparator above is
  // never defeated by a fresh function identity on every Jobs render.
  const toggleExpanded = useCallback((id: number) => {
    setExpandedId((prev) => (prev === id ? null : id));
  }, []);

  return (
    <>
      <div className={styles.controlsRow}>
        <div className={styles.filterBox}>
          <span aria-hidden className={styles.filterSlash}>/</span>
          <input
            className={styles.filterInput}
            type="text"
            placeholder={t.jobs.searchPlaceholder}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>
        <div className={styles.chipGroup} role="group" aria-label={t.columns.status}>
          {CHIP_ORDER.map((key) => (
            <Chip
              key={key}
              label={t.jobs.chipLabel[key]}
              count={hasData(phase) ? (key === 'all' ? allCount : counts[key]) : undefined}
              active={status === key}
              onClick={() => setStatus(key)}
            />
          ))}
        </div>
        <span aria-hidden className={styles.chipDivider} />
        <div className={styles.chipGroup} role="group" aria-label={t.jobs.sourceFilterLabel}>
          {SOURCE_CHIP_ORDER.map((key) => (
            <Chip
              key={key}
              label={t.jobs.sourceChipLabel[key]}
              count={hasData(phase) ? sourceCounts[key] : undefined}
              active={source === key}
              onClick={() => setSource(key)}
            />
          ))}
        </div>
      </div>

      <div role="table">
        <div role="row" className={`${styles.grid} ${styles.head}`}>
          <span role="columnheader">{t.jobs.gridHead.status}</span>
          <span role="columnheader">{t.jobs.gridHead.album}</span>
          <span role="columnheader">{t.jobs.gridHead.peer}</span>
          <span role="columnheader">{t.jobs.gridHead.format}</span>
          <span role="columnheader">{t.jobs.gridHead.progress}</span>
          <span role="columnheader" className={styles.headRight}>{t.jobs.gridHead.speed}</span>
          <span role="columnheader" className={styles.headRight}>{t.jobs.gridHead.eta}</span>
          <span role="columnheader" className={styles.headRight}>{t.jobs.gridHead.tries}</span>
        </div>

        {hasData(phase) &&
          filtered.map((j) => (
            <JobRow key={j.id} job={j} expanded={expandedId === j.id} onToggle={toggleExpanded} />
          ))}
      </div>

      {/* Both of these sit outside the table: `role="table"` admits only rows,
          so a notice or an empty state nested inside would be invalid ARIA. */}
      <QueryNotice phase={phase} />
      {hasData(phase) && filtered.length === 0 && <EmptyState message={t.jobs.noMatch} />}
    </>
  );
}
