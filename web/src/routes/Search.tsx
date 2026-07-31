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
      onError: (err) => {
        setStartError(messageForStartError(err));
        // Drop the PREVIOUS search's id too. Without this, `starting` goes
        // false while `searchId` still points at the search before this one,
        // so its cards, its result count and its header all come back looking
        // like the answer to a query that never ran — and `noHits` would name
        // that query in `noHitsTitle(submittedQuery)`. Clearing it leaves the
        // idle prompt under the error, which is what actually happened.
        setSearchId(undefined);
      },
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
        // Best-effort only: `parent` is the peer's parent DIRECTORY name, not
        // a resolved artist (the Soulseek wire carries no artist field — issue
        // #58 §4), so a peer sharing /Music/Various Artists/<album>/ makes the
        // job's artist "Various Artists". Posted anyway because `artist` is
        // what POST /api/jobs takes and a folder name is the best guess
        // available here; issue #59 owns actually matching a manual download
        // against Lidarr. The card labels it as a folder — see
        // SearchResultCard's .parent — so the UI never claims otherwise.
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

  // POST /api/search is in flight. `searchId` is still the PREVIOUS search's
  // id for the whole of that window (it only advances in the 201's onSuccess),
  // so every state below has to exclude it explicitly: without that, clicking
  // Search leaves the previous search's header, count and cards on screen,
  // looking current, with nothing but an aria-hidden spinner to say otherwise.
  const starting = startSearch.isPending;
  const hasSession = !starting && searchId !== undefined;
  const idle = searchId === undefined && !starting;
  const noHits = hasSession && hasData(phase) && !!session?.done && groups.length === 0;
  const showResults = hasSession && groups.length > 0;
  // Deliberately not gated on having results: a search with nothing back yet
  // is exactly the state that most needs to say it is working. Rendered
  // outside the `showResults` block below for the same reason.
  //
  // The 'error' phase is excluded rather than relying on `!session?.done`:
  // when the GET fails with nothing cached, `session` is undefined, so
  // `!session?.done` reads as "still running" and the view renders
  // QueryNotice's failure AND "Asking peers on the network…" at once. Nothing
  // ever clears that pairing either — refetchInterval returns false with no
  // data, so no further request is made and the state is terminal. 'stale'
  // deliberately stays in: there the poll IS still armed and the session may
  // genuinely still be running.
  const searching = starting || (hasSession && phase !== 'error' && !session?.done);

  return (
    <Page title={t.page.search.title} subtitle={t.page.search.subtitle}>
      <Panel>
        <form className={styles.searchBar} onSubmit={handleSubmit}>
          <div className={styles.inputWrap}>
            {/* A real label, not just the placeholder: the placeholder-derived
                accessible name vanishes the moment the user types. Visually
                hidden because the design gives the field no room for one. */}
            <label className={styles.srOnly} htmlFor="search-query">{t.search.queryLabel}</label>
            <input
              id="search-query"
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
            {/* Suppressed while a start is in flight: the query is either
                disabled (first search — phase 'loading' with nothing actually
                loading) or still holding the previous search's data, and the
                searching block below is the honest signal for that window. */}
            {!starting && <QueryNotice phase={phase} />}

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
                    {session.done && (
                      <span className={styles.doneNote}> · {session.expired ? t.search.expiredSuffix : t.search.completeSuffix}</span>
                    )}
                    {session.truncated && <span className={styles.doneNote}> · {t.search.truncatedSuffix}</span>}
                  </div>
                  <div className={styles.controls}>
                    {/* <label htmlFor>, not a bare <span>: this is the
                        select's only accessible name, and it doubles as a
                        click target for it. */}
                    <label className={styles.sortLabel} htmlFor="search-sort">{t.search.sortLabel}</label>
                    <select
                      id="search-sort"
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
              </>
            )}

            {/* Outside the `showResults` block on purpose. Nested inside it —
                and so behind `groups.length > 0` — this could only ever appear
                once results had already arrived, i.e. never in the state it
                was written for: a search that is running with nothing back
                yet. It is also the only feedback during `starting`, when the
                previous search's results are deliberately hidden. */}
            {searching && (
              <div className={styles.askingPeers}>
                <span aria-hidden className={styles.spinner} />
                {t.search.askingPeers}
              </div>
            )}
          </>
        )}
      </Panel>
    </Page>
  );
}
