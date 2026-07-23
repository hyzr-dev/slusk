import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { apiGet, ApiError } from '../api/client';
import { queryKeys, useConfig, useTestConnection, useUpdateConfig } from '../api/queries';
import type { AppConfig, ConfigUpdateRequest, SharedFolderDTO } from '../api/types';
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
      {config && <ConfigForm config={config} />}

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.connections}</h2>
        <ConnectionTest label={t.settings.lidarr} dependency="lidarr" />
        {/* Only offer the Soulseek test when the native client is enabled —
            otherwise there is nothing to connect to. */}
        {config?.soulseek.enabled && (
          <ConnectionTest label={t.settings.soulseek} dependency="soulseek" />
        )}
      </section>
    </>
  );
}

// --- Generic field-descriptor rendering -----------------------------------
//
// Every card below (Lidarr, slskd, Pipeline, Matching weights, Soulseek,
// Observability, Danger zone) is a flat set of string-valued inputs rendered
// from a small array of FieldDescriptors, instead of ~40 hand-written JSX
// blocks. Every state slice is a Record<K, string> — including numbers,
// which are kept as their raw input text and converted with Number() only
// when the POST body is built — mirroring v1's maxActive/minBitrate pattern.

// 'integer' and 'float' both render as a native number input (see
// nativeInputType below); they're kept distinct so client-side validation
// (see numericFieldErrors) knows whether a decimal point is an error.
type FieldKind = 'text' | 'integer' | 'float' | 'password' | 'select';

interface SelectOption {
  value: string;
  label: string;
}

interface FieldDescriptor<K extends string> {
  key: K;
  label: string;
  configKey?: string;
  kind: FieldKind;
  options?: readonly SelectOption[];
  placeholder?: string;
  // Dot-path into the POST body matching its own key names, e.g.
  // "pipeline.maxActive" — how a 422's fieldErrors map is looked up.
  errorKey: string;
}

function nativeInputType(kind: FieldKind): 'text' | 'number' | 'password' {
  return kind === 'integer' || kind === 'float' ? 'number' : kind === 'select' ? 'text' : kind;
}

// numericFieldErrors runs the two minimal client-side checks the server
// can't attribute to a field itself: an empty numeric input (Number('') is
// silently 0, not "unset") and a non-integer value in an integer-kind field
// (today only surfaced as a generic 400 from a failed JSON decode). Anything
// else — ranges, formats, cross-field rules — stays server-validated only.
function numericFieldErrors<K extends string>(
  fields: readonly FieldDescriptor<K>[],
  state: Record<K, string>,
): Record<string, string> {
  const errors: Record<string, string> = {};
  for (const f of fields) {
    if (f.kind !== 'integer' && f.kind !== 'float') continue;
    const raw = state[f.key].trim();
    if (raw === '') {
      errors[f.errorKey] = t.settings.fieldRequired;
    } else if (f.kind === 'integer' && !/^-?\d+$/.test(raw)) {
      errors[f.errorKey] = t.settings.mustBeWholeNumber;
    }
  }
  return errors;
}

function Field({
  label,
  configKey,
  kind,
  value,
  options,
  placeholder,
  error,
  disabled,
  onChange,
}: {
  label: string;
  configKey?: string;
  kind: FieldKind;
  value: string;
  options?: readonly SelectOption[];
  placeholder?: string;
  error?: string;
  disabled: boolean;
  onChange: (value: string) => void;
}) {
  return (
    <label className={styles.field}>
      <span className={styles.label}>
        {label}
        {configKey && <span className={styles.key}> ({configKey})</span>}
      </span>
      {kind === 'select' ? (
        <select
          className={styles.select}
          value={value}
          disabled={disabled}
          aria-invalid={error ? true : undefined}
          onChange={(e) => onChange(e.target.value)}
        >
          {options?.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      ) : (
        <input
          className={styles.input}
          type={nativeInputType(kind)}
          step={kind === 'integer' ? '1' : undefined}
          value={value}
          placeholder={placeholder}
          disabled={disabled}
          aria-invalid={error ? true : undefined}
          // Secret fields hold API keys/tokens/DSNs, not login credentials;
          // keep browser password managers from offering to save or fill them.
          autoComplete={kind === 'password' ? 'new-password' : undefined}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {error && <span className={styles.fieldError}>{error}</span>}
    </label>
  );
}

function FieldGrid<K extends string>({
  fields,
  state,
  onChange,
  fieldErrors,
  disabled,
}: {
  fields: readonly FieldDescriptor<K>[];
  state: Record<K, string>;
  onChange: (key: K, value: string) => void;
  fieldErrors: Record<string, string>;
  disabled: boolean;
}) {
  return (
    <div className={styles.grid}>
      {fields.map((f) => (
        <Field
          key={f.key}
          label={f.label}
          configKey={f.configKey}
          kind={f.kind}
          options={f.options}
          value={state[f.key]}
          placeholder={f.placeholder}
          error={fieldErrors[f.errorKey]}
          disabled={disabled}
          onChange={(v) => onChange(f.key, v)}
        />
      ))}
    </div>
  );
}

function SectionCard<K extends string>({
  title,
  fields,
  state,
  onChange,
  fieldErrors,
  disabled,
}: {
  title: string;
  fields: readonly FieldDescriptor<K>[];
  state: Record<K, string>;
  onChange: (key: K, value: string) => void;
  fieldErrors: Record<string, string>;
  disabled: boolean;
}) {
  return (
    <section className={styles.group}>
      <h2 className={styles.groupTitle}>{title}</h2>
      <FieldGrid
        fields={fields}
        state={state}
        onChange={onChange}
        fieldErrors={fieldErrors}
        disabled={disabled}
      />
    </section>
  );
}

// --- Form state shapes ------------------------------------------------------

type LidarrFieldKey = 'url' | 'apiKey';
type SlskdFieldKey = 'url' | 'apiKey';

type PipelineFieldKey =
  | 'backend'
  | 'maxCandidatesPerAlbum'
  | 'maxActive'
  | 'maxRetries'
  | 'maxInflightPerPeer'
  | 'maxTransferRetries'
  | 'minBitrate'
  | 'transferDeadline'
  | 'stallTimeout'
  | 'searchTimeout'
  | 'backoffBase'
  | 'backoffCap'
  | 'candidateTtl'
  | 'failedReviveAfter'
  | 'stuckAfter'
  | 'tickTimeout'
  | 'importConfirmTimeout'
  | 'wantedSyncInterval'
  | 'discoveryInterval'
  | 'selectingInterval'
  | 'downloadingInterval'
  | 'importingInterval'
  | 'manualImportTimeout'
  | 'importRetryCooldown';
type PipelineFormState = Record<PipelineFieldKey, string>;

type WeightsFieldKey = 'format' | 'bitrate' | 'reliability' | 'fileCount' | 'knownUser';
type WeightsFormState = Record<WeightsFieldKey, string>;

type SoulseekFieldKey =
  | 'serverAddress'
  | 'username'
  | 'password'
  | 'listenAddr'
  | 'uploadSlots'
  | 'allowPrivatePeerAddresses'
  | 'gluetunControlUrl'
  | 'gluetunApiKey';
type SoulseekFormState = Record<SoulseekFieldKey, string>;

type DangerFieldKey = 'dsn' | 'listenAddr' | 'authToken' | 'slskdCompleteDir';
type DangerFormState = Record<DangerFieldKey, string>;

interface FolderRow {
  key: string;
  name: string;
  path: string;
}

function pipelineToForm(p: AppConfig['pipeline']): PipelineFormState {
  return {
    backend: p.backend,
    maxCandidatesPerAlbum: String(p.maxCandidatesPerAlbum),
    maxActive: String(p.maxActive),
    maxRetries: String(p.maxRetries),
    maxInflightPerPeer: String(p.maxInflightPerPeer),
    maxTransferRetries: String(p.maxTransferRetries),
    minBitrate: String(p.minBitrate),
    transferDeadline: p.transferDeadline,
    stallTimeout: p.stallTimeout,
    searchTimeout: p.searchTimeout,
    backoffBase: p.backoffBase,
    backoffCap: p.backoffCap,
    candidateTtl: p.candidateTtl,
    failedReviveAfter: p.failedReviveAfter,
    stuckAfter: p.stuckAfter,
    tickTimeout: p.tickTimeout,
    importConfirmTimeout: p.importConfirmTimeout,
    wantedSyncInterval: p.wantedSyncInterval,
    discoveryInterval: p.discoveryInterval,
    selectingInterval: p.selectingInterval,
    downloadingInterval: p.downloadingInterval,
    importingInterval: p.importingInterval,
    manualImportTimeout: p.manualImportTimeout,
    importRetryCooldown: p.importRetryCooldown,
  };
}

function weightsToForm(w: AppConfig['pipeline']['weights']): WeightsFormState {
  return {
    format: String(w.format),
    bitrate: String(w.bitrate),
    reliability: String(w.reliability),
    fileCount: String(w.fileCount),
    knownUser: String(w.knownUser),
  };
}

function soulseekToForm(s: AppConfig['soulseek']): SoulseekFormState {
  return {
    serverAddress: s.serverAddress,
    username: s.username,
    password: '',
    listenAddr: s.listenAddr,
    uploadSlots: String(s.uploadSlots),
    allowPrivatePeerAddresses: String(s.allowPrivatePeerAddresses),
    gluetunControlUrl: s.gluetun.controlUrl,
    gluetunApiKey: '',
  };
}

// ConfigForm renders once config has loaded (Settings only mounts it once
// config is truthy), so seeding useState from config here is safe: after a
// successful save, the restart-poll effect below invalidates the cached
// config once the process comes back up, which makes useConfig() refetch and
// re-render this same ConfigForm instance with a new `config` prop — React
// does not remount it (same component, no key change), so these useState
// seeds never re-run. That's not a bug: what's already on screen is exactly
// what was just submitted and saved, so it already matches the refetched
// config. writable:false disables every input and hides the Save row, rather
// than rendering a separate read-only tree, since duplicating ~40 fields
// across two components isn't worth it for a disabled attribute.
function ConfigForm({ config }: { config: AppConfig }) {
  const disabled = !config.writable;

  const [lidarrUrl, setLidarrUrl] = useState(config.lidarr.url);
  const [lidarrApiKey, setLidarrApiKey] = useState('');
  const [slskdUrl, setSlskdUrl] = useState(config.slskd.url);
  const [slskdApiKey, setSlskdApiKey] = useState('');

  const [pipeline, setPipeline] = useState<PipelineFormState>(() => pipelineToForm(config.pipeline));
  const [weights, setWeights] = useState<WeightsFormState>(() => weightsToForm(config.pipeline.weights));
  const [soulseek, setSoulseek] = useState<SoulseekFormState>(() => soulseekToForm(config.soulseek));
  const folderKeySeq = useRef(0);
  const [folders, setFolders] = useState<FolderRow[]>(() =>
    config.soulseek.sharedFolders.map((f) => ({
      key: `folder-${folderKeySeq.current++}`,
      name: f.name,
      path: f.path,
    })),
  );
  const [logLevel, setLogLevel] = useState(config.observ.logLevel);

  const [danger, setDanger] = useState<DangerFormState>({
    dsn: '',
    listenAddr: config.observ.listenAddr,
    authToken: '',
    slskdCompleteDir: config.paths.slskdCompleteDir,
  });
  const [dangerArmed, setDangerArmed] = useState(false);
  const dangerTouched =
    danger.listenAddr !== config.observ.listenAddr ||
    danger.slskdCompleteDir !== config.paths.slskdCompleteDir ||
    danger.dsn.trim() !== '' ||
    danger.authToken.trim() !== '';

  // Disarm if the user edits fields back to the untouched state after arming.
  useEffect(() => {
    if (!dangerTouched) setDangerArmed(false);
  }, [dangerTouched]);

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

  function handleLidarrChange(key: LidarrFieldKey, value: string) {
    if (key === 'url') setLidarrUrl(value);
    else setLidarrApiKey(value);
  }

  function handleSlskdChange(key: SlskdFieldKey, value: string) {
    if (key === 'url') setSlskdUrl(value);
    else setSlskdApiKey(value);
  }

  function handlePipelineChange(key: PipelineFieldKey, value: string) {
    setPipeline((prev) => ({ ...prev, [key]: value }));
  }

  function handleWeightsChange(key: WeightsFieldKey, value: string) {
    setWeights((prev) => ({ ...prev, [key]: value }));
  }

  function handleSoulseekChange(key: SoulseekFieldKey, value: string) {
    setSoulseek((prev) => ({ ...prev, [key]: value }));
  }

  function handleDangerChange(key: DangerFieldKey, value: string) {
    setDanger((prev) => ({ ...prev, [key]: value }));
  }

  function updateFolder(index: number, field: 'name' | 'path', value: string) {
    setFolders((prev) => prev.map((row, i) => (i === index ? { ...row, [field]: value } : row)));
  }

  function addFolder() {
    setFolders((prev) => [...prev, { key: `folder-${folderKeySeq.current++}`, name: '', path: '' }]);
  }

  function removeFolder(index: number) {
    setFolders((prev) => prev.filter((_, i) => i !== index));
  }

  function buildBody(): ConfigUpdateRequest {
    const sharedFolders: SharedFolderDTO[] = folders.map((f) => ({ name: f.name, path: f.path }));

    const body: ConfigUpdateRequest = {
      lidarr: { url: lidarrUrl },
      slskd: { url: slskdUrl },
      pipeline: {
        backend: pipeline.backend === 'soulseek' ? 'soulseek' : 'slskd',
        maxCandidatesPerAlbum: Number(pipeline.maxCandidatesPerAlbum),
        maxActive: Number(pipeline.maxActive),
        maxRetries: Number(pipeline.maxRetries),
        maxInflightPerPeer: Number(pipeline.maxInflightPerPeer),
        maxTransferRetries: Number(pipeline.maxTransferRetries),
        minBitrate: Number(pipeline.minBitrate),
        transferDeadline: pipeline.transferDeadline,
        stallTimeout: pipeline.stallTimeout,
        searchTimeout: pipeline.searchTimeout,
        backoffBase: pipeline.backoffBase,
        backoffCap: pipeline.backoffCap,
        candidateTtl: pipeline.candidateTtl,
        failedReviveAfter: pipeline.failedReviveAfter,
        stuckAfter: pipeline.stuckAfter,
        tickTimeout: pipeline.tickTimeout,
        importConfirmTimeout: pipeline.importConfirmTimeout,
        wantedSyncInterval: pipeline.wantedSyncInterval,
        discoveryInterval: pipeline.discoveryInterval,
        selectingInterval: pipeline.selectingInterval,
        downloadingInterval: pipeline.downloadingInterval,
        importingInterval: pipeline.importingInterval,
        manualImportTimeout: pipeline.manualImportTimeout,
        importRetryCooldown: pipeline.importRetryCooldown,
        weights: {
          format: Number(weights.format),
          bitrate: Number(weights.bitrate),
          reliability: Number(weights.reliability),
          fileCount: Number(weights.fileCount),
          knownUser: Number(weights.knownUser),
        },
      },
      soulseek: {
        serverAddress: soulseek.serverAddress,
        username: soulseek.username,
        listenAddr: soulseek.listenAddr,
        uploadSlots: Number(soulseek.uploadSlots),
        allowPrivatePeerAddresses: soulseek.allowPrivatePeerAddresses === 'true',
        gluetun: { controlUrl: soulseek.gluetunControlUrl },
        sharedFolders,
      },
      store: {},
      observ: { listenAddr: danger.listenAddr, logLevel },
      paths: { slskdCompleteDir: danger.slskdCompleteDir },
    };

    if (lidarrApiKey.trim() !== '') body.lidarr.apiKey = lidarrApiKey;
    if (slskdApiKey.trim() !== '') body.slskd.apiKey = slskdApiKey;
    if (soulseek.password.trim() !== '') body.soulseek.password = soulseek.password;
    if (soulseek.gluetunApiKey.trim() !== '') body.soulseek.gluetun.apiKey = soulseek.gluetunApiKey;
    if (danger.dsn.trim() !== '') body.store.dsn = danger.dsn;
    if (danger.authToken.trim() !== '') body.observ.authToken = danger.authToken;

    return body;
  }

  function submit() {
    setFieldErrors({});
    setSaveError('');

    update.mutate(buildBody(), {
      onSuccess: () => {
        setLidarrApiKey('');
        setSlskdApiKey('');
        setSoulseek((prev) => ({ ...prev, password: '', gluetunApiKey: '' }));
        setDanger((prev) => ({ ...prev, dsn: '', authToken: '' }));
        setDangerArmed(false);
        setRestarting(true);
      },
      onError: (err) => {
        if (
          err instanceof ApiError &&
          err.status === 422 &&
          err.body?.fieldErrors &&
          Object.keys(err.body.fieldErrors).length > 0
        ) {
          setFieldErrors(err.body.fieldErrors);
        } else if (err instanceof ApiError && err.body?.error) {
          setSaveError(err.body.error);
        } else {
          setSaveError(t.settings.saveFailed);
        }
      },
    });
  }

  const secretPlaceholder = (configured: boolean) =>
    configured ? t.settings.secretPlaceholderConfigured : t.settings.secretPlaceholderMissing;

  const lidarrFields: readonly FieldDescriptor<LidarrFieldKey>[] = [
    { key: 'url', label: t.settings.url, configKey: t.settings.configKeys.lidarrUrl, kind: 'text', errorKey: 'lidarr.url' },
    {
      key: 'apiKey',
      label: t.settings.apiKey,
      configKey: t.settings.configKeys.lidarrApiKey,
      kind: 'password',
      placeholder: secretPlaceholder(config.lidarr.apiKeyConfigured),
      errorKey: 'lidarr.apiKey',
    },
  ];

  const slskdFields: readonly FieldDescriptor<SlskdFieldKey>[] = [
    { key: 'url', label: t.settings.url, configKey: t.settings.configKeys.slskdUrl, kind: 'text', errorKey: 'slskd.url' },
    {
      key: 'apiKey',
      label: t.settings.apiKey,
      configKey: t.settings.configKeys.slskdApiKey,
      kind: 'password',
      placeholder: secretPlaceholder(config.slskd.apiKeyConfigured),
      errorKey: 'slskd.apiKey',
    },
  ];

  const backendOptions: readonly SelectOption[] = [
    { value: 'slskd', label: t.settings.backendSlskd },
    { value: 'soulseek', label: t.settings.backendSoulseek },
  ];

  const pipelineFields: readonly FieldDescriptor<PipelineFieldKey>[] = [
    { key: 'backend', label: t.settings.backend, configKey: t.settings.configKeys.backend, kind: 'select', options: backendOptions, errorKey: 'pipeline.backend' },
    { key: 'maxCandidatesPerAlbum', label: t.settings.maxCandidatesPerAlbum, configKey: t.settings.configKeys.maxCandidatesPerAlbum, kind: 'integer', errorKey: 'pipeline.maxCandidatesPerAlbum' },
    { key: 'maxActive', label: t.settings.maxActive, configKey: t.settings.configKeys.maxActive, kind: 'integer', errorKey: 'pipeline.maxActive' },
    { key: 'maxRetries', label: t.settings.maxRetries, configKey: t.settings.configKeys.maxRetries, kind: 'integer', errorKey: 'pipeline.maxRetries' },
    { key: 'maxInflightPerPeer', label: t.settings.maxInflightPerPeer, configKey: t.settings.configKeys.maxInflightPerPeer, kind: 'integer', errorKey: 'pipeline.maxInflightPerPeer' },
    { key: 'maxTransferRetries', label: t.settings.maxTransferRetries, configKey: t.settings.configKeys.maxTransferRetries, kind: 'integer', errorKey: 'pipeline.maxTransferRetries' },
    { key: 'minBitrate', label: t.settings.minBitrate, configKey: t.settings.configKeys.minBitrate, kind: 'integer', errorKey: 'pipeline.minBitrate' },
    { key: 'transferDeadline', label: t.settings.transferDeadline, configKey: t.settings.configKeys.transferDeadline, kind: 'text', errorKey: 'pipeline.transferDeadline' },
    { key: 'stallTimeout', label: t.settings.stallTimeout, configKey: t.settings.configKeys.stallTimeout, kind: 'text', errorKey: 'pipeline.stallTimeout' },
    { key: 'searchTimeout', label: t.settings.searchTimeout, configKey: t.settings.configKeys.searchTimeout, kind: 'text', errorKey: 'pipeline.searchTimeout' },
    { key: 'backoffBase', label: t.settings.backoffBase, configKey: t.settings.configKeys.backoffBase, kind: 'text', errorKey: 'pipeline.backoffBase' },
    { key: 'backoffCap', label: t.settings.backoffCap, configKey: t.settings.configKeys.backoffCap, kind: 'text', errorKey: 'pipeline.backoffCap' },
    { key: 'candidateTtl', label: t.settings.candidateTtl, configKey: t.settings.configKeys.candidateTtl, kind: 'text', errorKey: 'pipeline.candidateTtl' },
    { key: 'failedReviveAfter', label: t.settings.failedReviveAfter, configKey: t.settings.configKeys.failedReviveAfter, kind: 'text', errorKey: 'pipeline.failedReviveAfter' },
    { key: 'stuckAfter', label: t.settings.stuckAfter, configKey: t.settings.configKeys.stuckAfter, kind: 'text', errorKey: 'pipeline.stuckAfter' },
    { key: 'tickTimeout', label: t.settings.tickTimeout, configKey: t.settings.configKeys.tickTimeout, kind: 'text', errorKey: 'pipeline.tickTimeout' },
    { key: 'importConfirmTimeout', label: t.settings.importConfirmTimeout, configKey: t.settings.configKeys.importConfirmTimeout, kind: 'text', errorKey: 'pipeline.importConfirmTimeout' },
    { key: 'wantedSyncInterval', label: t.settings.wantedSyncInterval, configKey: t.settings.configKeys.wantedSyncInterval, kind: 'text', errorKey: 'pipeline.wantedSyncInterval' },
    { key: 'discoveryInterval', label: t.settings.discoveryInterval, configKey: t.settings.configKeys.discoveryInterval, kind: 'text', errorKey: 'pipeline.discoveryInterval' },
    { key: 'selectingInterval', label: t.settings.selectingInterval, configKey: t.settings.configKeys.selectingInterval, kind: 'text', errorKey: 'pipeline.selectingInterval' },
    { key: 'downloadingInterval', label: t.settings.downloadingInterval, configKey: t.settings.configKeys.downloadingInterval, kind: 'text', errorKey: 'pipeline.downloadingInterval' },
    { key: 'importingInterval', label: t.settings.importingInterval, configKey: t.settings.configKeys.importingInterval, kind: 'text', errorKey: 'pipeline.importingInterval' },
    { key: 'manualImportTimeout', label: t.settings.manualImportTimeout, configKey: t.settings.configKeys.manualImportTimeout, kind: 'text', errorKey: 'pipeline.manualImportTimeout' },
    { key: 'importRetryCooldown', label: t.settings.importRetryCooldown, configKey: t.settings.configKeys.importRetryCooldown, kind: 'text', errorKey: 'pipeline.importRetryCooldown' },
  ];

  const weightsFields: readonly FieldDescriptor<WeightsFieldKey>[] = [
    { key: 'format', label: t.settings.weightFormat, configKey: t.settings.configKeys.weightFormat, kind: 'float', errorKey: 'pipeline.weights.format' },
    { key: 'bitrate', label: t.settings.weightBitrate, configKey: t.settings.configKeys.weightBitrate, kind: 'float', errorKey: 'pipeline.weights.bitrate' },
    { key: 'reliability', label: t.settings.weightReliability, configKey: t.settings.configKeys.weightReliability, kind: 'float', errorKey: 'pipeline.weights.reliability' },
    { key: 'fileCount', label: t.settings.weightFileCount, configKey: t.settings.configKeys.weightFileCount, kind: 'float', errorKey: 'pipeline.weights.fileCount' },
    { key: 'knownUser', label: t.settings.weightKnownUser, configKey: t.settings.configKeys.weightKnownUser, kind: 'float', errorKey: 'pipeline.weights.knownUser' },
  ];

  const allowPrivatePeerAddressesOptions: readonly SelectOption[] = [
    { value: 'false', label: t.settings.allowPrivatePeerAddressesBlocked },
    { value: 'true', label: t.settings.allowPrivatePeerAddressesAllowed },
  ];

  const soulseekFields: readonly FieldDescriptor<SoulseekFieldKey>[] = [
    { key: 'serverAddress', label: t.settings.serverAddress, configKey: t.settings.configKeys.serverAddress, kind: 'text', errorKey: 'soulseek.serverAddress' },
    { key: 'username', label: t.settings.username, configKey: t.settings.configKeys.username, kind: 'text', errorKey: 'soulseek.username' },
    {
      key: 'password',
      label: t.settings.password,
      configKey: t.settings.configKeys.password,
      kind: 'password',
      placeholder: secretPlaceholder(config.soulseek.passwordConfigured),
      errorKey: 'soulseek.password',
    },
    { key: 'listenAddr', label: t.settings.listenAddr, configKey: t.settings.configKeys.soulseekListenAddr, kind: 'text', errorKey: 'soulseek.listenAddr' },
    { key: 'uploadSlots', label: t.settings.uploadSlots, configKey: t.settings.configKeys.uploadSlots, kind: 'integer', errorKey: 'soulseek.uploadSlots' },
    {
      key: 'allowPrivatePeerAddresses',
      label: t.settings.allowPrivatePeerAddresses,
      configKey: t.settings.configKeys.allowPrivatePeerAddresses,
      kind: 'select',
      options: allowPrivatePeerAddressesOptions,
      errorKey: 'soulseek.allowPrivatePeerAddresses',
    },
  ];

  const gluetunFields: readonly FieldDescriptor<SoulseekFieldKey>[] = [
    { key: 'gluetunControlUrl', label: t.settings.gluetunControlUrl, configKey: t.settings.configKeys.gluetunControlUrl, kind: 'text', errorKey: 'soulseek.gluetun.controlUrl' },
    {
      key: 'gluetunApiKey',
      label: t.settings.apiKey,
      configKey: t.settings.configKeys.gluetunApiKey,
      kind: 'password',
      placeholder: secretPlaceholder(config.soulseek.gluetun.apiKeyConfigured),
      errorKey: 'soulseek.gluetun.apiKey',
    },
  ];

  const logLevelOptions: readonly SelectOption[] = [
    { value: '', label: t.settings.logLevelDefault },
    { value: 'debug', label: t.settings.logLevelDebug },
    { value: 'info', label: t.settings.logLevelInfo },
    { value: 'warn', label: t.settings.logLevelWarn },
    { value: 'error', label: t.settings.logLevelError },
  ];

  const observFields: readonly FieldDescriptor<'logLevel'>[] = [
    { key: 'logLevel', label: t.settings.logLevel, configKey: t.settings.configKeys.logLevel, kind: 'select', options: logLevelOptions, errorKey: 'observ.logLevel' },
  ];

  const dangerFields: readonly FieldDescriptor<DangerFieldKey>[] = [
    {
      key: 'dsn',
      label: t.settings.dsn,
      configKey: t.settings.configKeys.dsn,
      kind: 'password',
      placeholder: secretPlaceholder(config.store.dsnConfigured),
      errorKey: 'store.dsn',
    },
    { key: 'listenAddr', label: t.settings.listenAddr, configKey: t.settings.configKeys.observListenAddr, kind: 'text', errorKey: 'observ.listenAddr' },
    {
      key: 'authToken',
      label: t.settings.authToken,
      configKey: t.settings.configKeys.authToken,
      kind: 'password',
      placeholder: secretPlaceholder(config.observ.authTokenConfigured),
      errorKey: 'observ.authToken',
    },
    { key: 'slskdCompleteDir', label: t.settings.slskdCompleteDir, configKey: t.settings.configKeys.slskdCompleteDir, kind: 'text', errorKey: 'paths.slskdCompleteDir' },
  ];

  // Client-side checks for the numeric fields only — anything a server round
  // trip would otherwise report as a generic 400 (a decimal in an integer
  // field fails Go's JSON decode) or silently miscompute (Number('') = 0).
  // Every other rule (ranges, formats, cross-field) stays server-validated.
  function handleSaveClick() {
    const localErrors = {
      ...numericFieldErrors(pipelineFields, pipeline),
      ...numericFieldErrors(weightsFields, weights),
      ...numericFieldErrors(soulseekFields, soulseek),
    };
    if (Object.keys(localErrors).length > 0) {
      setFieldErrors(localErrors);
      setSaveError('');
      return;
    }
    if (dangerTouched && !dangerArmed) {
      setDangerArmed(true);
      return;
    }
    submit();
  }

  const saveLabel = update.isPending
    ? t.settings.saving
    : dangerTouched && dangerArmed
      ? t.settings.saveConfirm
      : t.settings.save;

  return (
    <>
      <SectionCard title={t.settings.lidarr} fields={lidarrFields} state={{ url: lidarrUrl, apiKey: lidarrApiKey }} onChange={handleLidarrChange} fieldErrors={fieldErrors} disabled={disabled} />
      <SectionCard title={t.settings.slskd} fields={slskdFields} state={{ url: slskdUrl, apiKey: slskdApiKey }} onChange={handleSlskdChange} fieldErrors={fieldErrors} disabled={disabled} />
      <SectionCard title={t.settings.pipeline} fields={pipelineFields} state={pipeline} onChange={handlePipelineChange} fieldErrors={fieldErrors} disabled={disabled} />
      <SectionCard title={t.settings.weights} fields={weightsFields} state={weights} onChange={handleWeightsChange} fieldErrors={fieldErrors} disabled={disabled} />

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.soulseek}</h2>
        <FieldGrid fields={soulseekFields} state={soulseek} onChange={handleSoulseekChange} fieldErrors={fieldErrors} disabled={disabled} />

        <h3 className={styles.subTitle}>{t.settings.gluetunTitle}</h3>
        <FieldGrid fields={gluetunFields} state={soulseek} onChange={handleSoulseekChange} fieldErrors={fieldErrors} disabled={disabled} />

        <h3 className={styles.subTitle}>
          {t.settings.sharedFoldersTitle}
          <span className={styles.key}> ({t.settings.configKeys.sharedFolders})</span>
        </h3>
        <div className={styles.folderRows}>
          {folders.map((row, i) => (
            <div className={styles.folderRow} key={row.key}>
              <Field
                label={t.settings.folderName}
                kind="text"
                value={row.name}
                disabled={disabled}
                error={fieldErrors[`soulseek.sharedFolders[${i}].name`]}
                onChange={(v) => updateFolder(i, 'name', v)}
              />
              <Field
                label={t.settings.folderPath}
                kind="text"
                value={row.path}
                disabled={disabled}
                error={fieldErrors[`soulseek.sharedFolders[${i}].path`]}
                onChange={(v) => updateFolder(i, 'path', v)}
              />
              {!disabled && (
                <button type="button" className={styles.removeFolderButton} onClick={() => removeFolder(i)}>
                  {t.settings.removeFolder}
                </button>
              )}
            </div>
          ))}
          {!disabled && (
            <button type="button" className={styles.addFolderButton} onClick={addFolder}>
              {t.settings.addFolder}
            </button>
          )}
        </div>
      </section>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.observability}</h2>
        <FieldGrid
          fields={observFields}
          state={{ logLevel }}
          onChange={(_key, value) => setLogLevel(value)}
          fieldErrors={fieldErrors}
          disabled={disabled}
        />
      </section>

      <section className={styles.dangerGroup}>
        <h2 className={styles.dangerTitle}>{t.settings.dangerZone}</h2>
        <FieldGrid fields={dangerFields} state={danger} onChange={handleDangerChange} fieldErrors={fieldErrors} disabled={disabled} />
        <div className={styles.dangerHint}>{t.settings.dangerRecoveryHint}</div>
      </section>

      {!disabled && (
        <div className={styles.saveRow}>
          <button className={styles.saveButton} disabled={update.isPending} onClick={handleSaveClick}>
            {saveLabel}
          </button>
          {dangerArmed && <span className={styles.dangerWarning}>{t.settings.dangerConfirmWarning}</span>}
          {saveError && <span className={styles.saveError}>{saveError}</span>}
          {restarting && <span className={styles.restartingBanner}>{t.settings.savedRestarting}</span>}
        </div>
      )}
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
    <div className={styles.testRow}>
      <label className={styles.testLabel}>{label}</label>
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
