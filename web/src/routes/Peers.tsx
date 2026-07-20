import { Fragment, useState } from 'react';
import { usePeers } from '../api/queries';
import type { Peer } from '../api/types';
import PageHeading from '../components/PageHeading';
import table from '../components/Table.module.css';
import { formatScore } from '../format';
import { t } from '../strings';
import styles from './Peers.module.css';

type SortKey = 'score' | 'successCount' | 'failCount';

export default function Peers() {
  const { data: peers = [] } = usePeers();
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

  const header = (key: SortKey, label: string) => (
    <th className={`${table.th} ${table.thSortable}`} onClick={() => sortBy(key)}>
      {label}
    </th>
  );

  return (
    <>
      <PageHeading>{t.nav.peers}</PageHeading>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.peer}</th>
            {header('score', t.columns.score)}
            {header('successCount', t.columns.succeeded)}
            {header('failCount', t.columns.failed)}
          </tr>
        </thead>
        <tbody>
          {sorted.length === 0 ? (
            <tr><td className={table.empty} colSpan={4}>{t.peers.empty}</td></tr>
          ) : (
            sorted.map((p: Peer) => (
              // A keyed Fragment is required here: the shorthand <> cannot take
              // a key, and each peer renders two sibling rows.
              <Fragment key={p.username}>
                <tr
                  className={table.rowClickable}
                  onClick={() => setExpanded(expanded === p.username ? null : p.username)}
                >
                  <td className={table.td}>{p.username}</td>
                  <td className={`${table.td} ${table.mono}`}>{formatScore(p.score)}</td>
                  <td className={`${table.td} ${table.mono}`}>{p.successCount}</td>
                  <td className={`${table.td} ${table.mono}`}>{p.failCount}</td>
                </tr>
                {expanded === p.username && (
                  <tr className={styles.detailRow}>
                    <td className={table.td} colSpan={4}>
                      {p.artists.length === 0 ? (
                        <div className={styles.artist}>{t.peers.noArtistHistory}</div>
                      ) : (
                        p.artists.map((a) => (
                          <div key={a.artistId} className={styles.artist}>
                            {t.peers.artistLine(
                              a.artistId,
                              formatScore(a.score),
                              a.successCount,
                              a.failCount,
                            )}
                          </div>
                        ))
                      )}
                    </td>
                  </tr>
                )}
              </Fragment>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
