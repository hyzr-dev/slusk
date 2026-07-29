import { useConfig, useShares, useTestConnection } from '../api/queries';
import type { UseMutationResult } from '@tanstack/react-query';
import type { ConnectionTestResult } from '../api/types';
import Button from '../components/tui/Button';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import { t } from '../strings';
import styles from './Setup.module.css';

type StepState = 'ok' | 'failed' | 'untested' | 'testing' | 'disabled';

const STATE_LABEL: Record<StepState, string> = {
  ok: t.setup.stateOk,
  failed: t.setup.stateFailed,
  untested: t.setup.stateUntested,
  testing: t.setup.testing,
  disabled: t.setup.stateDisabled,
};

// Built as an explicit map rather than a computed `styles[\`state${x}\`]`
// lookup: CSS Modules' ambient type is an index signature, so a typo in a
// computed key would still type-check as `string` and only fail silently at
// render (no class applied). A literal map fails loudly instead — a renamed
// or removed class here is a compile error.
const STATE_CLASS: Record<StepState, string> = {
  ok: styles.stateOk,
  failed: styles.stateFailed,
  untested: styles.stateUntested,
  testing: styles.stateTesting,
  disabled: styles.stateDisabled,
};

function secretValue(configured: boolean): string {
  return configured ? t.setup.secretSet : t.setup.secretUnset;
}

// Maps a useTestConnection() mutation's state onto the four states the
// pipeline brief distinguishes for a probed dependency (the fifth state,
// 'disabled', only applies to Soulseek and is decided by the caller from
// config, not from this mutation). A failed *request* (the endpoint itself
// unreachable) and a failed *test* (200 with ok:false) both read as
// 'failed' — the distinction only changes which fallback message is shown,
// since a returned error string always wins when one exists.
function connectionState(
  test: UseMutationResult<ConnectionTestResult, Error, void>,
): { state: StepState; error?: string } {
  if (test.isPending) return { state: 'testing' };
  if (test.isError) return { state: 'failed', error: t.settings.testUnreachable };
  if (test.data) {
    return test.data.ok
      ? { state: 'ok' }
      : { state: 'failed', error: test.data.error ?? t.settings.testFailed };
  }
  return { state: 'untested' };
}

// `state` is optional: step 3's badge would otherwise have to assert 'ok' or
// 'untested' before /api/shares has answered, which is a conclusion the page
// cannot back up yet. Omitting the badge is preferred over inventing a
// StepState for "unknown" — the step's own QueryNotice already says so.
function StepHeader({ num, title, state, onTest }: { num: number; title: string; state?: StepState; onTest?: () => void }) {
  return (
    <SectionHeader
      label={`${num}  ${title}`}
      meta={
        <span className={styles.stepMeta}>
          {state && <span className={STATE_CLASS[state]}>{STATE_LABEL[state]}</span>}
          {onTest && (
            <Button disabled={state === 'testing'} onClick={onTest}>
              {t.setup.test}
            </Button>
          )}
        </span>
      }
    />
  );
}

function ErrorCard({ message }: { message: string }) {
  return (
    <div className={styles.errorCard}>
      <div className={styles.errorBody}>{message}</div>
    </div>
  );
}

export default function Setup() {
  const configQuery = useConfig();
  const sharesQuery = useShares();
  const config = configQuery.data;
  const shares = sharesQuery.data;
  // config gates the page: every step below reads it, so there is no useful
  // partial render. shares feeds step 3 only and gates there.
  const configPhase = queryPhase(configQuery);
  const sharesPhase = queryPhase(sharesQuery);
  const soulseekTest = useTestConnection('soulseek');
  const lidarrTest = useTestConnection('lidarr');

  // Config only changes when the file changes (staleTime: Infinity), so
  // there is nothing meaningful to render before the first response — a
  // brief loading notice beats flashing every step as NOT ENABLED.
  if (!config) {
    return (
      <Page title={t.page.setup.title} subtitle={t.page.setup.subtitle} maxWidth={820}>
        <QueryNotice phase={configPhase} />
      </Page>
    );
  }

  const soulseekEnabled = config.soulseek.enabled;
  const soulseek = soulseekEnabled ? connectionState(soulseekTest) : { state: 'disabled' as StepState };
  const lidarr = connectionState(lidarrTest);
  const sharesState: StepState = (shares?.files ?? 0) > 0 ? 'ok' : 'untested';

  return (
    <Page title={t.page.setup.title} subtitle={t.page.setup.subtitle} maxWidth={820}>
      <div className={styles.header}>
        <div className={styles.title}>{t.setup.title}</div>
        <div className={styles.intro}>{t.setup.intro}</div>
      </div>

      <QueryNotice phase={configPhase} />

      {/* One Panel around all three steps, matching the mock's single
          bordered <section> (docs/design/slskdarr-tui.dc.html SETUP block)
          — each step's own border-top stays as the internal divider between
          them, the same way a table's row dividers stay untouched by this
          restyle. */}
      <Panel>
        <div className={styles.step}>
          <StepHeader
            num={1}
            title={t.setup.stepSoulseek}
            state={soulseek.state}
            onTest={soulseekEnabled ? () => soulseekTest.mutate() : undefined}
          />
          <div className={styles.fields}>
            <div className={styles.field}>
              <span className={styles.fieldKey}>{t.setup.fieldUsername}</span>
              <span className={styles.fieldValue}>{config.soulseek.username}</span>
            </div>
            <div className={styles.field}>
              <span className={styles.fieldKey}>{t.setup.fieldPassword}</span>
              <span className={styles.fieldValue}>{secretValue(config.soulseek.passwordConfigured)}</span>
            </div>
            {soulseek.state === 'failed' && soulseek.error && <ErrorCard message={soulseek.error} />}
          </div>
        </div>

        <div className={styles.step}>
          <StepHeader num={2} title={t.setup.stepLidarr} state={lidarr.state} onTest={() => lidarrTest.mutate()} />
          <div className={styles.fields}>
            <div className={styles.field}>
              <span className={styles.fieldKey}>{t.setup.fieldUrl}</span>
              <span className={styles.fieldValue}>{config.lidarr.url}</span>
            </div>
            <div className={styles.field}>
              <span className={styles.fieldKey}>{t.setup.fieldApiKey}</span>
              <span className={styles.fieldValue}>{secretValue(config.lidarr.apiKeyConfigured)}</span>
            </div>
            {lidarr.state === 'failed' && lidarr.error && <ErrorCard message={lidarr.error} />}
          </div>
        </div>

        <div className={styles.step}>
          <StepHeader num={3} title={t.setup.stepShares} state={hasData(sharesPhase) ? sharesState : undefined} />
          <QueryNotice phase={sharesPhase} />
          <div className={styles.fields}>
            <div className={styles.field}>
              <span className={styles.fieldKey}>{t.setup.fieldFolders}</span>
              <span className={styles.fieldValue}>{t.setup.foldersCount(config.soulseek.sharedFolders.length)}</span>
            </div>
            <div className={styles.field}>
              <span className={styles.fieldKey}>{t.setup.fieldIndex}</span>
              <span className={styles.fieldValue}>
                {hasData(sharesPhase) ? t.setup.indexCount(shares?.files ?? 0) : '—'}
              </span>
            </div>
            <div className={styles.field}>
              <span className={styles.fieldValue}>{t.setup.sharesNoTest}</span>
            </div>
          </div>
        </div>
      </Panel>
    </Page>
  );
}
