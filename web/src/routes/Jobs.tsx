import { Fragment, memo, useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { JOBS_PAGE_SIZE, useBulkRetryJobs, useJobs } from '../api/queries';
import { useJobScope } from '../api/stream';
import type {
  Job,
  JobPageDirection,
  JobPageSort,
  JobSourceFilter,
  JobStatusFilter,
} from '../api/types';
import { useFlash } from '../components/chrome/FlashContext';
import Button from '../components/tui/Button';
import Chip from '../components/tui/Chip';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Pager from '../components/tui/Pager';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import Tag from '../components/tui/Tag';
import Ticks, { type TickTone } from '../components/tui/Ticks';
import { formatEta, formatSpeed, percent } from '../format';
import { t } from '../strings';
import BulkRetryDialog from './BulkRetryDialog';
import JobExpansion from './JobExpansion';
import styles from './Jobs.module.css';

// The two filters a bulk retry is offered for (issue #378). Both are status
// filters whose set overlaps the retryable states; every other filter would
// send the server a scope it must refuse in full. #416's three new statuses
// are none of them: WANTED/SELECTING/WAITING describe a job that has not
// failed, so there is nothing to retry.
type BulkRetryFilter = 'failed' | 'parked';

function bulkRetryFilter(status: JobStatusFilter): BulkRetryFilter | null {
  return status === 'failed' || status === 'parked' ? status : null;
}

// The approved status row (issue #416 adds WANTED/SELECTING/WAITING to the
// original seven, ten in total). IMPORTING, INFLIGHT, FINISHED and FAILURES
// (issues #287, #310) are all server-only filter values used by Overview's
// own useJobs calls, not chips a user picks here — JobStatusFacets has no
// count for any of them, and IMPORTING is otherwise represented under ALL by
// its IM tag. Ordered to mirror the backend's `sort=st` ranking.
type ChipKey = Exclude<JobStatusFilter, 'importing' | 'inflight' | 'finished' | 'failures'>;
const CHIP_ORDER: ChipKey[] = ['all', 'active', 'waiting', 'queued', 'selecting', 'wanted', 'stalled', 'failed', 'parked', 'done'];

// A second, orthogonal axis of chips (Manual vs Lidarr-sourced jobs). The
// mock doesn't draw this control — its designer was working against a data
// model that predates the source axis, though the mock does know the concept
// (the small "●" dot on manual rows). Source filtering is a shipped feature,
// so it stays in the same TUI chip idiom as the status row, separated by a 1px
// rule so it reads as a second axis rather than more status values.
// No ALL chip here, unlike the status axis: 'all' is the absence of a source
// filter, not a third source, and a second chip reading ALL next to the status
// row's own ALL made the same word mean two different things on one screen.
// Clicking the active chip clears the filter instead. Counts are omitted too —
// the source split is a two-way toggle, and the numbers cost width the row does
// not have (see .controlsRow's note in Jobs.module.css).
const SOURCE_CHIP_ORDER: Exclude<JobSourceFilter, 'all'>[] = ['manual', 'lidarr'];

// Tick/percentage colour for a row (mock's `col`, line ~1052). Issue #416
// moved "no bytes moving in a peer queue" onto its own 'queued' status —
// the backend, not a client-derived queuePosition check, is now the single
// source of truth for it (see Tag.tagFor's doc comment on the same change).
// 'waiting' gets the same quiet tone: a gap between two files of the same
// candidate has no bytes moving either, the same story 'queued' tells.
// 'selecting'/'wanted' fall through to the neutral bar instead — neither has
// reached a candidate or transfer yet, so there's no "waiting on bytes"
// story to tell with the queued tone. done is the one unambiguous success
// color, and the three failure-ish statuses share the bad tone.
function rowTone(job: Job): TickTone {
  if (job.status === 'queued' || job.status === 'waiting') return 'queued';
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
  // Both peer-queue statuses: 'queued' before the candidate's first file has
  // arrived, 'waiting' in the gaps after. Nothing is moving in either.
  const queued = job.status === 'queued' || job.status === 'waiting';
  // "Downloading" in the narrow sense used by the SPEED/ETA cells: bytes are
  // actually moving. As of issue #416 this is exactly job.status === 'active'
  // — the backend now reports "active but no bytes moving" as its own
  // 'queued' status rather than overloading 'active' with a queuePosition.
  const downloading = job.status === 'active';
  // A selecting job may still point at a dead candidate's partial bytes
  // (issue #269, and #416's rename of this status from 'queued' to
  // 'selecting'): the same aggregate-status fix that lets a SELECTING job
  // hold a FAILED candidate's leftover AlbumBytesDone/Total also means the
  // tick bar must not render that candidate's progress next to a label ('—')
  // that says nothing is happening. Bar and label read one binding, not two
  // matching expressions — a second, independently editable guard is exactly
  // the kind of drift issue #269 was about. Neither peer-queue status needs
  // this guard: as of #416 both mean a real wait on a live candidate, and
  // 'waiting' in particular has genuine partial bytes to show.
  const noProgress = job.status === 'selecting';
  const pct = noProgress ? 0 : percent(job.bytesDone, job.bytesTotal);
  const tone = rowTone(job);
  // The queue position is shown only when the backend actually supplied one.
  // Before issue #416 a non-null assertion was safe here because 'queued' was
  // derived client-side from queuePosition > 0 and so implied it; the backend's
  // 'queued' is derived from transfer aggregates instead and carries no such
  // guarantee, which would have rendered a literal "Pundefined". Falling back to
  // the percentage keeps the cell on data that exists.
  const pctLabel = noProgress
    ? '—'
    : queued && job.queuePosition
      ? t.jobs.queueShort(job.queuePosition)
      : `${pct}%`;

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
          <Tag status={job.status} bare />
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
  const [bulkRetryOpen, setBulkRetryOpen] = useState(false);
  const bulkRetry = useBulkRetryJobs();
  const flash = useFlash();

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

  const retryableFilter = bulkRetryFilter(status);
  // The chip's own count, reused rather than refetched. For filter=failed it
  // is an upper bound on what can actually be revived — the dialog's copy
  // says so, and the server's response is what gets reported afterwards.
  const retryableCount = retryableFilter ? (result?.facets.status[retryableFilter] ?? 0) : 0;
  // Hidden, not disabled, when the scope is empty: an offer to retry nothing
  // is noise, and this is the one control on the page whose availability is
  // itself information.
  const showBulkRetry = retryableFilter !== null && hasData(phase) && retryableCount > 0;

  function runBulkRetry() {
    bulkRetry.mutate(
      { page, sort, dir: direction, filter: status, source, q: debouncedSearch },
      {
        onSuccess: (r) => {
          setBulkRetryOpen(false);
          setExpandedId(null);
          flash(t.jobs.bulkRetry.resultFlash(r.retried, r.skipped));
        },
      },
    );
  }

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
                active={source === key}
                onClick={() => {
                  setSource(source === key ? 'all' : key);
                  resetPage();
                }}
              />
            ))}
          </div>
          {showBulkRetry && (
            <span className={styles.bulkRetryAction}>
              <Button
                variant="primary"
                disabled={bulkRetry.isPending}
                onClick={() => setBulkRetryOpen(true)}
              >
                {t.jobs.bulkRetry.button}
              </Button>
            </span>
          )}
        </div>

        {/* A failure keeps the dialog open and reports itself inside it: the
            scrim is position:fixed with a z-index, so an error rendered out
            here would be painted behind the dialog the user is still looking
            at — and jsdom, which computes no layout, would never fail on it. */}
        {bulkRetryOpen && retryableFilter && (
          <BulkRetryDialog
            filter={retryableFilter}
            count={retryableCount}
            pending={bulkRetry.isPending}
            failed={bulkRetry.isError}
            onConfirm={runBulkRetry}
            onClose={() => setBulkRetryOpen(false)}
          />
        )}

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

        {/* A single page gets no pager and no range, matching Peers (#427): the
            control would offer only the page already on screen, and `1–7 of 7
            jobs` beside seven visible rows is chrome restating what the table
            already says. The <nav> goes with them rather than staying behind
            empty — a navigation landmark with nothing to navigate is a lie to a
            screen reader. */}
        {hasData(phase) && totalPages > 1 && (
          <nav className={styles.pagination} aria-label={t.jobs.paginationLabel}>
            <span className={styles.resultRange}>{t.jobs.resultRange(start, end, total)}</span>
            <Pager page={page} totalPages={totalPages} onChange={goToPage} />
          </nav>
        )}
      </Panel>
    </Page>
  );
}
