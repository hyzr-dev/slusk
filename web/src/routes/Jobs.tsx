import { Fragment, memo, useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { JOBS_PAGE_SIZE, useJobs } from '../api/queries';
import { useJobScope } from '../api/stream';
import type {
  Job,
  JobPageDirection,
  JobPageSort,
  JobSourceFilter,
  JobStatusFilter,
} from '../api/types';
import Chip from '../components/tui/Chip';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import Tag from '../components/tui/Tag';
import Ticks, { type TickTone } from '../components/tui/Ticks';
import { formatEta, formatSpeed, percent } from '../format';
import { t } from '../strings';
import JobExpansion from './JobExpansion';
import styles from './Jobs.module.css';

// The approved status row keeps the mock's seven chips. IMPORTING, INFLIGHT,
// FINISHED and FAILURES (issues #287, #310) are all server-only filter values
// used by Overview's own useJobs calls, not chips a user picks here —
// JobStatusFacets has no count for any of them, and IMPORTING is otherwise
// represented under ALL by its IM tag.
type ChipKey = Exclude<JobStatusFilter, 'importing' | 'inflight' | 'finished' | 'failures'>;
const CHIP_ORDER: ChipKey[] = ['all', 'active', 'queued', 'stalled', 'failed', 'parked', 'done'];

// A second, orthogonal axis of chips (Manual vs Lidarr-sourced jobs). The
// mock doesn't draw this control — its designer was working against a data
// model that predates the source axis, though the mock does know the concept
// (the small "●" dot on manual rows). Source filtering is a shipped feature,
// so it stays in the same TUI chip idiom as the status row, visually separated
// by a divider so it reads as a second axis rather than more status values.
const SOURCE_CHIP_ORDER: JobSourceFilter[] = ['all', 'manual', 'lidarr'];

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
  // A queued job may still point at a dead candidate's partial bytes (issue
  // #269): the same aggregate-status fix that lets a SELECTING job hold a
  // FAILED candidate's leftover AlbumBytesDone/Total also means the tick bar
  // must not render that candidate's progress next to a label ('—') that says
  // nothing is happening. Bar and label read one binding, not two matching
  // expressions — a second, independently editable guard is exactly the kind
  // of drift issue #269 was about.
  const noProgress = job.status === 'queued';
  const pct = noProgress ? 0 : percent(job.bytesDone, job.bytesTotal);
  const tone = rowTone(job);
  const pctLabel = noProgress ? '—' : queued ? t.jobs.queueShort(job.queuePosition!) : `${pct}%`;

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
          <Tag status={job.status} queuePosition={job.queuePosition} bare />
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
            <Ticks percent={pct} tone={tone} live={downloading} height={9} />
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

type PageItem = number | 'ellipsis';

// Always exposes the boundaries while keeping the control compact for large
// collections. A one-page neighbourhood around the current page is enough to
// move locally; first/last provide the long jump.
export function paginationItems(page: number, totalPages: number): PageItem[] {
  if (totalPages <= 7) return Array.from({ length: totalPages }, (_, i) => i);
  const pages = [...new Set([0, totalPages - 1, page - 1, page, page + 1])]
    .filter((candidate) => candidate >= 0 && candidate < totalPages)
    .sort((a, b) => a - b);
  const items: PageItem[] = [];
  pages.forEach((candidate, index) => {
    if (index > 0 && candidate - pages[index - 1] > 1) items.push('ellipsis');
    items.push(candidate);
  });
  return items;
}

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return Boolean(target.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])'));
}

interface SortHeaderProps {
  label: string;
  sortKey: JobPageSort;
  activeSort: JobPageSort;
  direction: JobPageDirection;
  onSort: (sort: JobPageSort) => void;
  right?: boolean;
}

function SortHeader({ label, sortKey, activeSort, direction, onSort, right }: SortHeaderProps) {
  const active = activeSort === sortKey;
  return (
    <span
      role="columnheader"
      aria-sort={active ? (direction === 'asc' ? 'ascending' : 'descending') : 'none'}
      className={right ? styles.headRight : undefined}
    >
      <button type="button" className={styles.sortButton} onClick={() => onSort(sortKey)}>
        {label}
        {active && <span aria-hidden className={styles.sortDirection}>{direction === 'asc' ? '↑' : '↓'}</span>}
      </button>
    </span>
  );
}

export default function Jobs() {
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState<JobPageSort>('st');
  const [direction, setDirection] = useState<JobPageDirection>('asc');
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const [status, setStatus] = useState<JobStatusFilter>('all');
  const [source, setSource] = useState<JobSourceFilter>('all');
  const [expandedId, setExpandedId] = useState<number | null>(null);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedSearch(search), 250);
    return () => window.clearTimeout(timer);
  }, [search]);

  const jobsQuery = useJobs({ page, sort, dir: direction, filter: status, source, q: debouncedSearch });
  const result = jobsQuery.data;
  const jobs = result?.jobs ?? [];
  const total = result?.total ?? 0;
  // Scopes the SSE connection to exactly this page's jobs (issue #258) so a
  // stream frame's `jobs` only ever needs to cover what's actually on screen.
  useJobScope(jobs.map((job) => job.id));
  const phase = queryPhase(jobsQuery);
  const totalPages = Math.max(1, Math.ceil(total / JOBS_PAGE_SIZE));

  // A mutation or narrower filter can make the current page cease to exist.
  // Correct it as soon as the new total arrives instead of leaving an empty
  // out-of-range page selected.
  useEffect(() => {
    const lastPage = Math.max(0, Math.ceil(total / JOBS_PAGE_SIZE) - 1);
    if (result && page > lastPage) {
      setPage(lastPage);
      setExpandedId(null);
    }
  }, [page, result, total]);

  const toggleExpanded = useCallback((id: number) => {
    setExpandedId((prev) => (prev === id ? null : id));
  }, []);

  const goToPage = useCallback((nextPage: number) => {
    if (nextPage < 0 || nextPage >= totalPages) return;
    setPage(nextPage);
    setExpandedId(null);
  }, [totalPages]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || isEditableTarget(event.target)) return;
      if (event.key === ',' && page > 0) {
        event.preventDefault();
        goToPage(page - 1);
      } else if (event.key === '.' && page + 1 < totalPages) {
        event.preventDefault();
        goToPage(page + 1);
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [goToPage, page, totalPages]);

  const resetPage = () => {
    setPage(0);
    setExpandedId(null);
  };

  const changeSort = (nextSort: JobPageSort) => {
    if (sort === nextSort) {
      setDirection((current) => (current === 'asc' ? 'desc' : 'asc'));
    } else {
      setSort(nextSort);
      setDirection('asc');
    }
    resetPage();
  };

  const start = total === 0 ? 0 : page * JOBS_PAGE_SIZE + 1;
  const end = Math.min(total, (page + 1) * JOBS_PAGE_SIZE);

  return (
    <Page title={t.page.jobs.title} subtitle={t.page.jobs.subtitle}>
      <Panel>
        <div className={styles.controlsRow}>
          <div className={styles.filterBox}>
            <span aria-hidden className={styles.filterSlash}>/</span>
            <input
              className={styles.filterInput}
              type="text"
              placeholder={t.jobs.searchPlaceholder}
              value={search}
              onChange={(event) => {
                setSearch(event.target.value);
                resetPage();
              }}
            />
          </div>
          <div className={styles.chipGroup} role="group" aria-label={t.columns.status}>
            {CHIP_ORDER.map((key) => (
              <Chip
                key={key}
                label={t.jobs.chipLabel[key]}
                count={hasData(phase) ? result?.facets.status[key] : undefined}
                active={status === key}
                onClick={() => {
                  setStatus(key);
                  resetPage();
                }}
              />
            ))}
          </div>
          <span aria-hidden className={styles.chipDivider} />
          <div className={styles.chipGroup} role="group" aria-label={t.jobs.sourceFilterLabel}>
            {SOURCE_CHIP_ORDER.map((key) => (
              <Chip
                key={key}
                label={t.jobs.sourceChipLabel[key]}
                count={hasData(phase) ? result?.facets.source[key] : undefined}
                active={source === key}
                onClick={() => {
                  setSource(key);
                  resetPage();
                }}
              />
            ))}
          </div>
        </div>

        <div role="table">
          <div role="row" className={`${styles.grid} ${styles.head}`}>
            <SortHeader label={t.jobs.gridHead.status} sortKey="st" activeSort={sort} direction={direction} onSort={changeSort} />
            <SortHeader label={t.jobs.gridHead.album} sortKey="album" activeSort={sort} direction={direction} onSort={changeSort} />
            <SortHeader label={t.jobs.gridHead.peer} sortKey="peer" activeSort={sort} direction={direction} onSort={changeSort} />
            <span role="columnheader">{t.jobs.gridHead.format}</span>
            <span role="columnheader">{t.jobs.gridHead.progress}</span>
            <span role="columnheader" className={styles.headRight}>{t.jobs.gridHead.speed}</span>
            <span role="columnheader" className={styles.headRight}>{t.jobs.gridHead.eta}</span>
            {/* Not sortable, like PROGRESS/SPEED/ETA beside it (mock's plain(),
                docs/design/slusk-tui.dc.html:1174): TRY is a live field, and
                only the three stable columns (ST, ALBUM, PEER) can reorder rows
                without a row jumping mid-poll. */}
            <span role="columnheader" className={styles.headRight}>{t.jobs.gridHead.tries}</span>
          </div>

          {hasData(phase) && jobs.map((job) => (
            <JobRow key={job.id} job={job} expanded={expandedId === job.id} onToggle={toggleExpanded} />
          ))}
        </div>

        {/* These sit outside the table because `role="table"` admits only rows. */}
        <QueryNotice phase={phase} />
        {hasData(phase) && jobs.length === 0 && <EmptyState message={t.jobs.noMatch} />}

        {hasData(phase) && (
          <nav className={styles.pagination} aria-label={t.jobs.paginationLabel}>
            <span className={styles.resultRange}>{t.jobs.resultRange(start, end, total)}</span>
            <div className={styles.pageButtons}>
              <button type="button" className={styles.pageButton} disabled={page === 0} onClick={() => goToPage(page - 1)}>
                {t.jobs.previousPage}
              </button>
              {paginationItems(page, totalPages).map((item, index) =>
                item === 'ellipsis' ? (
                  <span key={`ellipsis-${index}`} className={styles.ellipsis} aria-hidden>…</span>
                ) : (
                  <button
                    type="button"
                    key={item}
                    className={`${styles.pageButton} ${item === page ? styles.pageButtonActive : ''}`}
                    aria-current={item === page ? 'page' : undefined}
                    aria-label={t.jobs.pageLabel(item + 1)}
                    onClick={() => goToPage(item)}
                  >
                    {item + 1}
                  </button>
                ),
              )}
              <button type="button" className={styles.pageButton} disabled={page + 1 >= totalPages} onClick={() => goToPage(page + 1)}>
                {t.jobs.nextPage}
              </button>
            </div>
          </nav>
        )}
      </Panel>
    </Page>
  );
}
