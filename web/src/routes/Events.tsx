import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useEvents } from '../api/queries';
import table from '../components/Table.module.css';
import { formatDateTime } from '../format';
import { eventLabel, t } from '../strings';
import { matchesFilter } from './eventFilter';
import styles from './Jobs.module.css';

export default function Events() {
  const { data: events = [] } = useEvents();
  const [filter, setFilter] = useState('');

  const filtered = events.filter((e) => matchesFilter(e, filter));

  return (
    <>
      <div className={styles.controls}>
        <input
          className={styles.input}
          type="text"
          placeholder={t.events.filterPlaceholder}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.time}</th>
            <th className={table.th}>{t.columns.job}</th>
            <th className={table.th}>{t.columns.event}</th>
            <th className={table.th}>{t.columns.detail}</th>
          </tr>
        </thead>
        <tbody>
          {filtered.length === 0 ? (
            <tr><td className={table.empty} colSpan={4}>{t.events.empty}</td></tr>
          ) : (
            filtered.map((e) => (
              <tr key={e.id}>
                <td className={`${table.td} ${table.mono}`}>{formatDateTime(e.createdAt)}</td>
                <td className={`${table.td} ${table.mono}`}>
                  <Link to={`/jobs/${e.jobId}`} className={styles.link}>#{e.jobId}</Link>
                </td>
                <td className={table.td}>{eventLabel(e.event)}</td>
                <td className={table.td}>{e.detail}</td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
