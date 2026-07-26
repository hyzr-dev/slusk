import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useEvents } from '../api/queries';
import EmptyState from '../components/tui/EmptyState';
import { formatShortTime } from '../format';
import { eventLabel, t } from '../strings';
import { matchesFilter } from './eventFilter';
import styles from './Events.module.css';

export default function Events() {
  const { data: events = [] } = useEvents();
  const [filter, setFilter] = useState('');

  const filtered = events.filter((e) => matchesFilter(e, filter));

  return (
    <>
      <div className={styles.controlsRow}>
        <div className={styles.filterBox}>
          <span aria-hidden className={styles.filterSlash}>/</span>
          <input
            className={styles.filterInput}
            type="text"
            placeholder={t.events.filterPlaceholder}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
        </div>
      </div>

      <div role="table">
        <div role="row" className={`${styles.grid} ${styles.head}`}>
          <span role="columnheader">{t.columns.time}</span>
          <span role="columnheader">{t.columns.job}</span>
          <span role="columnheader">{t.columns.event}</span>
          <span role="columnheader">{t.columns.detail}</span>
        </div>

        {filtered.map((e) => (
          <div key={e.id} role="row" className={`${styles.grid} ${styles.row}`}>
            <span role="cell" className={styles.mono}>{formatShortTime(e.createdAt)}</span>
            {/* role="cell" goes on the span, not the <a>, so the link keeps its own link role */}
            <span role="cell" className={styles.mono}>
              <Link to={`/jobs/${e.jobId}`} className={styles.link}>
                #{e.jobId}
              </Link>
            </span>
            <span role="cell">{eventLabel(e.event)}</span>
            <span role="cell" className={styles.detail}>{e.detail}</span>
          </div>
        ))}
      </div>

      {filtered.length === 0 && <EmptyState message={t.events.empty} />}
    </>
  );
}
