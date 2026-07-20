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
        <h2 className={styles.groupTitle}>{t.settings.reconcile}</h2>
        <Field label={t.settings.interval} value={config?.reconcileInterval ?? '—'} />
        <Field
          label={t.settings.concurrentDownloads}
          value={String(config?.maxConcurrentDownloads ?? '—')}
        />
      </section>
    </>
  );
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className={styles.field}>
      <label className={styles.label}>{label}</label>
      <input className={styles.input} value={value} disabled readOnly />
    </div>
  );
}
