import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { restartPollInterval, watchRestart } from './restartWatch';

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function handlers() {
  return {
    onRecovered: vi.fn(),
    onAuthChanged: vi.fn(),
    onTimeout: vi.fn(),
  };
}

describe('watchRestart without a baseline instance id', () => {
  // The save response always carries one today, so this is the degraded path:
  // with no identity to compare, an observed down window is the only honest
  // evidence that the process actually restarted. Settings.test.tsx covers
  // the normal path end to end.
  it('ignores a healthy probe until it has seen the process go down', async () => {
    let reachable = true;
    const fetchMock = vi.fn((url: string) => {
      if (url === '/healthz') {
        return reachable
          ? Promise.resolve(new Response(JSON.stringify({ instance: '' }), { status: 200 }))
          : Promise.reject(new Error('connection refused'));
      }
      return Promise.resolve(new Response(JSON.stringify({ writable: true }), { status: 200 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const h = handlers();
    const controller = new AbortController();
    void watchRestart('', controller.signal, h);

    // Reachable throughout: this is the old process, not its replacement.
    await vi.advanceTimersByTimeAsync(restartPollInterval * 3);
    expect(h.onRecovered).not.toHaveBeenCalled();

    reachable = false;
    await vi.advanceTimersByTimeAsync(restartPollInterval);
    expect(h.onRecovered).not.toHaveBeenCalled();

    reachable = true;
    await vi.advanceTimersByTimeAsync(restartPollInterval);
    await vi.advanceTimersByTimeAsync(0);
    expect(h.onRecovered).toHaveBeenCalledTimes(1);
    expect(h.onTimeout).not.toHaveBeenCalled();

    controller.abort();
  });
});

describe('watchRestart', () => {
  it('keeps probing when the recovered process is reachable but not yet serving config', async () => {
    let configStatus = 503;
    const fetchMock = vi.fn((url: string) => {
      if (url === '/healthz') {
        return Promise.resolve(new Response(JSON.stringify({ instance: 'after' }), { status: 200 }));
      }
      return configStatus === 200
        ? Promise.resolve(new Response(JSON.stringify({ writable: true }), { status: 200 }))
        : Promise.resolve(new Response('starting', { status: configStatus }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const h = handlers();
    const controller = new AbortController();
    void watchRestart('before', controller.signal, h);

    await vi.advanceTimersByTimeAsync(restartPollInterval * 2);
    expect(h.onRecovered).not.toHaveBeenCalled();
    // A 503 is not an auth failure and must not be reported as one.
    expect(h.onAuthChanged).not.toHaveBeenCalled();

    configStatus = 200;
    await vi.advanceTimersByTimeAsync(restartPollInterval);
    await vi.advanceTimersByTimeAsync(0);
    expect(h.onRecovered).toHaveBeenCalledTimes(1);

    controller.abort();
  });
});
