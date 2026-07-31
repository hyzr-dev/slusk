// The manual-search half of queries.ts (issue #58), mirroring
// queries.live.test.tsx's shape for its `live` counterpart: the pure merge
// functions first, then the hook whose fallback poll depends on them.
import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mergeSearchSession, queryKeys, replaceSearchGroups, useSearchSession } from './queries';
import type { SearchPayload, SearchSession, WireSearchGroup } from './types';

afterEach(() => vi.unstubAllGlobals());

const SESSION_ID = 'deadbeefdeadbeefdeadbeefdeadbeef';

function makeGroup(overrides: Partial<WireSearchGroup> = {}): WireSearchGroup {
  return {
    id: 'g1',
    peer: 'lossless_lars',
    folder: '@@abc/Music/Radiohead/In Rainbows',
    title: 'In Rainbows',
    parent: 'Radiohead',
    trackCount: 2,
    sizeBytes: 90_000_000,
    format: 'flac',
    freeUploadSlot: true,
    queueLength: 0,
    uploadSpeed: 940_000,
    score: 0.9,
    files: [
      { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\01.flac', name: '01.flac', size: 30_000_000 },
    ],
    ...overrides,
  };
}

function makeSession(overrides: Partial<SearchSession> = {}): SearchSession {
  return {
    id: SESSION_ID,
    query: 'radiohead in rainbows',
    startedAt: '2026-01-01T00:00:00Z',
    done: false,
    streaming: true,
    total: 1,
    groups: [makeGroup()],
    ...overrides,
  };
}

function makeFrame(overrides: Partial<SearchPayload> = {}): SearchPayload {
  return {
    id: SESSION_ID,
    seq: 1,
    groups: [makeGroup()],
    total: 1,
    done: false,
    streaming: true,
    ...overrides,
  };
}

describe('replaceSearchGroups', () => {
  // The base case the happy path in stream.test.tsx already covers, restated
  // here so the branches below read against something.
  it('unions new groups onto the cached ones by id', () => {
    const prev = makeSession({ groups: [makeGroup({ id: 'a' })], total: 1 });
    const next = replaceSearchGroups(prev, makeFrame({ groups: [makeGroup({ id: 'b' })], total: 2 }));
    expect(next?.groups.map((g) => g.id)).toEqual(['a', 'b']);
    expect(next?.total).toBe(2);
  });

  // A frame can resend a group whole when any file inside it changed (see
  // WireSearchGroup's doc comment) — that must replace the cached copy in
  // place, not append a duplicate.
  it('replaces a resent group rather than duplicating it', () => {
    const prev = makeSession({ groups: [makeGroup({ id: 'a', trackCount: 2 })] });
    const next = replaceSearchGroups(prev, makeFrame({ groups: [makeGroup({ id: 'a', trackCount: 9 })] }));
    expect(next?.groups).toHaveLength(1);
    expect(next?.groups[0].trackCount).toBe(9);
  });

  // Nothing has been fetched for this id yet. Fabricating a session to hold
  // the frame would invent every field the frame does not carry (query,
  // startedAt); the imminent REST response carries them for real.
  it('ignores a frame for an id nothing has cached yet', () => {
    expect(replaceSearchGroups(undefined, makeFrame())).toBeUndefined();
  });

  // The stale-frame guard: the connection reopens on every new search, but a
  // frame already in flight when it does can still land afterwards. Folding
  // it in would splice the previous search's results into the new one.
  it('ignores a frame whose id is not the cached session', () => {
    const prev = makeSession({ id: 'aaaa', groups: [makeGroup({ id: 'a' })] });
    const next = replaceSearchGroups(prev, makeFrame({ id: 'bbbb', groups: [makeGroup({ id: 'b' })] }));
    expect(next).toBe(prev);
  });

  // `expired` arrives with every field but `id` at its zero value, so folding
  // it in like an ordinary frame would blank the results and reset total to 0.
  it('marks an expired session done and expired while keeping its groups', () => {
    const prev = makeSession({ groups: [makeGroup({ id: 'a' })], total: 7 });
    const next = replaceSearchGroups(prev, makeFrame({ expired: true, groups: undefined, total: 0, done: false }));
    expect(next?.done).toBe(true);
    expect(next?.expired).toBe(true);
    expect(next?.groups.map((g) => g.id)).toEqual(['a']);
    expect(next?.total).toBe(7);
  });

  // Idempotent: a second expired frame (a reconnect landing on the same dead
  // id) must return the identical object, or every arrival re-renders.
  it('returns the same object for a repeated expired frame', () => {
    const prev = makeSession();
    const once = replaceSearchGroups(prev, makeFrame({ expired: true }));
    const twice = replaceSearchGroups(once, makeFrame({ expired: true }));
    expect(twice).toBe(once);
  });

  // `done` follows the frame rather than being latched — a session that has
  // genuinely finished must not be reported as still running.
  it('adopts the frame\'s done flag', () => {
    const next = replaceSearchGroups(makeSession({ done: false }), makeFrame({ done: true }));
    expect(next?.done).toBe(true);
  });

  it('stamps streamedAt so the fallback poll can see the frame arrived', () => {
    const before = Date.now();
    const next = replaceSearchGroups(makeSession(), makeFrame());
    expect(next?.streamedAt).toBeGreaterThanOrEqual(before);
  });
});

describe('mergeSearchSession', () => {
  // The regression this function exists for: a REST snapshot computed at T1
  // landing after a stream frame folded in at T2 > T1. The server's
  // per-subscriber cursor has already advanced past that group, so a plain
  // replace would drop it and never see it resent.
  it('keeps a group the stream added that the fetched snapshot predates', () => {
    const cached = makeSession({ groups: [makeGroup({ id: 'a' }), makeGroup({ id: 'streamed' })], total: 2 });
    const fetched = makeSession({ groups: [makeGroup({ id: 'a' })], total: 1 });
    const merged = mergeSearchSession(cached, fetched);
    expect(merged.groups.map((g) => g.id)).toEqual(['a', 'streamed']);
  });

  it('adopts a group only REST has seen', () => {
    const cached = makeSession({ groups: [makeGroup({ id: 'a' })] });
    const fetched = makeSession({ groups: [makeGroup({ id: 'a' }), makeGroup({ id: 'b' })], total: 2 });
    expect(mergeSearchSession(cached, fetched).groups.map((g) => g.id)).toEqual(['a', 'b']);
  });

  // The monotonic scalars. A stale snapshot must not un-finish, un-expire or
  // un-truncate a session, nor walk `total` backwards.
  it('never regresses done, expired, truncated or total', () => {
    const cached = makeSession({ done: true, expired: true, truncated: true, total: 9 });
    const fetched = makeSession({ done: false, truncated: false, total: 4 });
    const merged = mergeSearchSession(cached, fetched);
    expect(merged).toMatchObject({ done: true, expired: true, truncated: true, total: 9 });
  });

  // A REST fetch says nothing about when the stream last spoke, so it must
  // not reset the staleness clock the fallback poll arms on — otherwise every
  // poll would silence the next one and the poll would stall itself.
  it('carries streamedAt across untouched', () => {
    const cached = makeSession({ streamedAt: 1234 });
    expect(mergeSearchSession(cached, makeSession()).streamedAt).toBe(1234);
  });

  it('takes the fetched session whole when nothing is cached', () => {
    const fetched = makeSession();
    expect(mergeSearchSession(undefined, fetched)).toBe(fetched);
  });

  // A cache entry for a different id is not a base to merge onto — this is
  // the same class of guard as replaceSearchGroups' id check.
  it('takes the fetched session whole when the cached id differs', () => {
    const fetched = makeSession({ id: 'bbbb' });
    expect(mergeSearchSession(makeSession({ id: 'aaaa' }), fetched)).toBe(fetched);
  });
});

function makeWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

function newClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

// The refetchInterval predicate — the streaming/live/done/staleness matrix.
// Measured as fetch counts rather than by reading the predicate directly,
// because what matters is whether a GET actually happens.
describe('useSearchSession fallback poll', () => {
  afterEach(() => vi.useRealTimers());

  function stubSessionFetch(session: SearchSession) {
    const fetchMock = vi.fn(() => Promise.resolve({ ok: true, json: () => Promise.resolve(session) } as Response));
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
  }

  // THE HOLE. `live` is undefined — not null — until an `event: live` frame
  // arrives, and on /search with nothing downloading none ever does, because
  // the server only frames `live` for jobs that are actually live. A
  // `live !== null` guard therefore reads as "the stream is healthy" forever
  // and the poll never arms, leaving an open-but-silent connection (a hub
  // bug, a dropped sendLatestSearch, a buffering proxy) with no safety net at
  // all: the view sits on the 201 snapshot showing "streaming in…" for good.
  it('polls a streaming session that no frame has ever reached, with live still undefined', async () => {
    vi.useFakeTimers();
    const fetchMock = stubSessionFetch(makeSession({ done: false, streaming: true }));
    const queryClient = newClient();

    renderHook(() => useSearchSession(SESSION_ID), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(queryClient.getQueryData(queryKeys.live)).toBeUndefined();

    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });

  // The mirror image, and why the fix is staleness rather than just widening
  // the null check to `== null`: while frames genuinely keep arriving, the
  // poll must stay off — that is the whole point of it being a fallback.
  it('stays quiet while search frames keep arriving', async () => {
    vi.useFakeTimers();
    const fetchMock = stubSessionFetch(makeSession({ done: false, streaming: true }));
    const queryClient = newClient();

    renderHook(() => useSearchSession(SESSION_ID), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // A frame every 2s for 10s — comfortably inside SEARCH_STREAM_STALE_MS.
    for (let i = 0; i < 5; i++) {
      act(() => {
        queryClient.setQueryData<SearchSession>(queryKeys.search(SESSION_ID), (prev) =>
          replaceSearchGroups(prev, makeFrame({ seq: i, groups: [makeGroup({ id: `g${i}` })], total: i + 1 })),
        );
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    }
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // ...and then they stop. The connection is still nominally fine; only
    // silence says otherwise.
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });

  // The error path, which was already covered: clearLive writes null, and
  // that arms the poll at once rather than after the staleness window.
  it('polls immediately once the stream errors, even just after a frame', async () => {
    vi.useFakeTimers();
    const fetchMock = stubSessionFetch(makeSession({ done: false, streaming: true }));
    const queryClient = newClient();

    const { rerender } = renderHook(() => useSearchSession(SESSION_ID), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });

    act(() => {
      queryClient.setQueryData<SearchSession>(queryKeys.search(SESSION_ID), (prev) =>
        replaceSearchGroups(prev, makeFrame()),
      );
      queryClient.setQueryData(queryKeys.live, null);
    });
    rerender();

    await act(async () => { await vi.advanceTimersByTimeAsync(3_500); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });

  // A batching backend (slskd) sends no incremental frames at all, so the
  // poll is its only freshness mechanism — not a fallback.
  it('polls a non-streaming session regardless of frames', async () => {
    vi.useFakeTimers();
    const fetchMock = stubSessionFetch(makeSession({ done: false, streaming: false }));
    const queryClient = newClient();

    renderHook(() => useSearchSession(SESSION_ID), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    await act(async () => { await vi.advanceTimersByTimeAsync(3_500); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });

  it('stops polling once the session is done', async () => {
    vi.useFakeTimers();
    const fetchMock = stubSessionFetch(makeSession({ done: true, streaming: false }));
    const queryClient = newClient();

    renderHook(() => useSearchSession(SESSION_ID), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // Cache coherence end to end: the poll and the stream are both live (the
  // scenario a blip-then-auto-reconnect produces), and the GET answers with a
  // snapshot older than the last frame. The streamed group must survive.
  it('does not drop a streamed group when a stale REST snapshot lands', async () => {
    vi.useFakeTimers();
    // The server's snapshot knows only 'a'.
    const fetchMock = stubSessionFetch(makeSession({ groups: [makeGroup({ id: 'a' })], total: 1 }));
    const queryClient = newClient();

    const { result, rerender } = renderHook(() => useSearchSession(SESSION_ID), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(result.current.data?.groups.map((g) => g.id)).toEqual(['a']);

    // A frame adds 'streamed'; the server's cursor has now advanced past it.
    act(() => {
      queryClient.setQueryData<SearchSession>(queryKeys.search(SESSION_ID), (prev) =>
        replaceSearchGroups(prev, makeFrame({ groups: [makeGroup({ id: 'streamed' })], total: 2 })),
      );
    });
    rerender();
    expect(result.current.data?.groups.map((g) => g.id)).toEqual(['a', 'streamed']);

    // The fallback poll fires and re-fetches that same older snapshot.
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
    rerender();
    expect(result.current.data?.groups.map((g) => g.id)).toEqual(['a', 'streamed']);
  });
});
