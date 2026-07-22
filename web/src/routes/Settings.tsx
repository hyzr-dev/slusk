import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { apiGet, ApiError } from '../api/client';
import { queryKeys, useConfig, useTestConnection, useUpdateConfig } from '../api/queries';
import type { AppConfig, ConfigUpdateRequest } from '../api/types';
import PageHeading from '../components/PageHeading';
import { t } from '../strings';
import styles from './Settings.module.css';

export default function Settings() {
  const { data: config } = useConfig();

  return (
    <>
      <PageHeading>{t.nav.settings}</PageHeading>
      {config && !config.writable && (
        <div className={styles.notice}>{t.settings.notWritableNotice}</div>
      )}
      {config && config.writable ? (
        <EditableFields config={config} />
      ) : (
        <ReadOnlyFields config={config} />
      )}

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

function ReadOnlyFields({ config }: { config?: AppConfig }) {
  return (
    <>
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
        <Field
          label={t.settings.minBitrate}
          configKey={t.settings.configKeys.minBitrate}
          value={String(config?.minBitrate ?? '—')}
        />
        <Field
          label={t.settings.stallTimeout}
          configKey={t.settings.configKeys.stallTimeout}
          value={config?.stallTimeout ?? '—'}
        />
      </section>
    </>
  );
}

// EditableFields renders once config has loaded (Settings only mounts it once
// config is truthy), so seeding useState from config here is safe — it never
// needs to react to a later config change, since a successful save always
// restarts the process and reloads this whole page.
function EditableFields({ config }: { config: AppConfig }) {
  const [lidarrUrl, setLidarrUrl] = useState(config.lidarrUrl);
  const [lidarrApiKey, setLidarrApiKey] = useState('');
  const [wantedSyncInterval, setWantedSyncInterval] = useState(config.wantedSyncInterval);
  const [maxActive, setMaxActive] = useState(String(config.maxActive));
  const [minBitrate, setMinBitrate] = useState(String(config.minBitrate));
  const [stallTimeout, setStallTimeout] = useState(config.stallTimeout);

  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [saveError, setSaveError] = useState('');
  const [restarting, setRestarting] = useState(false);

  const update = useUpdateConfig();
  const qc = useQueryClient();

  // Once restarting, poll GET /api/config until the restarted process answers
  // again, then invalidate the cached config (staleTime: Infinity means it
  // otherwise never refetches on its own) and clear the banner.
  useEffect(() => {
    if (!restarting) return;
    const id = setInterval(() => {
      apiGet<AppConfig>('/api/config').then(
        () => {
          clearInterval(id);
          setRestarting(false);
          void qc.invalidateQueries({ queryKey: queryKeys.config });
        },
        () => {
          // Still restarting; keep polling.
        },
      );
    }, 2000);
    return () => clearInterval(id);
  }, [restarting, qc]);

  function handleSave() {
    setFieldErrors({});
    setSaveError('');

    const body: ConfigUpdateRequest = {
      lidarrUrl,
      wantedSyncInterval,
      stallTimeout,
      maxActive: Number(maxActive),
      minBitrate: Number(minBitrate),
    };
    if (lidarrApiKey.trim() !== '') {
      body.lidarrApiKey = lidarrApiKey;
    }

    update.mutate(body, {
      onSuccess: () => {
        setLidarrApiKey('');
        setRestarting(true);
      },
      onError: (err) => {
        if (err instanceof ApiError && err.status === 422 && err.body?.fieldErrors) {
          setFieldErrors(err.body.fieldErrors);
        } else if (err instanceof ApiError && err.body?.error) {
          setSaveError(err.body.error);
        } else {
          setSaveError(t.settings.saveFailed);
        }
      },
    });
  }

  return (
    <>
      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.lidarr}</h2>
        <Field
          label={t.settings.url}
          value={lidarrUrl}
          onChange={setLidarrUrl}
          error={fieldErrors.lidarrUrl}
        />
        <Field
          label={t.settings.apiKey}
          type="password"
          value={lidarrApiKey}
          onChange={setLidarrApiKey}
          placeholder={
            config.lidarrApiKeyConfigured
              ? t.settings.apiKeyPlaceholderConfigured
              : t.settings.apiKeyPlaceholderMissing
          }
          error={fieldErrors.lidarrApiKey}
        />
      </section>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.pipeline}</h2>
        <Field
          label={t.settings.wantedSyncInterval}
          configKey={t.settings.configKeys.wantedSyncInterval}
          value={wantedSyncInterval}
          onChange={setWantedSyncInterval}
          error={fieldErrors.wantedSyncInterval}
        />
        <Field
          label={t.settings.maxActive}
          configKey={t.settings.configKeys.maxActive}
          type="number"
          value={maxActive}
          onChange={setMaxActive}
          error={fieldErrors.maxActive}
        />
        <Field
          label={t.settings.minBitrate}
          configKey={t.settings.configKeys.minBitrate}
          type="number"
          value={minBitrate}
          onChange={setMinBitrate}
          error={fieldErrors.minBitrate}
        />
        <Field
          label={t.settings.stallTimeout}
          configKey={t.settings.configKeys.stallTimeout}
          value={stallTimeout}
          onChange={setStallTimeout}
          error={fieldErrors.stallTimeout}
        />
      </section>

      <div className={styles.saveRow}>
        <button
          className={styles.saveButton}
          disabled={update.isPending}
          onClick={handleSave}
        >
          {update.isPending ? t.settings.saving : t.settings.save}
        </button>
        {saveError && <span className={styles.saveError}>{saveError}</span>}
        {restarting && (
          <span className={styles.restartingBanner}>{t.settings.savedRestarting}</span>
        )}
      </div>
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

// Field renders a labeled input. Passing onChange makes it editable; omitting
// it (the read-only views) renders a disabled, read-only input, matching the
// prior always-read-only behavior.
function Field({
  label,
  configKey,
  value,
  onChange,
  type = 'text',
  placeholder,
  error,
}: {
  label: string;
  configKey?: string;
  value: string;
  onChange?: (value: string) => void;
  type?: 'text' | 'password' | 'number';
  placeholder?: string;
  error?: string;
}) {
  return (
    <div className={styles.field}>
      <label className={styles.label}>
        {label}
        {configKey && <span className={styles.key}> ({configKey})</span>}
      </label>
      <input
        className={styles.input}
        type={type}
        value={value}
        placeholder={placeholder}
        disabled={!onChange}
        readOnly={!onChange}
        // The password field holds an API key, not a login credential; keep
        // browser password managers from offering to save or fill it.
        autoComplete={type === 'password' ? 'new-password' : undefined}
        onChange={onChange ? (e) => onChange(e.target.value) : undefined}
      />
      {error && <span className={styles.fieldError}>{error}</span>}
    </div>
  );
}
