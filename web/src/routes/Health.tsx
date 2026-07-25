import { useStatus } from '../api/queries';
import table from '../components/Table.module.css';
import { formatTime } from '../format';
import { t } from '../strings';
import styles from './Health.module.css';

export default function Health() {
  const { data: status } = useStatus();
  const modules = status?.moduleDetails ?? {};
  const names = Object.keys(modules).sort();

  return (
    <>
      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.module}</th>
            <th className={table.th}>{t.columns.lastRun}</th>
          </tr>
        </thead>
        <tbody>
          {names.length === 0 ? (
            <tr><td className={table.empty} colSpan={2}>{t.health.empty}</td></tr>
          ) : (
            names.map((name) => {
              const m = modules[name];
              const label = m.lastAttempt ? formatTime(m.lastAttempt) : t.health.neverRun;
              return (
                <tr key={name}>
                  <td className={table.td}>{name}</td>
                  <td
                    className={`${table.td} ${m.ready ? '' : styles.unhealthy}`}
                    title={m.lastError}
                  >
                    {label}
                    {m.consecutiveFailures > 0 &&
                      ` (${t.health.consecutiveFailures(m.consecutiveFailures)})`}
                  </td>
                </tr>
              );
            })
          )}
        </tbody>
      </table>
    </>
  );
}
