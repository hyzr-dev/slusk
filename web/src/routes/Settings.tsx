import { useConfig } from '../api/queries';
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
    </>
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
