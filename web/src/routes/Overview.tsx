import { useNavigate } from 'react-router-dom';
import { useJobs } from '../api/queries';
import type { JobStatus } from '../api/types';
import PageHeading from '../components/PageHeading';
import ProgressBar from '../components/ProgressBar';
import StatCard from '../components/StatCard';
import table from '../components/Table.module.css';
import { formatBytes } from '../format';
import { t } from '../strings';
import styles from './Overview.module.css';

// The legacy dashboard omitted the failed card even though it counted the
// status; showing it is a deliberate fix (#87).
const CARDS: JobStatus[] = ['queued', 'active', 'stalled', 'done', 'failed'];

export default function Overview() {
  const navigate = useNavigate();
  const { data: jobs = [] } = useJobs();

  const counts = Object.fromEntries(
    CARDS.map((s) => [s, jobs.filter((j) => j.status === s).length]),
  ) as Record<JobStatus, number>;

  // Overview deliberately ignores the Jobs view's search and status filters.
  const active = jobs.filter((j) => j.status === 'active');

  return (
    <>
      <PageHeading>{t.nav.overview}</PageHeading>

      <div className={styles.cards}>
        {CARDS.map((s) => (
          <StatCard key={s} label={t.status[s]} value={counts[s]} />
        ))}
      </div>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.album}</th>
            <th className={table.th}>{t.columns.peer}</th>
            <th className={table.th}>{t.columns.progress}</th>
          </tr>
        </thead>
        <tbody>
          {active.length === 0 ? (
            <tr>
              <td className={table.empty} colSpan={3}>{t.overview.empty}</td>
            </tr>
          ) : (
            active.map((j) => (
              <tr
                key={j.id}
                className={table.rowClickable}
                onClick={() => navigate(`/jobs/${j.id}`)}
              >
                <td className={table.td}>
                  <div>{j.title}</div>
                  <div className={styles.sub}>{j.artist}</div>
                </td>
                <td className={table.td}>{j.peer || '—'}</td>
                <td className={table.td}>
                  <div className={styles.bytes}>
                    {formatBytes(j.bytesDone)} / {formatBytes(j.bytesTotal)}
                  </div>
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
