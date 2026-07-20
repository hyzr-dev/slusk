import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useJobs } from '../api/queries';
import type { JobStatus } from '../api/types';
import PageHeading from '../components/PageHeading';
import ProgressBar from '../components/ProgressBar';
import StatusPill from '../components/StatusPill';
import table from '../components/Table.module.css';
import { formatBytes } from '../format';
import { t } from '../strings';
import { matchesFilters } from './jobFilter';
import styles from './Jobs.module.css';

const STATUSES: JobStatus[] = ['queued', 'active', 'stalled', 'done', 'failed'];

export default function Jobs() {
  const navigate = useNavigate();
  const { data: jobs = [] } = useJobs();
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState('');

  const filtered = jobs.filter((j) => matchesFilters(j, search, status));

  return (
    <>
      <PageHeading>{t.nav.jobs}</PageHeading>

      <div className={styles.controls}>
        <input
          className={styles.input}
          type="text"
          placeholder={t.jobs.searchPlaceholder}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <select
          className={styles.select}
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="">{t.jobs.allStatuses}</option>
          {STATUSES.map((s) => (
            <option key={s} value={s}>{t.status[s]}</option>
          ))}
        </select>
      </div>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.id}</th>
            <th className={table.th}>{t.columns.status}</th>
            <th className={table.th}>{t.columns.album}</th>
            <th className={table.th}>{t.columns.peer}</th>
            <th className={table.th}>{t.columns.progress}</th>
          </tr>
        </thead>
        <tbody>
          {filtered.length === 0 ? (
            <tr>
              <td className={table.empty} colSpan={5}>{t.jobs.empty}</td>
            </tr>
          ) : (
            filtered.map((j) => (
              <tr
                key={j.id}
                className={table.rowClickable}
                onClick={() => navigate(`/jobs/${j.id}`)}
              >
                <td className={`${table.td} ${table.mono}`}>#{j.id}</td>
                <td className={table.td}><StatusPill status={j.status} state={j.state} /></td>
                <td className={table.td}>
                  <div>{j.title}</div>
                  <div className={styles.sub}>{j.artist}</div>
                </td>
                <td className={table.td}>{j.peer || '—'}</td>
                <td className={table.td}>
                  {j.bytesTotal > 0 && (
                    <div className={styles.bytes}>
                      {formatBytes(j.bytesDone)} / {formatBytes(j.bytesTotal)}
                    </div>
                  )}
                  <ProgressBar done={j.bytesDone} total={j.bytesTotal} />
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
