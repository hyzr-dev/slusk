import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useEvents } from '../api/queries';
import EmptyState from '../components/tui/EmptyState';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import { formatShortTime } from '../format';
import { eventLabel, t } from '../strings';
import { matchesFilter } from './eventFilter';
import styles from './Events.module.css';

export default function Events() {
  const eventsQuery = useEvents();
  const events = eventsQuery.data ?? [];
  const phase = queryPhase(eventsQuery);
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

      <div className={`${styles.grid} ${styles.head}`}>
        <span>{t.columns.time}</span>
        <span>{t.columns.job}</span>
        <span>{t.columns.event}</span>
        <span>{t.columns.detail}</span>
      </div>

      <QueryNotice phase={phase} />
      {hasData(phase) &&
        (filtered.length === 0 ? (
          <EmptyState message={t.events.empty} />
        ) : (
          filtered.map((e) => (
            <div key={e.id} className={`${styles.grid} ${styles.row}`}>
              <span className={styles.mono}>{formatShortTime(e.createdAt)}</span>
              <Link to={`/jobs/${e.jobId}`} className={`${styles.mono} ${styles.link}`}>
                #{e.jobId}
              </Link>
              <span>{eventLabel(e.event)}</span>
              <span className={styles.detail}>{e.detail}</span>
            </div>
          ))
        ))}
    </>
  );
}
