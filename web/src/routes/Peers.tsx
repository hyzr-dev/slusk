import { Fragment, useState } from 'react';
import {
  DEFAULT_PEER_PAGE_PARAMS,
  PEERS_PAGE_SIZE,
  usePeerHistory,
  usePeers,
} from '../api/queries';
import type { Peer, PeerPageParams, PeerPageSort } from '../api/types';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Pager from '../components/tui/Pager';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import { formatScore, formatShortTime } from '../format';
import { t } from '../strings';
import styles from './Peers.module.css';

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
  const [params, setParams] = useState<PeerPageParams>(DEFAULT_PEER_PAGE_PARAMS);
  const peersQuery = usePeers(params);
  const peers = peersQuery.data?.peers ?? [];
  const total = peersQuery.data?.total ?? 0;
  const phase = queryPhase(peersQuery);
  const [expanded, setExpanded] = useState<string | null>(null);

  const totalPages = Math.max(1, Math.ceil(total / PEERS_PAGE_SIZE));
  const start = total === 0 ? 0 : params.page * PEERS_PAGE_SIZE + 1;
  const end = Math.min(total, (params.page + 1) * PEERS_PAGE_SIZE);

  // Clicking the active column toggles direction; a new column always starts
  // descending. Both reset to page 0: the row that was at offset 25 under the
  // old order has nothing to do with the row at offset 25 under the new one,
  // so staying on page 2 would land the user somewhere arbitrary.
  function sortBy(key: PeerPageSort) {
    setParams((prev) => ({
      page: 0,
      sort: key,
      dir: prev.sort === key && prev.dir === 'desc' ? 'asc' : 'desc',
    }));
  }

  function goToPage(page: number) {
    setParams((prev) => ({ ...prev, page }));
  }

  function toggle(username: string) {
    setExpanded((prev) => (prev === username ? null : username));
  }

  const sortHead = (key: PeerPageSort, label: string, className?: string) => {
    const dir = params.sort === key ? (params.dir === 'desc' ? 'descending' : 'ascending') : 'none';
    return (
      <span role="columnheader" aria-sort={dir} className={className}>
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
          {sortHead('username', t.peers.gridHead.peer)}
          {sortHead('score', t.peers.gridHead.score)}
          {sortHead('successCount', t.peers.gridHead.ok)}
          {sortHead('failCount', t.peers.gridHead.fail)}
          {/* Not sortable, and deliberately so: LAST SEEN is derived here from
              whichever of two independent timestamps is newer, and the backend
              has no key that ranks by it. A header that sorted only the
              fetched page would claim an order over the set it does not have. */}
          <span role="columnheader" className={styles.headRight}>{t.peers.gridHead.lastSeen}</span>
        </div>

        {hasData(phase) && peers.map((p) => {
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
      {/* An empty page and an empty list are different facts. Saying "no peers
          recorded yet" while the pager beside it reads "of 97 peers" would be
          the interface contradicting itself — see interface-must-not-invent-data. */}
      {hasData(phase) && peers.length === 0 && (
        <EmptyState message={total === 0 ? t.peers.empty : t.peers.pastTheEnd} />
      )}

      {hasData(phase) && (
        <nav className={styles.pagination} aria-label={t.peers.paginationLabel}>
          <span className={styles.resultRange}>{t.peers.resultRange(start, end, total)}</span>
          <Pager page={params.page} totalPages={totalPages} onChange={goToPage} />
        </nav>
      )}
      </Panel>
    </Page>
  );
}
