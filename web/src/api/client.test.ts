import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet, apiPost } from './client';

afterEach(() => vi.unstubAllGlobals());

describe('apiGet', () => {
  it('returns parsed JSON on success', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify([{ id: 1 }]), { status: 200 }),
    ));
    await expect(apiGet('/api/jobs')).resolves.toEqual([{ id: 1 }]);
  });

  it('throws ApiError carrying the status on failure', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('nope', { status: 500 })));
    await expect(apiGet('/api/jobs')).rejects.toBeInstanceOf(ApiError);
  });
});

describe('apiPost', () => {
  it('resolves on 204', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(apiPost('/api/jobs/1/cancel')).resolves.toBeUndefined();
  });

  // The legacy dashboard ignored failures here entirely; we surface them.
  it('throws ApiError with the status on 409', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('conflict', { status: 409 })));
    await expect(apiPost('/api/jobs/1/retry')).rejects.toMatchObject({ status: 409 });
  });
});
