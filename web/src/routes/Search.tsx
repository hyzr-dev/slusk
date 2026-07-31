import type { FormEvent } from 'react';
import { useEffect, useMemo, useState } from 'react';
import { ApiError } from '../api/client';
import { useCreateJob, useSearchSession, useStartSearch, useStopSearch } from '../api/queries';
import { useSearchStream } from '../api/stream';
import type { SearchFile, SearchGroup } from '../api/types';
import Chip from '../components/tui/Chip';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import { t } from '../strings';
import SearchResultCard, { type CardStatus } from './SearchResultCard';
import styles from './Search.module.css';

type SortKey = 'best' | 'size' | 'speed' | 'avail';

const SORT_ORDER: SortKey[] = ['best', 'size', 'speed', 'avail'];

/**
 * Sorted copy of `groups` — never mutates its argument, since it's read
 * straight from the React Query cache. 'best' is the server's own `score`
 * (issue #58 §4, computed in Go from the exported matcher primitives); the
 * other three are client-side only, over whatever page of results has
 * arrived so far.
 */
export function sortGroups(groups: SearchGroup[], sort: SortKey): SearchGroup[] {
  const copy = [...groups];
  switch (sort) {
    case 'size':
      copy.sort((a, b) => b.sizeBytes - a.sizeBytes);
      break;
    case 'speed':
      copy.sort((a, b) => b.uploadSpeed - a.uploadSpeed);
      break;
    case 'avail':
      copy.sort((a, b) => {
        if (a.freeUploadSlot !== b.freeUploadSlot) return a.freeUploadSlot ? -1 : 1;
        return a.queueLength - b.queueLength;
      });
      break;
    default:
      copy.sort((a, b) => b.score - a.score);
  }
  return copy;
}

/**
 * The distinct formats present in `groups`, sorted alphabetically — the
 * format chip row's source of truth. Exported as one helper (issue #58 §7)
 * so issue #129's filter-syntax box lifts this derivation rather than
 * forking a second copy of it.
 */
export function deriveFormats(groups: SearchGroup[]): string[] {
  const set = new Set<string>();
  for (const group of groups) {
    if (group.format) set.add(group.format);
  }
  return [...set].sort();
}

function filterByFormat(groups: SearchGroup[], activeFormats: Set<string>): SearchGroup[] {
  if (activeFormats.size === 0) return groups;
  return groups.filter((group) => group.format !== undefined && activeFormats.has(group.format));
}

// Builds the default file selection for a newly-seen group: every file,
// matching "Download selected" reading as "Download album" until the user
// deliberately unchecks something in the expansion.
function allFilenames(group: SearchGroup): Set<string> {
  return new Set(group.files.map((f) => f.filename));
}

function messageForStartError(err: unknown): string {
  if (err instanceof ApiError && err.status === 503) return t.search.busyRetry;
  return t.search.startFailed;
}

function messageForDownloadError(err: unknown): string {
  if (err instanceof ApiError && err.status === 409) return t.search.busyNotice;
  return t.search.downloadFailed;
}

export default function Search() {
  const [query, setQuery] = useState('');
  const [submittedQuery, setSubmittedQuery] = useState('');
  const [searchId, setSearchId] = useState<string | undefined>(undefined);
  const [startError, setStartError] = useState<string | undefined>(undefined);
  const [sort, setSort] = useState<SortKey>('best');
  const [activeFormats, setActiveFormats] = useState<Set<string>>(new Set());
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [selections, setSelections] = useState<Map<string, Set<string>>>(new Map());
  const [cardStatus, setCardStatus] = useState<Map<string, CardStatus>>(new Map());

  const startSearch = useStartSearch();
  const stopSearch = useStopSearch();
  const createJob = useCreateJob();
  const sessionQuery = useSearchSession(searchId);
  const session = sessionQuery.data;
  const phase = queryPhase(sessionQuery);

  // Rides the one shared SSE connection scoped to this search (issue #58) —
  // a no-op while searchId is undefined (the idle state).
  useSearchStream(searchId);

  const groups = session?.groups ?? [];

  // A group's files never change once it exists (a group is resent whole
  // only when a file within it changes — see WireSearchGroup's doc comment —
  // and this only adds the ones this component hasn't seen a selection for
  // yet, so an in-progress user edit is never clobbered by the next poll or
  // stream frame.
  useEffect(() => {
    setSelections((prev) => {
      let changed = false;
      const next = new Map(prev);
      for (const group of groups) {
        if (!next.has(group.id)) {
          next.set(group.id, allFilenames(group));
          changed = true;
        }
      }
      return changed ? next : prev;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [groups]);

  const formats = useMemo(() => deriveFormats(groups), [groups]);
  const visibleGroups = useMemo(
    () => sortGroups(filterByFormat(groups, activeFormats), sort),
    [groups, sort, activeFormats],
  );

  function runSearch(raw: string) {
    const trimmed = raw.trim();
    if (!trimmed) return;
    if (searchId && session && !session.done) {
      stopSearch.mutate(searchId);
    }
    setSubmittedQuery(trimmed);
    setStartError(undefined);
    setExpandedId(null);
    setSelections(new Map());
    setCardStatus(new Map());
    setActiveFormats(new Set());
    startSearch.mutate(trimmed, {
      onSuccess: (started) => setSearchId(started.id),
      onError: (err) => setStartError(messageForStartError(err)),
    });
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault();
    runSearch(query);
  }

  function toggleFile(groupId: string, filename: string) {
    setSelections((prev) => {
      const next = new Map(prev);
      const set = new Set(next.get(groupId) ?? []);
      if (set.has(filename)) set.delete(filename);
      else set.add(filename);
      next.set(groupId, set);
      return next;
    });
  }

  function toggleFormat(format: string) {
    setActiveFormats((prev) => {
      const next = new Set(prev);
      if (next.has(format)) next.delete(format);
      else next.add(format);
      return next;
    });
  }

  function download(group: SearchGroup, files: SearchFile[]) {
    if (files.length === 0) return;
    createJob.mutate(
      {
        title: group.title,
        artist: group.parent,
        peer: group.peer,
        files: files.map((f) => ({ filename: f.filename, size: f.size })),
      },
      {
        onSuccess: () => {
          setCardStatus((prev) => new Map(prev).set(group.id, { queued: true }));
        },
        onError: (err) => {
          setCardStatus((prev) => new Map(prev).set(group.id, { queued: false, error: messageForDownloadError(err) }));
        },
      },
    );
  }

  const idle = searchId === undefined;
  const noHits = !idle && hasData(phase) && !!session?.done && groups.length === 0;
  const showResults = !idle && groups.length > 0;
  const searching = !idle && !session?.done;

  return (
    <Page title={t.page.search.title} subtitle={t.page.search.subtitle}>
      <Panel>
        <form className={styles.searchBar} onSubmit={handleSubmit}>
          <div className={styles.inputWrap}>
            <input
              className={styles.input}
              type="text"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={t.search.queryPlaceholder}
            />
            {searching && <span aria-hidden className={styles.spinner} />}
          </div>
          <button type="submit" className={styles.submit}>{t.search.submit}</button>
        </form>

        {startError && <div className={styles.startError} role="alert">{startError}</div>}

        {idle && (
          <div className={styles.idle}>
            <div className={styles.idleTitle}>{t.search.idleTitle}</div>
            <div className={styles.idleBody}>{t.search.idleBody}</div>
            <div className={styles.examples}>
              {t.search.examples.map((example) => (
                <button key={example} type="button" className={styles.exampleChip} onClick={() => { setQuery(example); runSearch(example); }}>
                  {example}
                </button>
              ))}
            </div>
          </div>
        )}

        {!idle && (
          <>
            <QueryNotice phase={phase} />

            {noHits && (
              <div className={styles.noHits}>
                <div className={styles.idleTitle}>{t.search.noHitsTitle(submittedQuery)}</div>
                <div className={styles.idleBody}>{t.search.noHitsBody}</div>
                <button
                  type="button"
                  className={styles.newSearchButton}
                  onClick={() => { setSearchId(undefined); setQuery(''); }}
                >
                  {t.search.newSearch}
                </button>
              </div>
            )}

            {showResults && session && (
              <>
                <div className={styles.resultsHead}>
                  <div className={styles.resultsCount}>
                    <span className={styles.resultsCountNumber}>{t.search.resultsCount(session.total)}</span>
                    {!session.done && session.streaming && <span className={styles.streamingNote}> · {t.search.streamingSuffix}</span>}
                    {session.done && <span className={styles.doneNote}> · {t.search.completeSuffix}</span>}
                    {session.truncated && <span className={styles.doneNote}> · {t.search.truncatedSuffix}</span>}
                  </div>
                  <div className={styles.controls}>
                    <span className={styles.sortLabel}>{t.search.sortLabel}</span>
                    <select
                      className={styles.sortSelect}
                      value={sort}
                      onChange={(event) => setSort(event.target.value as SortKey)}
                    >
                      {SORT_ORDER.map((key) => (
                        <option key={key} value={key}>{t.search.sortOptions[key]}</option>
                      ))}
                    </select>
                    {formats.map((format) => (
                      <Chip
                        key={format}
                        label={format.toUpperCase()}
                        active={activeFormats.has(format)}
                        onClick={() => toggleFormat(format)}
                      />
                    ))}
                  </div>
                </div>

                <ul className={styles.list}>
                  {visibleGroups.map((group, index) => (
                    <SearchResultCard
                      key={group.id}
                      group={group}
                      best={sort === 'best' && index === 0 && session.total > 1}
                      expanded={expandedId === group.id}
                      onToggleExpand={() => setExpandedId((prev) => (prev === group.id ? null : group.id))}
                      selectedFiles={selections.get(group.id) ?? allFilenames(group)}
                      onToggleFile={(filename) => toggleFile(group.id, filename)}
                      status={cardStatus.get(group.id)}
                      onDownloadAlbum={() => download(group, group.files)}
                      onDownloadSelected={() => {
                        const selected = selections.get(group.id) ?? allFilenames(group);
                        download(group, group.files.filter((f) => selected.has(f.filename)));
                      }}
                    />
                  ))}
                </ul>

                {visibleGroups.length === 0 && <EmptyState message={t.search.noFormatMatch} />}

                {searching && (
                  <div className={styles.askingPeers}>
                    <span aria-hidden className={styles.spinner} />
                    {t.search.askingPeers}
                  </div>
                )}
              </>
            )}
          </>
        )}
      </Panel>
    </Page>
  );
}
