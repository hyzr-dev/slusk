import { useConfig, useTestConnection } from '../api/queries';
import PageHeading from '../components/PageHeading';
import { t } from '../strings';
import styles from './Settings.module.css';

export default function Settings() {
  const { data: config } = useConfig();

  return (
    <>
      <PageHeading>{t.nav.settings}</PageHeading>
      <div className={styles.notice}>{t.settings.readOnlyNotice}</div>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.lidarr}</h2>
        <Field label={t.settings.url} value={config?.lidarrUrl ?? '—'} />
        <Field
          label={t.settings.apiKey}
          value={
            config?.lidarrApiKeyConfigured
              ? t.settings.apiKeyHidden
              : t.settings.apiKeyMissing
          }
        />
      </section>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.pipeline}</h2>
        <Field
          label={t.settings.wantedSyncInterval}
          configKey={t.settings.configKeys.wantedSyncInterval}
          value={config?.wantedSyncInterval ?? '—'}
        />
        <Field
          label={t.settings.maxActive}
          configKey={t.settings.configKeys.maxActive}
          value={String(config?.maxActive ?? '—')}
        />
      </section>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.connections}</h2>
        <ConnectionTest label={t.settings.lidarr} dependency="lidarr" />
        {/* Only offer the Soulseek test when the native client is enabled —
            otherwise there is nothing to connect to. */}
        {config?.soulseekEnabled && (
          <ConnectionTest label={t.settings.soulseek} dependency="soulseek" />
        )}
      </section>
    </>
  );
}

// ConnectionTest renders one dependency's test button plus its four-state
// status. A failed *test* is a 200 response with ok:false (test.data.error); a
// failed *request* is test.isError (the endpoint itself was unreachable).
function ConnectionTest({
  label,
  dependency,
}: {
  label: string;
  dependency: 'lidarr' | 'soulseek';
}) {
  const test = useTestConnection(dependency);
  const result = test.data;

  let status: 'untested' | 'testing' | 'success' | 'failure' = 'untested';
  let message = '';
  if (test.isPending) {
    status = 'testing';
  } else if (test.isError) {
    status = 'failure';
    message = t.settings.testUnreachable;
  } else if (result) {
    status = result.ok ? 'success' : 'failure';
    message = result.ok ? '' : result.error ?? t.settings.testFailed;
  }

  return (
    <div className={styles.field}>
      <label className={styles.label}>{label}</label>
      <button
        className={styles.testButton}
        disabled={test.isPending}
        onClick={() => test.mutate()}
      >
        {t.settings.testConnection}
      </button>
      <span className={`${styles.status} ${styles[status]}`}>
        {t.settings.testStatus[status]}
      </span>
      {message && <span className={styles.testError}>{message}</span>}
    </div>
  );
}

function Field({
  label,
  configKey,
  value,
}: {
  label: string;
  configKey?: string;
  value: string;
}) {
  return (
    <div className={styles.field}>
      <label className={styles.label}>
        {label}
        {configKey && <span className={styles.key}> ({configKey})</span>}
      </label>
      <input className={styles.input} value={value} disabled readOnly />
    </div>
  );
}
