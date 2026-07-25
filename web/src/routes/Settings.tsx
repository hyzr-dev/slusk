import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { apiGet, ApiError } from '../api/client';
import { queryKeys, useConfig, useTestConnection, useUpdateConfig } from '../api/queries';
import type { AppConfig, ConfigUpdateRequest, SharedFolderDTO } from '../api/types';
import Button from '../components/tui/Button';
import SectionHeader from '../components/tui/SectionHeader';
import { t } from '../strings';
import styles from './Settings.module.css';

export default function Settings() {
  const { data: config } = useConfig();

  return (
    <>
      {config && !config.writable && (
        <div className={styles.notice}>{t.settings.notWritableNotice}</div>
      )}
      {config && <ConfigForm config={config} />}

      {/* Not collapsible (unlike the cards above), so this uses the plain
          SectionHeader primitive rather than CollapsibleSection's
          accordion. */}
      <section className={styles.group}>
        <SectionHeader label={t.settings.connections} />
        <div className={styles.sectionBody}>
          <ConnectionTest label={t.settings.lidarr} dependency="lidarr" />
          {/* Only offer the Soulseek test when the native client is enabled —
              otherwise there is nothing to connect to. */}
          {config?.soulseek.enabled && (
            <ConnectionTest label={t.settings.soulseek} dependency="soulseek" />
          )}
        </div>
      </section>
    </>
  );
}

// --- Generic field-descriptor rendering -----------------------------------
//
// Every card below (Lidarr, slskd, Pipeline, Soulseek, Observability, Danger
// zone) is a flat set of string-valued inputs rendered from a small array of
// FieldDescriptors, instead of ~40 hand-written JSX blocks. Every state slice
// is a Record<K, string> — including numbers, which are kept as their raw
// input text and converted with Number() only when the POST body is built —
// mirroring v1's maxActive/minBitrate pattern.

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
  // One or two sentences shown in a HelpPopover next to the field's label.
  help: string;
  // Set on fields rendered inside a section's Advanced disclosure; read by
  // registerErrorKeys below to decide whether a 422/local error on this
  // field also needs to open that disclosure, not just the section itself.
  advanced?: true;
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

// Shallow string-record comparison used for the per-section "changed" dirty
// check: every form-state slice in this file is a flat Record<K, string>, so
// this alone is enough — no need for a deep-equal library.
function shallowEqual<K extends string>(a: Record<K, string>, b: Record<K, string>): boolean {
  const keys = Object.keys(a) as K[];
  if (keys.length !== Object.keys(b).length) return false;
  return keys.every((k) => a[k] === b[k]);
}

type SectionId = 'lidarr' | 'slskd' | 'pipeline' | 'soulseek' | 'observ' | 'danger';

function allSectionsCollapsed(): Record<SectionId, boolean> {
  return { lidarr: false, slskd: false, pipeline: false, soulseek: false, observ: false, danger: false };
}

type ErrorLocation = { section: SectionId; advanced: boolean };

// Builds the errorKey -> {section, advanced} lookup used to auto-expand the
// right section (and its Advanced disclosure) when a save attempt reports a
// field error. advanced is read off each descriptor rather than passed in
// per call, so a field moved between a section's basic/advanced arrays stays
// correctly wired here without a second edit.
function registerErrorKeys<K extends string>(
  map: Record<string, ErrorLocation>,
  fields: readonly FieldDescriptor<K>[],
  section: SectionId,
) {
  for (const f of fields) map[f.errorKey] = { section, advanced: !!f.advanced };
}

function HelpPopover({ text, label }: { text: string; label: string }) {
  const [open, setOpen] = useState(false);
  const panelId = useId();
  const wrapperRef = useRef<HTMLSpanElement>(null);

  // Attached only while open, per field, so idle popovers cost nothing.
  useEffect(() => {
    if (!open) return;
    function handlePointerDown(e: MouseEvent) {
      if (wrapperRef.current && !wrapperRef.current.contains(e.target as Node)) setOpen(false);
    }
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(false);
    }
    document.addEventListener('mousedown', handlePointerDown);
    document.addEventListener('keydown', handleKeyDown);
    return () => {
      document.removeEventListener('mousedown', handlePointerDown);
      document.removeEventListener('keydown', handleKeyDown);
    };
  }, [open]);

  return (
    <span className={styles.helpWrapper} ref={wrapperRef}>
      <button
        type="button"
        className={styles.helpButton}
        aria-expanded={open}
        aria-controls={panelId}
        aria-label={`${t.settings.helpButtonLabel}: ${label}`}
        onClick={() => setOpen((o) => !o)}
      >
        ?
      </button>
      {open && (
        <div className={styles.helpPanel} id={panelId}>
          {text}
        </div>
      )}
    </span>
  );
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
  help,
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
  help?: string;
  onChange: (value: string) => void;
}) {
  // An explicit id/htmlFor pair, not the "wrap the input in <label>" pattern
  // used elsewhere, because the HelpPopover's own <button> would otherwise
  // become the label's first labelable descendant (buttons are labelable
  // too) and hijack its implicit association with the actual input.
  const inputId = useId();
  return (
    <div className={styles.field}>
      <span className={styles.labelRow}>
        <label className={styles.label} htmlFor={inputId}>
          {label}
          {configKey && <span className={styles.key}> ({configKey})</span>}
        </label>
        {help && <HelpPopover text={help} label={label} />}
      </span>
      {/* Input/select and its error share one column (see the field's
          190px 1fr grid in Settings.module.css) — grouped in a wrapper so
          the error lands under the value, not under the label. */}
      <span className={styles.value}>
        {kind === 'select' ? (
          <select
            id={inputId}
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
            id={inputId}
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
        {error && (
          <span className={styles.fieldError} data-field-error>
            {error}
          </span>
        )}
      </span>
    </div>
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
          help={f.help}
          onChange={(v) => onChange(f.key, v)}
        />
      ))}
    </div>
  );
}

// A quieter, nested disclosure for a section's advanced fields — collapsed
// by default, smaller header typography than CollapsibleSection.
function AdvancedDisclosure({
  open,
  onToggle,
  children,
}: {
  open: boolean;
  onToggle: () => void;
  children: ReactNode;
}) {
  return (
    <div className={styles.advancedGroup}>
      <button type="button" className={styles.advancedHeader} aria-expanded={open} onClick={onToggle}>
        <span className={styles.advancedTitle}>{t.settings.advanced}</span>
        <span className={styles.chevron} aria-hidden="true">
          {open ? '▾' : '▸'}
        </span>
      </button>
      {open && <div className={styles.advancedBody}>{children}</div>}
    </div>
  );
}

// The accordion shell every top-level card uses: a header row with the
// toggle <button> nested inside a real <h2> (the conforming ARIA-accordion
// shape — the reverse would strip the heading from the accessibility tree,
// since the button role treats its descendants as presentational). The
// button's stretched ::after keeps the whole row clickable while both the
// heading's and the button's accessible names stay exactly `title`, which
// is what the tests' getByRole lookups rely on.
function CollapsibleSection({
  title,
  changed,
  open,
  onToggle,
  danger,
  children,
}: {
  title: string;
  changed: boolean;
  open: boolean;
  onToggle: () => void;
  danger?: boolean;
  children: ReactNode;
}) {
  return (
    <section className={danger ? styles.dangerGroup : styles.group}>
      <div className={styles.sectionHeader}>
        <h2 className={danger ? styles.dangerTitle : styles.groupTitle}>
          <button
            type="button"
            className={styles.sectionHeaderButton}
            aria-expanded={open}
            onClick={onToggle}
          >
            {title}
          </button>
        </h2>
        <span className={styles.sectionHeaderRight}>
          {changed && <span className={styles.changedBadge}>{t.settings.changedBadge}</span>}
          <span className={styles.chevron} aria-hidden="true">
            {open ? '▾' : '▸'}
          </span>
        </span>
      </div>
      {open && <div className={styles.sectionBody}>{children}</div>}
    </section>
  );
}

function SectionCard<K extends string>({
  title,
  fields,
  state,
  onChange,
  fieldErrors,
  disabled,
  open,
  onToggle,
  changed,
  advancedFields,
  advancedOpen,
  onAdvancedToggle,
  advancedExtra,
}: {
  title: string;
  fields: readonly FieldDescriptor<K>[];
  state: Record<K, string>;
  onChange: (key: K, value: string) => void;
  fieldErrors: Record<string, string>;
  disabled: boolean;
  open: boolean;
  onToggle: () => void;
  changed: boolean;
  advancedFields?: readonly FieldDescriptor<K>[];
  advancedOpen?: boolean;
  onAdvancedToggle?: () => void;
  advancedExtra?: ReactNode;
}) {
  return (
    <CollapsibleSection title={title} changed={changed} open={open} onToggle={onToggle}>
      <FieldGrid fields={fields} state={state} onChange={onChange} fieldErrors={fieldErrors} disabled={disabled} />
      {advancedFields && advancedFields.length > 0 && (
        <AdvancedDisclosure open={!!advancedOpen} onToggle={onAdvancedToggle ?? (() => {})}>
          <FieldGrid
            fields={advancedFields}
            state={state}
            onChange={onChange}
            fieldErrors={fieldErrors}
            disabled={disabled}
          />
          {advancedExtra}
        </AdvancedDisclosure>
      )}
    </CollapsibleSection>
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

// Folders derived from config, for the dirty check below — same shape as
// FolderRow minus the synthetic React `key`, in document order.
function foldersEqual(a: readonly { name: string; path: string }[], b: readonly FolderRow[]): boolean {
  if (a.length !== b.length) return false;
  return a.every((row, i) => row.name === b[i].name && row.path === b[i].path);
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
// config is truthy), so seeding useState from config here is safe. The
// useState seeds run only on mount; after a successful save, the restart
// poll re-seeds the form from the freshly fetched config (see reseedForm
// below). writable:false disables every input and hides the Save row, rather
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

  // Re-seed every form slice from a freshly fetched config. Called when the
  // restart poll confirms a save was applied: the saved config can differ
  // from what was typed even though the save succeeded (the backend echoes
  // canonical duration forms — "5m" comes back as "5m0s"), so without this
  // the per-section dirty badges would stick permanently.
  function reseedForm(fresh: AppConfig) {
    setLidarrUrl(fresh.lidarr.url);
    setLidarrApiKey('');
    setSlskdUrl(fresh.slskd.url);
    setSlskdApiKey('');
    setPipeline(pipelineToForm(fresh.pipeline));
    setWeights(weightsToForm(fresh.pipeline.weights));
    setSoulseek(soulseekToForm(fresh.soulseek));
    setFolders(
      fresh.soulseek.sharedFolders.map((f) => ({
        key: `folder-${folderKeySeq.current++}`,
        name: f.name,
        path: f.path,
      })),
    );
    setLogLevel(fresh.observ.logLevel);
    setDanger({
      dsn: '',
      listenAddr: fresh.observ.listenAddr,
      authToken: '',
      slskdCompleteDir: fresh.paths.slskdCompleteDir,
    });
    // A save attempted during the restart window can leave errors pointing at
    // values this re-seed just replaced — drop them along with the old state.
    setFieldErrors({});
    setSaveError('');
  }

  // Every card starts collapsed; expandForErrors below opens whichever ones
  // a failed save reports errors in (and their Advanced disclosure, when the
  // erroring field lives there).
  const [sectionOpen, setSectionOpen] = useState<Record<SectionId, boolean>>(allSectionsCollapsed);
  const [advancedOpen, setAdvancedOpen] = useState<Record<SectionId, boolean>>(allSectionsCollapsed);

  // Bumped whenever a save attempt produces field errors, to scroll the
  // first one into view once the sections containing them have expanded.
  // Never fires on an ordinary re-render (initial 0 is a no-op).
  const [scrollNonce, setScrollNonce] = useState(0);
  useEffect(() => {
    if (scrollNonce === 0) return;
    document.querySelector('[data-field-error]')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }, [scrollNonce]);

  function toggleSection(id: SectionId) {
    setSectionOpen((prev) => ({ ...prev, [id]: !prev[id] }));
  }

  function toggleAdvanced(id: SectionId) {
    setAdvancedOpen((prev) => ({ ...prev, [id]: !prev[id] }));
  }

  const update = useUpdateConfig();
  const qc = useQueryClient();

  // Once restarting, poll GET /api/config until the restarted process answers
  // again, then invalidate the cached config (staleTime: Infinity means it
  // otherwise never refetches on its own), clear the banner, and re-seed the
  // form from the polled config. Re-seeding from the poll response rather
  // than from the refetched query keeps it deterministic: structural sharing
  // preserves the cached object's identity when the refetched config is
  // deep-equal, so a prop-identity check would miss exactly the case where
  // only the typed spelling differs from the saved canonical form.
  useEffect(() => {
    if (!restarting) return;
    const id = setInterval(() => {
      apiGet<AppConfig>('/api/config').then(
        (fresh) => {
          clearInterval(id);
          setRestarting(false);
          void qc.invalidateQueries({ queryKey: queryKeys.config });
          reseedForm(fresh);
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

  // Per-section dirty detection for the "changed" badge: each initial slice
  // below is exactly what the corresponding useState seed above computed
  // from config, memoized so re-renders don't reconstruct it every keystroke.
  const initialLidarr = useMemo(() => ({ url: config.lidarr.url, apiKey: '' }), [config]);
  const initialSlskd = useMemo(() => ({ url: config.slskd.url, apiKey: '' }), [config]);
  const initialPipeline = useMemo(() => pipelineToForm(config.pipeline), [config]);
  const initialWeights = useMemo(() => weightsToForm(config.pipeline.weights), [config]);
  const initialSoulseek = useMemo(() => soulseekToForm(config.soulseek), [config]);
  const initialFolders = useMemo(
    () => config.soulseek.sharedFolders.map((f) => ({ name: f.name, path: f.path })),
    [config],
  );

  const lidarrDirty = !shallowEqual(initialLidarr, { url: lidarrUrl, apiKey: lidarrApiKey });
  const slskdDirty = !shallowEqual(initialSlskd, { url: slskdUrl, apiKey: slskdApiKey });
  // Weights live inside Pipeline's Advanced disclosure, not their own card,
  // so a weight edit marks the Pipeline section itself dirty.
  const pipelineDirty = !shallowEqual(initialPipeline, pipeline) || !shallowEqual(initialWeights, weights);
  const soulseekDirty = !shallowEqual(initialSoulseek, soulseek) || !foldersEqual(initialFolders, folders);
  const observDirty = logLevel !== config.observ.logLevel;

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
          if (!expandForErrors(err.body.fieldErrors)) {
            setSaveError(err.body.error ?? t.settings.saveFailed);
          }
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
    {
      key: 'url',
      label: t.settings.url,
      configKey: t.settings.configKeys.lidarrUrl,
      kind: 'text',
      errorKey: 'lidarr.url',
      help: t.settings.help.lidarrUrl,
    },
    {
      key: 'apiKey',
      label: t.settings.apiKey,
      configKey: t.settings.configKeys.lidarrApiKey,
      kind: 'password',
      placeholder: secretPlaceholder(config.lidarr.apiKeyConfigured),
      errorKey: 'lidarr.apiKey',
      help: t.settings.help.lidarrApiKey,
    },
  ];

  const slskdFields: readonly FieldDescriptor<SlskdFieldKey>[] = [
    {
      key: 'url',
      label: t.settings.url,
      configKey: t.settings.configKeys.slskdUrl,
      kind: 'text',
      errorKey: 'slskd.url',
      help: t.settings.help.slskdUrl,
    },
    {
      key: 'apiKey',
      label: t.settings.apiKey,
      configKey: t.settings.configKeys.slskdApiKey,
      kind: 'password',
      placeholder: secretPlaceholder(config.slskd.apiKeyConfigured),
      errorKey: 'slskd.apiKey',
      help: t.settings.help.slskdApiKey,
    },
  ];

  const backendOptions: readonly SelectOption[] = [
    { value: 'slskd', label: t.settings.backendSlskd },
    { value: 'soulseek', label: t.settings.backendSoulseek },
  ];

  // Basic: the knobs most installs actually need to touch day-to-day.
  const pipelineBasicFields: readonly FieldDescriptor<PipelineFieldKey>[] = [
    { key: 'backend', label: t.settings.backend, configKey: t.settings.configKeys.backend, kind: 'select', options: backendOptions, errorKey: 'pipeline.backend', help: t.settings.help.backend },
    { key: 'wantedSyncInterval', label: t.settings.wantedSyncInterval, configKey: t.settings.configKeys.wantedSyncInterval, kind: 'text', errorKey: 'pipeline.wantedSyncInterval', help: t.settings.help.wantedSyncInterval },
    { key: 'maxActive', label: t.settings.maxActive, configKey: t.settings.configKeys.maxActive, kind: 'integer', errorKey: 'pipeline.maxActive', help: t.settings.help.maxActive },
    { key: 'minBitrate', label: t.settings.minBitrate, configKey: t.settings.configKeys.minBitrate, kind: 'integer', errorKey: 'pipeline.minBitrate', help: t.settings.help.minBitrate },
    { key: 'stallTimeout', label: t.settings.stallTimeout, configKey: t.settings.configKeys.stallTimeout, kind: 'text', errorKey: 'pipeline.stallTimeout', help: t.settings.help.stallTimeout },
    { key: 'searchTimeout', label: t.settings.searchTimeout, configKey: t.settings.configKeys.searchTimeout, kind: 'text', errorKey: 'pipeline.searchTimeout', help: t.settings.help.searchTimeout },
  ];

  // Advanced: everything else — retry/backoff tuning, per-phase interval
  // knobs, and timeouts most installs never need to change from defaults.
  const pipelineAdvancedFields: readonly FieldDescriptor<PipelineFieldKey>[] = [
    { key: 'maxCandidatesPerAlbum', label: t.settings.maxCandidatesPerAlbum, configKey: t.settings.configKeys.maxCandidatesPerAlbum, kind: 'integer', errorKey: 'pipeline.maxCandidatesPerAlbum', advanced: true, help: t.settings.help.maxCandidatesPerAlbum },
    { key: 'transferDeadline', label: t.settings.transferDeadline, configKey: t.settings.configKeys.transferDeadline, kind: 'text', errorKey: 'pipeline.transferDeadline', advanced: true, help: t.settings.help.transferDeadline },
    { key: 'maxInflightPerPeer', label: t.settings.maxInflightPerPeer, configKey: t.settings.configKeys.maxInflightPerPeer, kind: 'integer', errorKey: 'pipeline.maxInflightPerPeer', advanced: true, help: t.settings.help.maxInflightPerPeer },
    { key: 'maxTransferRetries', label: t.settings.maxTransferRetries, configKey: t.settings.configKeys.maxTransferRetries, kind: 'integer', errorKey: 'pipeline.maxTransferRetries', advanced: true, help: t.settings.help.maxTransferRetries },
    { key: 'maxRetries', label: t.settings.maxRetries, configKey: t.settings.configKeys.maxRetries, kind: 'integer', errorKey: 'pipeline.maxRetries', advanced: true, help: t.settings.help.maxRetries },
    { key: 'backoffBase', label: t.settings.backoffBase, configKey: t.settings.configKeys.backoffBase, kind: 'text', errorKey: 'pipeline.backoffBase', advanced: true, help: t.settings.help.backoffBase },
    { key: 'backoffCap', label: t.settings.backoffCap, configKey: t.settings.configKeys.backoffCap, kind: 'text', errorKey: 'pipeline.backoffCap', advanced: true, help: t.settings.help.backoffCap },
    { key: 'candidateTtl', label: t.settings.candidateTtl, configKey: t.settings.configKeys.candidateTtl, kind: 'text', errorKey: 'pipeline.candidateTtl', advanced: true, help: t.settings.help.candidateTtl },
    { key: 'failedReviveAfter', label: t.settings.failedReviveAfter, configKey: t.settings.configKeys.failedReviveAfter, kind: 'text', errorKey: 'pipeline.failedReviveAfter', advanced: true, help: t.settings.help.failedReviveAfter },
    { key: 'stuckAfter', label: t.settings.stuckAfter, configKey: t.settings.configKeys.stuckAfter, kind: 'text', errorKey: 'pipeline.stuckAfter', advanced: true, help: t.settings.help.stuckAfter },
    { key: 'tickTimeout', label: t.settings.tickTimeout, configKey: t.settings.configKeys.tickTimeout, kind: 'text', errorKey: 'pipeline.tickTimeout', advanced: true, help: t.settings.help.tickTimeout },
    { key: 'importConfirmTimeout', label: t.settings.importConfirmTimeout, configKey: t.settings.configKeys.importConfirmTimeout, kind: 'text', errorKey: 'pipeline.importConfirmTimeout', advanced: true, help: t.settings.help.importConfirmTimeout },
    { key: 'discoveryInterval', label: t.settings.discoveryInterval, configKey: t.settings.configKeys.discoveryInterval, kind: 'text', errorKey: 'pipeline.discoveryInterval', advanced: true, help: t.settings.help.discoveryInterval },
    { key: 'selectingInterval', label: t.settings.selectingInterval, configKey: t.settings.configKeys.selectingInterval, kind: 'text', errorKey: 'pipeline.selectingInterval', advanced: true, help: t.settings.help.selectingInterval },
    { key: 'downloadingInterval', label: t.settings.downloadingInterval, configKey: t.settings.configKeys.downloadingInterval, kind: 'text', errorKey: 'pipeline.downloadingInterval', advanced: true, help: t.settings.help.downloadingInterval },
    { key: 'importingInterval', label: t.settings.importingInterval, configKey: t.settings.configKeys.importingInterval, kind: 'text', errorKey: 'pipeline.importingInterval', advanced: true, help: t.settings.help.importingInterval },
    { key: 'manualImportTimeout', label: t.settings.manualImportTimeout, configKey: t.settings.configKeys.manualImportTimeout, kind: 'text', errorKey: 'pipeline.manualImportTimeout', advanced: true, help: t.settings.help.manualImportTimeout },
    { key: 'importRetryCooldown', label: t.settings.importRetryCooldown, configKey: t.settings.configKeys.importRetryCooldown, kind: 'text', errorKey: 'pipeline.importRetryCooldown', advanced: true, help: t.settings.help.importRetryCooldown },
  ];

  // Rendered inside Pipeline's Advanced disclosure (see advancedExtra below),
  // under their own "Weights" subTitle.
  const weightsFields: readonly FieldDescriptor<WeightsFieldKey>[] = [
    { key: 'format', label: t.settings.weightFormat, configKey: t.settings.configKeys.weightFormat, kind: 'float', errorKey: 'pipeline.weights.format', advanced: true, help: t.settings.help.weightFormat },
    { key: 'bitrate', label: t.settings.weightBitrate, configKey: t.settings.configKeys.weightBitrate, kind: 'float', errorKey: 'pipeline.weights.bitrate', advanced: true, help: t.settings.help.weightBitrate },
    { key: 'reliability', label: t.settings.weightReliability, configKey: t.settings.configKeys.weightReliability, kind: 'float', errorKey: 'pipeline.weights.reliability', advanced: true, help: t.settings.help.weightReliability },
    { key: 'fileCount', label: t.settings.weightFileCount, configKey: t.settings.configKeys.weightFileCount, kind: 'float', errorKey: 'pipeline.weights.fileCount', advanced: true, help: t.settings.help.weightFileCount },
    { key: 'knownUser', label: t.settings.weightKnownUser, configKey: t.settings.configKeys.weightKnownUser, kind: 'float', errorKey: 'pipeline.weights.knownUser', advanced: true, help: t.settings.help.weightKnownUser },
  ];

  const allowPrivatePeerAddressesOptions: readonly SelectOption[] = [
    { value: 'false', label: t.settings.allowPrivatePeerAddressesBlocked },
    { value: 'true', label: t.settings.allowPrivatePeerAddressesAllowed },
  ];

  // Basic: credentials, shared folders (rendered separately, see below) and
  // upload slots — what a native-backend install needs to get running.
  const soulseekBasicFields: readonly FieldDescriptor<SoulseekFieldKey>[] = [
    { key: 'username', label: t.settings.username, configKey: t.settings.configKeys.username, kind: 'text', errorKey: 'soulseek.username', help: t.settings.help.username },
    {
      key: 'password',
      label: t.settings.password,
      configKey: t.settings.configKeys.password,
      kind: 'password',
      placeholder: secretPlaceholder(config.soulseek.passwordConfigured),
      errorKey: 'soulseek.password',
      help: t.settings.help.password,
    },
    { key: 'uploadSlots', label: t.settings.uploadSlots, configKey: t.settings.configKeys.uploadSlots, kind: 'integer', errorKey: 'soulseek.uploadSlots', help: t.settings.help.uploadSlots },
  ];

  // Advanced: network-level settings most installs leave at their defaults.
  const soulseekAdvancedFields: readonly FieldDescriptor<SoulseekFieldKey>[] = [
    { key: 'serverAddress', label: t.settings.serverAddress, configKey: t.settings.configKeys.serverAddress, kind: 'text', errorKey: 'soulseek.serverAddress', advanced: true, help: t.settings.help.serverAddress },
    { key: 'listenAddr', label: t.settings.listenAddr, configKey: t.settings.configKeys.soulseekListenAddr, kind: 'text', errorKey: 'soulseek.listenAddr', advanced: true, help: t.settings.help.soulseekListenAddr },
    { key: 'allowPrivatePeerAddresses', label: t.settings.allowPrivatePeerAddresses, configKey: t.settings.configKeys.allowPrivatePeerAddresses, kind: 'select', options: allowPrivatePeerAddressesOptions, errorKey: 'soulseek.allowPrivatePeerAddresses', advanced: true, help: t.settings.help.allowPrivatePeerAddresses },
  ];

  const gluetunFields: readonly FieldDescriptor<SoulseekFieldKey>[] = [
    { key: 'gluetunControlUrl', label: t.settings.gluetunControlUrl, configKey: t.settings.configKeys.gluetunControlUrl, kind: 'text', errorKey: 'soulseek.gluetun.controlUrl', advanced: true, help: t.settings.help.gluetunControlUrl },
    {
      key: 'gluetunApiKey',
      label: t.settings.apiKey,
      configKey: t.settings.configKeys.gluetunApiKey,
      kind: 'password',
      placeholder: secretPlaceholder(config.soulseek.gluetun.apiKeyConfigured),
      errorKey: 'soulseek.gluetun.apiKey',
      advanced: true,
      help: t.settings.help.gluetunApiKey,
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
    { key: 'logLevel', label: t.settings.logLevel, configKey: t.settings.configKeys.logLevel, kind: 'select', options: logLevelOptions, errorKey: 'observ.logLevel', help: t.settings.help.logLevel },
  ];

  const dangerFields: readonly FieldDescriptor<DangerFieldKey>[] = [
    {
      key: 'dsn',
      label: t.settings.dsn,
      configKey: t.settings.configKeys.dsn,
      kind: 'password',
      placeholder: secretPlaceholder(config.store.dsnConfigured),
      errorKey: 'store.dsn',
      help: t.settings.help.dsn,
    },
    { key: 'listenAddr', label: t.settings.listenAddr, configKey: t.settings.configKeys.observListenAddr, kind: 'text', errorKey: 'observ.listenAddr', help: t.settings.help.observListenAddr },
    {
      key: 'authToken',
      label: t.settings.authToken,
      configKey: t.settings.configKeys.authToken,
      kind: 'password',
      placeholder: secretPlaceholder(config.observ.authTokenConfigured),
      errorKey: 'observ.authToken',
      help: t.settings.help.authToken,
    },
    { key: 'slskdCompleteDir', label: t.settings.slskdCompleteDir, configKey: t.settings.configKeys.slskdCompleteDir, kind: 'text', errorKey: 'paths.slskdCompleteDir', help: t.settings.help.slskdCompleteDir },
  ];

  const errorLocationMap: Record<string, ErrorLocation> = {};
  registerErrorKeys(errorLocationMap, lidarrFields, 'lidarr');
  registerErrorKeys(errorLocationMap, slskdFields, 'slskd');
  registerErrorKeys(errorLocationMap, pipelineBasicFields, 'pipeline');
  registerErrorKeys(errorLocationMap, pipelineAdvancedFields, 'pipeline');
  registerErrorKeys(errorLocationMap, weightsFields, 'pipeline');
  registerErrorKeys(errorLocationMap, soulseekBasicFields, 'soulseek');
  registerErrorKeys(errorLocationMap, soulseekAdvancedFields, 'soulseek');
  registerErrorKeys(errorLocationMap, gluetunFields, 'soulseek');
  registerErrorKeys(errorLocationMap, observFields, 'observ');
  registerErrorKeys(errorLocationMap, dangerFields, 'danger');

  // Shared-folder row errors (soulseek.sharedFolders[i].name/path) aren't in
  // the map above since folder rows aren't FieldDescriptors — they're always
  // basic Soulseek fields, so this one prefix check covers all of them.
  function locateError(errorKey: string): ErrorLocation | undefined {
    if (errorKey.startsWith('soulseek.sharedFolders')) return { section: 'soulseek', advanced: false };
    return errorLocationMap[errorKey];
  }

  // Expands every section (and Advanced disclosure) containing one of the
  // given errorKeys, then bumps scrollNonce so the effect above scrolls the
  // first errored field into view. Called for both a 422's fieldErrors and
  // the client-side numericFieldErrors result. Returns false when none of
  // the keys maps to a rendered field, so the 422 handler can fall back to
  // the banner instead of failing invisibly behind collapsed sections.
  function expandForErrors(errors: Record<string, string>): boolean {
    const sectionsToOpen = new Set<SectionId>();
    const advancedToOpen = new Set<SectionId>();
    for (const key of Object.keys(errors)) {
      const loc = locateError(key);
      if (!loc) continue;
      sectionsToOpen.add(loc.section);
      if (loc.advanced) advancedToOpen.add(loc.section);
    }
    if (sectionsToOpen.size === 0) return false;

    setSectionOpen((prev) => {
      const next = { ...prev };
      for (const s of sectionsToOpen) next[s] = true;
      return next;
    });
    if (advancedToOpen.size > 0) {
      setAdvancedOpen((prev) => {
        const next = { ...prev };
        for (const s of advancedToOpen) next[s] = true;
        return next;
      });
    }
    setScrollNonce((n) => n + 1);
    return true;
  }

  const pipelineAllFields = [...pipelineBasicFields, ...pipelineAdvancedFields];
  const soulseekAllFields = [...soulseekBasicFields, ...soulseekAdvancedFields];

  // Client-side checks for the numeric fields only — anything a server round
  // trip would otherwise report as a generic 400 (a decimal in an integer
  // field fails Go's JSON decode) or silently miscompute (Number('') = 0).
  // Every other rule (ranges, formats, cross-field) stays server-validated.
  function handleSaveClick() {
    const localErrors = {
      ...numericFieldErrors(pipelineAllFields, pipeline),
      ...numericFieldErrors(weightsFields, weights),
      ...numericFieldErrors(soulseekAllFields, soulseek),
    };
    if (Object.keys(localErrors).length > 0) {
      setFieldErrors(localErrors);
      setSaveError('');
      expandForErrors(localErrors);
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
      <SectionCard
        title={t.settings.lidarr}
        fields={lidarrFields}
        state={{ url: lidarrUrl, apiKey: lidarrApiKey }}
        onChange={handleLidarrChange}
        fieldErrors={fieldErrors}
        disabled={disabled}
        open={sectionOpen.lidarr}
        onToggle={() => toggleSection('lidarr')}
        changed={lidarrDirty}
      />
      <SectionCard
        title={t.settings.slskd}
        fields={slskdFields}
        state={{ url: slskdUrl, apiKey: slskdApiKey }}
        onChange={handleSlskdChange}
        fieldErrors={fieldErrors}
        disabled={disabled}
        open={sectionOpen.slskd}
        onToggle={() => toggleSection('slskd')}
        changed={slskdDirty}
      />
      <SectionCard
        title={t.settings.pipeline}
        fields={pipelineBasicFields}
        state={pipeline}
        onChange={handlePipelineChange}
        fieldErrors={fieldErrors}
        disabled={disabled}
        open={sectionOpen.pipeline}
        onToggle={() => toggleSection('pipeline')}
        changed={pipelineDirty}
        advancedFields={pipelineAdvancedFields}
        advancedOpen={advancedOpen.pipeline}
        onAdvancedToggle={() => toggleAdvanced('pipeline')}
        advancedExtra={
          <div>
            <h3 className={styles.subTitle}>{t.settings.weights}</h3>
            <FieldGrid
              fields={weightsFields}
              state={weights}
              onChange={handleWeightsChange}
              fieldErrors={fieldErrors}
              disabled={disabled}
            />
          </div>
        }
      />

      <CollapsibleSection
        title={t.settings.soulseek}
        changed={soulseekDirty}
        open={sectionOpen.soulseek}
        onToggle={() => toggleSection('soulseek')}
      >
        <FieldGrid fields={soulseekBasicFields} state={soulseek} onChange={handleSoulseekChange} fieldErrors={fieldErrors} disabled={disabled} />

        {/* The popover sits beside the <h3>, not inside it — heading content
            is announced as part of the heading, so an open panel's whole text
            would otherwise become the h3's accessible content. */}
        <div className={styles.subTitleRow}>
          <h3 className={styles.subTitle}>
            {t.settings.sharedFoldersTitle}
            <span className={styles.key}> ({t.settings.configKeys.sharedFolders})</span>
          </h3>
          <HelpPopover text={t.settings.help.sharedFolders} label={t.settings.sharedFoldersTitle} />
        </div>
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
                <Button onClick={() => removeFolder(i)}>{t.settings.removeFolder}</Button>
              )}
            </div>
          ))}
          {!disabled && <Button onClick={addFolder}>{t.settings.addFolder}</Button>}
        </div>

        <AdvancedDisclosure open={advancedOpen.soulseek} onToggle={() => toggleAdvanced('soulseek')}>
          <FieldGrid fields={soulseekAdvancedFields} state={soulseek} onChange={handleSoulseekChange} fieldErrors={fieldErrors} disabled={disabled} />

          <div className={styles.subTitleRow}>
            <h3 className={styles.subTitle}>{t.settings.gluetunTitle}</h3>
            <HelpPopover text={t.settings.help.gluetun} label={t.settings.gluetunTitle} />
          </div>
          <FieldGrid fields={gluetunFields} state={soulseek} onChange={handleSoulseekChange} fieldErrors={fieldErrors} disabled={disabled} />
        </AdvancedDisclosure>
      </CollapsibleSection>

      <SectionCard
        title={t.settings.observability}
        fields={observFields}
        state={{ logLevel }}
        onChange={(_key, value) => setLogLevel(value)}
        fieldErrors={fieldErrors}
        disabled={disabled}
        open={sectionOpen.observ}
        onToggle={() => toggleSection('observ')}
        changed={observDirty}
      />

      <CollapsibleSection
        title={t.settings.dangerZone}
        changed={dangerTouched}
        open={sectionOpen.danger}
        onToggle={() => toggleSection('danger')}
        danger
      >
        <FieldGrid fields={dangerFields} state={danger} onChange={handleDangerChange} fieldErrors={fieldErrors} disabled={disabled} />
        <div className={styles.dangerHint}>{t.settings.dangerRecoveryHint}</div>
      </CollapsibleSection>

      {!disabled && (
        <div className={styles.saveRow}>
          <Button variant="primary" disabled={update.isPending} onClick={handleSaveClick}>
            {saveLabel}
          </Button>
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
      <Button disabled={test.isPending} onClick={() => test.mutate()}>
        {t.settings.testConnection}
      </Button>
      <span className={`${styles.status} ${styles[status]}`}>
        {t.settings.testStatus[status]}
      </span>
      {message && <span className={styles.testError}>{message}</span>}
    </div>
  );
}
