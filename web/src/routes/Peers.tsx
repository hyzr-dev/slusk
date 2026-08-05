import { Fragment, useState } from 'react';
import { usePeerHistory, usePeers } from '../api/queries';
import type { Peer } from '../api/types';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import { formatScore, formatShortTime } from '../format';
import { t } from '../strings';
import styles from './Peers.module.css';

type SortKey = 'score' | 'successCount' | 'failCount';

// lastSuccessAt and lastFailAt are recorded independently (each only touched
// by its own outcome), so neither alone tells you when a peer was last seen
// at all — take whichever is more recent. Empty strings (never happened)
// sort before any real timestamp lexicographically, which is what we want.
function lastSeenAt(p: Peer): string {
  return p.lastSuccessAt > p.lastFailAt ? p.lastSuccessAt : p.lastFailAt;
}

/**
 * The body of one expanded peer row.
 *
 * Its own component so the fetch is mounted with the expansion and unmounted
 * with it — usePeerHistory is `enabled` on a username, and rendering this
 * conditionally is what scopes the request to the one open row (issue #424).
 * Before that split the artist rows arrived with the list and this was a plain
 * read of data already in hand; it is a network call now, so it carries the
 * loading and failure states that come with one.
 */
function PeerExpansion({ username }: { username: string }) {
  const historyQuery = usePeerHistory(username);

  if (historyQuery.isPending) {
    return <div className={styles.artist}>{t.peers.artistHistoryLoading}</div>;
  }
  // A failure is reported even when a previous response is still cached: a
  // stale artist history looks identical to a fresh one, so silently showing
  // it would be the interface inventing certainty it does not have.
  if (historyQuery.isError) {
    return <div className={styles.artist}>{t.peers.artistHistoryFailed}</div>;
  }
  const artists = historyQuery.data?.artists ?? [];
  if (artists.length === 0) {
    return <div className={styles.artist}>{t.peers.noArtistHistory}</div>;
  }
  return (
    <>
      {artists.map((a) => (
        <div key={a.artistId} className={styles.artist}>
          {t.peers.artistLine(
            t.peers.artistLabel(a.artistId, a.artistName),
            formatScore(a.score),
            a.successCount,
            a.failCount,
          )}
        </div>
      ))}
    </>
  );
}

export default function Peers() {
  const peersQuery = usePeers();
  const peers = peersQuery.data ?? [];
  const phase = queryPhase(peersQuery);
  const [sortKey, setSortKey] = useState<SortKey>('score');
  const [desc, setDesc] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);

  // Clicking the active column toggles direction; a new column always starts
  // descending. Sort is non-mutating so the API order breaks ties stably.
  function sortBy(key: SortKey) {
    if (key === sortKey) {
      setDesc((d) => !d);
    } else {
      setSortKey(key);
      setDesc(true);
    }
  }

  const sorted = [...peers].sort((a, b) => {
    const d = a[sortKey] - b[sortKey];
    return desc ? -d : d;
  });

  function toggle(username: string) {
    setExpanded((prev) => (prev === username ? null : username));
  }

  const sortHead = (key: SortKey, label: string) => {
    const dir = sortKey === key ? (desc ? 'descending' : 'ascending') : 'none';
    return (
      <span role="columnheader" aria-sort={dir}>
        <button type="button" className={styles.sortHead} onClick={() => sortBy(key)}>
          {label}
        </button>
      </span>
    );
  };

  return (
    <Page title={t.page.peers.title} subtitle={t.page.peers.subtitle}>
      <Panel>
      <div role="table">
        <div role="row" className={`${styles.grid} ${styles.head}`}>
          <span role="columnheader">{t.peers.gridHead.peer}</span>
          {sortHead('score', t.peers.gridHead.score)}
          {sortHead('successCount', t.peers.gridHead.ok)}
          {sortHead('failCount', t.peers.gridHead.fail)}
          <span role="columnheader" className={styles.headRight}>{t.peers.gridHead.lastSeen}</span>
        </div>

        {hasData(phase) && sorted.map((p) => {
          const isExpanded = expanded === p.username;
          const expansionId = `peer-expansion-${p.username}`;

          return (
            // A keyed Fragment is required here: the shorthand <> cannot take
            // a key, and each peer renders a row plus its (conditional)
            // expansion as siblings.
            <Fragment key={p.username}>
              <div
                role="row"
                className={`${styles.grid} ${styles.row} ${isExpanded ? styles.rowExpanded : ''}`}
                onClick={() => toggle(p.username)}
              >
                <div role="cell" className={styles.peerCell}>
                  <button
                    type="button"
                    className={styles.caretButton}
                    onClick={(e) => {
                      // Without stopPropagation the click also reaches the
                      // row handler above and toggles a second time.
                      e.stopPropagation();
                      toggle(p.username);
                    }}
                    aria-expanded={isExpanded}
                    aria-controls={expansionId}
                    aria-label={isExpanded ? t.jobs.hideDetails : t.jobs.showDetails}
                  >
                    <span aria-hidden className={styles.caret}>{isExpanded ? '▾' : '▸'}</span>
                  </button>
                  <span className={styles.username}>{p.username}</span>
                </div>
                <span role="cell" className={styles.mono}>{formatScore(p.score)}</span>
                <span role="cell" className={styles.mono}>{p.successCount}</span>
                <span role="cell" className={styles.mono}>{p.failCount}</span>
                <span role="cell" className={`${styles.mono} ${styles.right}`}>
                  {formatShortTime(lastSeenAt(p))}
                </span>
              </div>
              {isExpanded && (
                <div id={expansionId} role="row" className={styles.expansionWrap}>
                  <div role="cell" aria-colspan={5}>
                    <PeerExpansion username={p.username} />
                  </div>
                </div>
              )}
            </Fragment>
          );
        })}
      </div>

      {/* Both of these sit outside the table: `role="table"` admits only rows,
          so a notice or an empty state nested inside would be invalid ARIA. */}
      <QueryNotice phase={phase} />
      {hasData(phase) && sorted.length === 0 && <EmptyState message={t.peers.empty} />}
      </Panel>
    </Page>
  );
}
