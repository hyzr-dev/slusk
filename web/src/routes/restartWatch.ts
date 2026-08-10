// The settings view's restart window (issue #154). Saving settings makes the
// backend exit ~250ms after it flushes the response and lets the container's
// restart policy bring it back, so between the save and the recovery there is
// a real gap in which every request fails — and, at the very start of it, a
// short stretch where the *dying* server still answers.
//
// This module owns the wait: a non-overlapping poll of /healthz that treats
// the process instance id as the identity of the run (see ServerDeps.
// InstanceID), so "answered" is not confused with "restarted".

import { ApiError, apiGet } from '../api/client';
import type { AppConfig, HealthResult } from '../api/types';

/**
 * Where the settings view is in the restart window. `waiting` is the only
 * phase that resolves on its own; `timedOut` and `authChanged` are terminal
 * and carry guidance, because in both cases nothing this page can do will
 * bring the connection back.
 */
export type RestartPhase = 'idle' | 'waiting' | 'timedOut' | 'authChanged';

/** How long to wait between probes. */
export const restartPollInterval = 1000;

/**
 * How long to keep probing before giving up. Generous on purpose: a restart
 * re-runs the whole startup path (Postgres, migrations, Soulseek login, whose
 * own budget is startupTimeout = 30s in cmd/slusk) and the container may back
 * off before retrying. Anything past this is not slow, it is broken — and a
 * spinner that never ends is worse than saying so.
 */
export const restartTimeout = 120_000;

/** What watchRestart reports back; exactly one of these is ever called. */
export interface RestartHandlers {
  /** The replacement process is up and served its configuration. */
  onRecovered: (fresh: AppConfig) => void;
  /** It came back, but this session can no longer authenticate against it. */
  onAuthChanged: () => void;
  /** It never came back within restartTimeout. */
  onTimeout: () => void;
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const id = setTimeout(resolve, ms);
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(id);
        resolve();
      },
      { once: true },
    );
  });
}

/**
 * Polls until the process identified by `baseline` has been replaced, then
 * fetches the new process's configuration. Runs one request at a time — the
 * next probe is scheduled only once the previous one has settled — and stops
 * on `signal`, which is how the caller cleans up on unmount.
 *
 * `baseline` is the instance id POST /api/config reported for the process
 * that served the save. An empty string means the save answered without one,
 * and the poll then falls back to requiring an observed *failed* probe before
 * accepting any success: without an identity to compare, a down window is the
 * only honest evidence that a restart actually happened.
 */
export async function watchRestart(
  baseline: string,
  signal: AbortSignal,
  handlers: RestartHandlers,
): Promise<void> {
  const deadline = Date.now() + restartTimeout;
  let sawDown = baseline !== '';

  while (!signal.aborted) {
    // Wait before the first probe, not after: the old server is still
    // answering at this point, and probing it immediately only ever produces
    // the baseline id back.
    await delay(restartPollInterval, signal);
    if (signal.aborted) return;
    if (Date.now() >= deadline) {
      handlers.onTimeout();
      return;
    }

    let instance: string;
    try {
      instance = (await apiGet<HealthResult>('/healthz', signal)).instance;
    } catch {
      if (signal.aborted) return;
      // A failed probe is the expected middle of a restart, not an error to
      // report: the process is down, so whatever answers next is the new one.
      sawDown = true;
      continue;
    }

    if (baseline !== '' && instance === baseline) continue; // still the dying process
    if (!sawDown) continue; // no baseline to compare against; wait for the gap

    try {
      const fresh = await apiGet<AppConfig>('/api/config', signal);
      handlers.onRecovered(fresh);
      return;
    } catch (err) {
      if (signal.aborted) return;
      if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
        // /healthz is public, so it recovers even when the saved change
        // invalidated this session's credentials — which is precisely how
        // that case becomes distinguishable from a backend that never
        // came back.
        handlers.onAuthChanged();
        return;
      }
      // Reachable but not serving configuration yet. Keep probing.
    }
  }
}
