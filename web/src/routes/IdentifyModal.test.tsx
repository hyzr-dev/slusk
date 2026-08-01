import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type {
  LidarrAddOptions,
  LidarrArtistMatch,
  LidarrMatch,
  MusicBrainzEdition,
  MusicBrainzEditionListResult,
  MusicBrainzSearchResponse,
  MusicBrainzSearchResult,
  SearchGroup,
} from '../api/types';
import { t } from '../strings';
import IdentifyModal, {
  computeVerdict,
  lidarrAddAvailability,
  lidarrLine,
  parseFolderGuess,
  pickDefaultEdition,
} from './IdentifyModal';

afterEach(() => vi.unstubAllGlobals());

function group(overrides: Partial<SearchGroup> = {}): SearchGroup {
  return {
    id: 'g1',
    peer: 'lossless_lars',
    folder: '@@abc/Music/Radiohead/In Rainbows',
    title: 'Radiohead - In Rainbows [FLAC]',
    parent: 'Radiohead',
    trackCount: 10,
    sizeBytes: 90000000,
    format: 'flac',
    freeUploadSlot: true,
    queueLength: 0,
    uploadSpeed: 940000,
    score: 0.9,
    files: [
      { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\01.flac', name: '01.flac', size: 30000000 },
    ],
    ...overrides,
  };
}

function edition(overrides: Partial<MusicBrainzEdition> = {}): MusicBrainzEdition {
  return {
    id: 'e1',
    title: 'In Rainbows',
    date: '2007-10-10',
    country: 'GB',
    status: 'Official',
    trackCount: 10,
    trackCountKnown: true,
    ...overrides,
  };
}

function searchResult(overrides: Partial<MusicBrainzSearchResult> = {}): MusicBrainzSearchResult {
  return {
    id: 'al1',
    title: 'In Rainbows',
    artist: 'Radiohead',
    artistId: 'a1',
    firstReleaseDate: '2007-10-10',
    primaryType: 'Album',
    secondaryTypes: [],
    editionCount: 3,
    score: 100,
    ...overrides,
  };
}

function renderModal(g: SearchGroup, onConfirm = vi.fn(), onClose = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <IdentifyModal group={g} onClose={onClose} onConfirm={onConfirm} />
    </QueryClientProvider>,
  );
  return { ...utils, onConfirm, onClose };
}

// A GET-router keyed by exact path, mirroring the pattern in
// Search.test.tsx and queries.search.test.tsx.
function stubFetch(routes: Record<string, unknown | (() => unknown)>) {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const match = Object.keys(routes).find((k) => url.startsWith(k));
      if (!match) return Promise.reject(new Error(`unexpected fetch ${url}`));
      const value = routes[match];
      const body = typeof value === 'function' ? (value as () => unknown)() : value;
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    }),
  );
}

// A method-aware router, needed once GET /api/lidarr/artists/{mbid} and
// POST /api/lidarr/artists share a common URL prefix — the plain stubFetch
// above would let the POST route's key wrongly swallow the GET's more
// specific one (or vice versa) depending on object key iteration order.
function stubFetchByMethod(routes: { method: string; match: string; body: unknown | (() => unknown); status?: number }[]) {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const route = routes.find((r) => r.method === method && url.startsWith(r.match));
      if (!route) return Promise.reject(new Error(`unexpected fetch ${method} ${url}`));
      const body = typeof route.body === 'function' ? (route.body as () => unknown)() : route.body;
      return Promise.resolve(new Response(JSON.stringify(body), { status: route.status ?? 200 }));
    }),
  );
}

function artistMatch(overrides: Partial<LidarrArtistMatch> = {}): LidarrArtistMatch {
  return { known: true, inLibrary: false, artistId: 42, name: 'Radiohead', ...overrides };
}

function addOptions(overrides: Partial<LidarrAddOptions> = {}): LidarrAddOptions {
  return {
    rootFolders: [
      { id: 1, path: '/music', accessible: true, freeSpace: 500_000_000_000, defaultQualityProfileId: 3, defaultMetadataProfileId: 5 },
    ],
    qualityProfiles: [{ id: 3, name: 'Lossless' }, { id: 4, name: 'Standard' }],
    metadataProfiles: [{ id: 5, name: 'Standard' }, { id: 6, name: 'None' }],
    ...overrides,
  };
}

// Drives the modal from initial through to the 'selected' state with the
// given album/artist Lidarr status stubbed, and returns once the "WILL BE
// RECORDED AS" summary is on screen. Shared setup for the #331 tests below.
async function selectWithLidarrStatus(
  album: LidarrMatch,
  artist: LidarrArtistMatch | undefined,
  opts: {
    searchResult?: MusicBrainzSearchResult;
    addOptionsRoute?: LidarrAddOptions;
    extraRoutes?: { method: string; match: string; body: unknown | (() => unknown); status?: number }[];
  } = {},
) {
  const search: MusicBrainzSearchResponse = { results: [opts.searchResult ?? searchResult()], total: 1 };
  const routes = [
    ...(opts.extraRoutes ?? []),
    { method: 'GET', match: '/api/identify/search?', body: search },
    { method: 'GET', match: '/api/identify/albums/al1/editions', body: { editions: [], total: 0 } },
    { method: 'GET', match: '/api/identify/albums/al1/lidarr', body: album },
    { method: 'GET', match: '/api/lidarr/artists/a1', body: artist ?? { known: false, inLibrary: false } },
    { method: 'GET', match: '/api/lidarr/add-options', body: opts.addOptionsRoute ?? addOptions() },
  ];
  stubFetchByMethod(routes);
  const utils = renderModal(group());
  fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
  await waitFor(() => screen.getByRole('button', { name: /In Rainbows/ }));
  fireEvent.click(screen.getByRole('button', { name: /In Rainbows/ }));
  await waitFor(() => screen.getByText(t.search.identify.willBeRecordedAs));
  return utils;
}

describe('parseFolderGuess', () => {
  it('splits "Artist - Album" and strips a trailing format tag', () => {
    expect(parseFolderGuess(group({ title: 'Metallica - Ride the Lightning [FLAC]', parent: 'irrelevant' }))).toEqual({
      artist: 'Metallica',
      album: 'Ride the Lightning',
    });
  });

  it('strips more than one trailing tag', () => {
    expect(parseFolderGuess(group({ title: 'Portishead - Dummy (1994) [FLAC 16/44]' }))).toEqual({
      artist: 'Portishead',
      album: 'Dummy',
    });
  });

  // The brief's own ugly real examples: a folder with no "Artist - Album"
  // separator falls back to the peer's parent directory as the artist guess.
  it('falls back to the parent directory when the title has no separator (Soulseek - Share)', () => {
    expect(parseFolderGuess(group({ title: 'Ride the Lightning', parent: 'Soulseek - Share' }))).toEqual({
      artist: 'Soulseek - Share',
      album: 'Ride the Lightning',
    });
  });

  it('falls back to the parent directory (music/_emily/)', () => {
    expect(parseFolderGuess(group({ title: 'Some Album [MP3]', parent: 'music/_emily/' }))).toEqual({
      artist: 'music/_emily/',
      album: 'Some Album',
    });
  });

  it('falls back to the parent directory (Various Artists)', () => {
    expect(parseFolderGuess(group({ title: 'Kind of Blue', parent: 'Various Artists' }))).toEqual({
      artist: 'Various Artists',
      album: 'Kind of Blue',
    });
  });

  // #355: "Artist/YYYY - Album" is a common peer layout too, and the naive
  // dash split mistook the year for the artist. The parent directory is
  // preferred over a bare 4-digit year, same as the no-separator fallback.
  it('prefers the parent directory over a leading year (Artist/YYYY - Album)', () => {
    expect(parseFolderGuess(group({ title: '1984 - Ride The Lightning', parent: 'Metallica' }))).toEqual({
      artist: 'Metallica',
      album: 'Ride The Lightning',
    });
  });

  it('prefers the parent directory over a leading year, tags and all', () => {
    expect(parseFolderGuess(group({ title: '1994 - Dummy [FLAC 16/44]', parent: 'Portishead' }))).toEqual({
      artist: 'Portishead',
      album: 'Dummy',
    });
  });

  it('keeps the year as artist when there is no parent to fall back on', () => {
    expect(parseFolderGuess(group({ title: '1984 - Ride The Lightning', parent: '' }))).toEqual({
      artist: '1984',
      album: 'Ride The Lightning',
    });
  });

  // #355 follow-up: path.Dir of a slashless path returns "." (not ""), which
  // is what the server sends when a peer shares the release folder at the
  // top of their share with no artist directory above it. "." is just as
  // useless an artist guess as the year it would replace.
  it('keeps the year as artist when the peer shares the album at the share root (parent ".")', () => {
    expect(parseFolderGuess(group({ title: '1984 - Ride The Lightning', parent: '.' }))).toEqual({
      artist: '1984',
      album: 'Ride The Lightning',
    });
  });

  it('uses the parent even when it looks unhelpful, same as the no-separator case', () => {
    expect(parseFolderGuess(group({ title: '1984 - Ride The Lightning', parent: 'Soulseek - Share' }))).toEqual({
      artist: 'Soulseek - Share',
      album: 'Ride The Lightning',
    });
  });
});

describe('pickDefaultEdition', () => {
  it('prefers an edition whose known track count matches the folder exactly', () => {
    const editions = [edition({ id: 'a', trackCount: 19 }), edition({ id: 'b', trackCount: 10 })];
    expect(pickDefaultEdition(editions, 10)?.id).toBe('b');
  });

  it('falls back to an Official edition when nothing matches exactly', () => {
    const editions = [
      edition({ id: 'a', trackCount: 19, status: 'Promotion' }),
      edition({ id: 'b', trackCount: 12, status: 'Official' }),
    ];
    expect(pickDefaultEdition(editions, 10)?.id).toBe('b');
  });

  it('falls back to the first edition when nothing else qualifies', () => {
    const editions = [edition({ id: 'a', trackCount: 19, status: 'Promotion' })];
    expect(pickDefaultEdition(editions, 10)?.id).toBe('a');
  });

  it('returns undefined for an empty list', () => {
    expect(pickDefaultEdition([], 10)).toBeUndefined();
  });
});

describe('computeVerdict', () => {
  it('is COMPLETE when the folder matches the selected edition exactly', () => {
    expect(computeVerdict(10, edition({ trackCount: 10 })).tone).toBe('ok');
  });

  it('is INCOMPLETE (bad) when the folder has fewer tracks than the edition', () => {
    const v = computeVerdict(8, edition({ trackCount: 10 }));
    expect(v.tone).toBe('bad');
    expect(v.text).toContain('8');
  });

  // Not an error — the brief is explicit that a folder with MORE tracks than
  // the selected edition (a different edition, or more than one release in
  // the folder) must render 'dim', not 'bad'.
  it('is not styled as an error when the folder has more tracks than the edition', () => {
    const v = computeVerdict(97, edition({ trackCount: 8 }));
    expect(v.tone).toBe('dim');
  });

  it('is unknown when the edition track count is unknown, and never shows 0', () => {
    const v = computeVerdict(10, edition({ trackCountKnown: false, trackCount: 0 }));
    expect(v.tone).toBe('quiet');
    expect(v.text).not.toContain('0');
    expect(v.text).toBe(t.search.identify.verdictUnknownEdition);
  });

  // Review item B: a DIFFERENT string from the one above — no edition is
  // selected at all here (empty editions list, or a failed editions fetch),
  // so "this edition has no track listing" would name a referent that
  // isn't there.
  it('is unknown, with different wording, when no edition is selected at all', () => {
    const v = computeVerdict(10, undefined);
    expect(v.tone).toBe('quiet');
    expect(v.text).toBe(t.search.identify.verdictNoEdition);
    expect(v.text).not.toBe(t.search.identify.verdictUnknownEdition);
  });
});

describe('lidarrLine', () => {
  const inLib = { known: true, inLibrary: true };
  const absent = { known: true, inLibrary: false };
  const unknown = { known: false, inLibrary: false };

  it('reads IN LIBRARY (ok) when the album is in the library', () => {
    expect(lidarrLine(inLib, inLib, true).tone).toBe('ok');
  });

  it('names artist AND album when both are absent — one line, not two', () => {
    const line = lidarrLine(absent, absent, true);
    expect(line.tone).toBe('quiet');
    expect(line.text).toBe(t.search.identify.lidarrArtistAndAlbumMissing);
  });

  it('names only the album when the artist is present but the album is not', () => {
    const line = lidarrLine(absent, inLib, true);
    expect(line.text).toBe(t.search.identify.lidarrAlbumMissing);
  });

  it('reads UNKNOWN when the album lookup is unknown — distinct from "not in library"', () => {
    expect(lidarrLine(unknown, inLib, true).text).toBe(t.search.identify.lidarrUnknown);
  });

  it('reads UNKNOWN when the album is absent and the ARTIST lookup failed — we cannot say which case it is', () => {
    expect(lidarrLine(absent, unknown, true).text).toBe(t.search.identify.lidarrUnknown);
  });

  it('still reads IN LIBRARY when the artist lookup failed but the album IS present', () => {
    // An album in the library implies its artist is too, so a failed artist
    // lookup must not downgrade a decisive album answer to UNKNOWN.
    expect(lidarrLine(inLib, unknown, true).tone).toBe('ok');
    expect(lidarrLine(inLib, undefined, true).tone).toBe('ok');
  });

  it('reads UNKNOWN when no match was fetched at all (e.g. the lookup failed)', () => {
    expect(lidarrLine(undefined, undefined, true).text).toBe(t.search.identify.lidarrUnknown);
  });

  it('judges on the album alone when there is no artist MBID to look up', () => {
    // No artistId means no artist lookup was ever made; reporting that
    // never-made lookup as UNKNOWN would hide a perfectly good album answer.
    expect(lidarrLine(absent, undefined, false).text).toBe(t.search.identify.lidarrAlbumMissing);
    expect(lidarrLine(inLib, undefined, false).tone).toBe('ok');
  });
});

describe('IdentifyModal states', () => {
  it('renders the initial state with guessed, editable fields and fires no request before the button is pressed', () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    renderModal(group());
    expect(screen.getByText(t.search.identify.identifyingFolder)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: t.search.identify.artistLabel })).toHaveValue('Radiohead');
    expect(screen.getByRole('textbox', { name: t.search.identify.albumLabel })).toHaveValue('In Rainbows');
    expect(screen.getByRole('button', { name: t.search.identify.searchButton })).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('does not fire until the album field has something in it (the endpoint 422s on a blank one)', () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    renderModal(group({ title: '[FLAC]', parent: '' })); // parses to an empty album guess too
    fireEvent.change(screen.getByRole('textbox', { name: t.search.identify.albumLabel }), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('goes through searching, into suggestions, and shows the EDITIONS column and the best-matches notice when total exceeds the array', async () => {
    const response: MusicBrainzSearchResponse = {
      results: [searchResult({ editionCount: 60 })],
      total: 412,
    };
    stubFetch({ '/api/identify/search?': response });
    renderModal(group());
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    expect(screen.getByText(t.search.identify.searching)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: /In Rainbows/ })).toBeInTheDocument());
    expect(screen.getByText(t.search.identify.editionCountShort(60))).toBeInTheDocument();
    expect(screen.getByText(t.search.identify.showingBestOf(1))).toBeInTheDocument();
  });

  it('sends both artist and album as query params, url-encoded', async () => {
    const fetchSpy = vi.fn((_url: string) =>
      Promise.resolve(new Response(JSON.stringify({ results: [], total: 0 } satisfies MusicBrainzSearchResponse), { status: 200 })),
    );
    vi.stubGlobal('fetch', fetchSpy);
    renderModal(group({ title: 'Sigur Rós - Ágætis byrjun', parent: 'x' }));
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => expect(fetchSpy).toHaveBeenCalled());
    const url = fetchSpy.mock.calls[0][0];
    expect(url).toContain('/api/identify/search?');
    // URLSearchParams encodes a space as '+', not '%20' — matching that
    // rather than encodeURIComponent's own escaping.
    const params = new URLSearchParams(url.slice(url.indexOf('?') + 1));
    expect(params.get('artist')).toBe('Sigur Rós');
    expect(params.get('album')).toBe('Ágætis byrjun');
  });

  it('shows NO MATCHES when the combined search returns nothing', async () => {
    stubFetch({ '/api/identify/search?': { results: [], total: 0 } satisfies MusicBrainzSearchResponse });
    renderModal(group());
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => expect(screen.getByText(t.search.identify.noMatchesTitle)).toBeInTheDocument());
    expect(screen.getByRole('button', { name: t.search.identify.searchAgain })).toBeInTheDocument();
  });

  it('shows MUSICBRAINZ UNAVAILABLE on a 503 and never blocks downloading (Retry is offered, not a dead end)', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('down', { status: 503 }))));
    renderModal(group());
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => expect(screen.getByText(t.search.identify.unavailableTitle)).toBeInTheDocument());
    expect(screen.getByRole('button', { name: t.search.identify.retry })).toBeInTheDocument();
  });

  it('reaches the selected state, defaults the edition, and shows the three verdict/Lidarr facts — never rendering an unknown track count as 0', async () => {
    const search: MusicBrainzSearchResponse = { results: [searchResult({ editionCount: 3 })], total: 1 };
    const editions: MusicBrainzEditionListResult = {
      editions: [
        edition({ id: 'e1', trackCount: 10, trackCountKnown: true }),
        edition({ id: 'e2', trackCountKnown: false, trackCount: 0, title: 'Unknown pressing' }),
      ],
      total: 25,
    };
    const lidarr: LidarrMatch = { known: true, inLibrary: true };
    stubFetch({
      '/api/identify/search?': search,
      '/api/identify/albums/al1/editions': editions,
      '/api/identify/albums/al1/lidarr': lidarr,
    });
    renderModal(group());
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => screen.getByRole('button', { name: /In Rainbows/ }));
    fireEvent.click(screen.getByRole('button', { name: /In Rainbows/ }));

    await waitFor(() => expect(screen.getByText(t.search.identify.willBeRecordedAs)).toBeInTheDocument());
    // The summary line's edition count comes straight from the search
    // result (editionCount: 3), NOT from the separate editions list total
    // (25) — those are deliberately different values in this test so a
    // mix-up between the two would fail it.
    expect(screen.getByText(t.search.identify.editionCount(3), { exact: false })).toBeInTheDocument();
    // Edition e1 (exact track match) is the default selection, so the
    // verdict reads COMPLETE against it.
    expect(screen.getByText(t.search.identify.verdictComplete(10))).toBeInTheDocument();
    expect(screen.getByText(t.search.identify.lidarrInLibrary)).toBeInTheDocument();
    // The PICKER's own truncation notice, from the editions list call (2 of 25).
    expect(screen.getByText(t.search.identify.showingOf(2, 25))).toBeInTheDocument();

    // The unknown-track-count edition must say so, never show "0 tracks".
    expect(screen.getByText(t.search.identify.editionUnknownTracks, { exact: false })).toBeInTheDocument();
    expect(screen.queryByText(/(^|\s)0 tracks/)).not.toBeInTheDocument();
  });

  // Review item I: computeVerdict is unit-tested and the picker renders, but
  // the WIRING between the two — the entire justification for adding a
  // picker the mock does not draw — was untested until now.
  it('recomputes the verdict when the user changes the edition picker selection (incomplete → complete)', async () => {
    const search: MusicBrainzSearchResponse = { results: [searchResult()], total: 1 };
    const editions: MusicBrainzEditionListResult = {
      editions: [
        edition({ id: 'e-incomplete', trackCount: 19, trackCountKnown: true, status: 'Deluxe' }),
        edition({ id: 'e-complete', trackCount: 10, trackCountKnown: true, status: 'Official' }),
      ],
      total: 2,
    };
    stubFetch({
      '/api/identify/search?': search,
      '/api/identify/albums/al1/editions': editions,
      '/api/identify/albums/al1/lidarr': { known: false, inLibrary: false },
    });
    renderModal(group()); // group()'s trackCount is 10
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => screen.getByRole('button', { name: /In Rainbows/ }));
    fireEvent.click(screen.getByRole('button', { name: /In Rainbows/ }));
    await waitFor(() => screen.getByText(t.search.identify.willBeRecordedAs));

    const select = screen.getByRole('combobox');
    fireEvent.change(select, { target: { value: 'e-incomplete' } });
    expect(screen.getByText(t.search.identify.verdictIncomplete(10, 19))).toBeInTheDocument();

    fireEvent.change(select, { target: { value: 'e-complete' } });
    expect(screen.getByText(t.search.identify.verdictComplete(10))).toBeInTheDocument();
    expect(screen.queryByText(t.search.identify.verdictIncomplete(10, 19))).not.toBeInTheDocument();
  });

  it('confirms with the canonical MusicBrainz artist/album, not the folder guess', async () => {
    const search: MusicBrainzSearchResponse = { results: [searchResult()], total: 1 };
    stubFetch({
      '/api/identify/search?': search,
      '/api/identify/albums/al1/editions': { editions: [], total: 0 },
      '/api/identify/albums/al1/lidarr': { known: false, inLibrary: false },
    });
    const { onConfirm } = renderModal(group({ title: 'music/_emily/ folder [MP3]', parent: 'music/_emily/' }));
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => screen.getByRole('button', { name: /In Rainbows/ }));
    fireEvent.click(screen.getByRole('button', { name: /In Rainbows/ }));
    await waitFor(() => screen.getByText(t.search.identify.willBeRecordedAs));
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.confirm }));
    expect(onConfirm).toHaveBeenCalledWith({ artist: 'Radiohead', album: 'In Rainbows' });
  });

  // MusicBrainzSearchResult.artist/artistId are `omitempty` on the Go DTO —
  // genuinely absent, not empty strings, for a release-group whose
  // artist-credit is itself empty. Neither the suggestion row nor the
  // confirm path may render/post "undefined" for that; see the doc comment
  // on MusicBrainzSearchResult and on IdentifyModal's canonicalArtistOf.
  it('renders and selects a result with no artist/artistId without crashing, and confirms with the artist FIELD rather than an invented or undefined value', async () => {
    const search: MusicBrainzSearchResponse = {
      results: [searchResult({ artist: undefined, artistId: undefined, title: 'Anonymous Compilation' })],
      total: 1,
    };
    stubFetch({
      '/api/identify/search?': search,
      '/api/identify/albums/al1/editions': { editions: [], total: 0 },
      '/api/identify/albums/al1/lidarr': { known: false, inLibrary: false },
    });
    // Deliberately distinct from group.parent, so a fallback to the WRONG
    // source (group.parent instead of the artist field) would be caught.
    const { onConfirm } = renderModal(group({ title: 'Guessed Artist - Some Album [FLAC]', parent: 'DifferentParent' }));

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => screen.getByRole('button', { name: /Anonymous Compilation/ }));
    expect(screen.queryByText('undefined')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /Anonymous Compilation/ }));
    await waitFor(() => screen.getByText(t.search.identify.willBeRecordedAs));
    // The summary line shows exactly what confirm() will send.
    expect(screen.getByText(/Guessed Artist/)).toBeInTheDocument();
    expect(screen.queryByText(/DifferentParent/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.confirm }));
    expect(onConfirm).toHaveBeenCalledWith({ artist: 'Guessed Artist', album: 'Anonymous Compilation' });
  });

  // Review item A, the important one. When MusicBrainz supplies no artist
  // AND the user has blanked the artist field, there is no honest canonical
  // artist — the OLD code fell back to group.parent (the peer's parent
  // directory) here, which is the exact folder-name guess #321 exists to
  // stop posting. CONFIRM must be disabled, not silently post that guess.
  it('disables CONFIRM and explains why when there is no honest canonical artist at all', async () => {
    const search: MusicBrainzSearchResponse = {
      results: [searchResult({ artist: undefined, artistId: undefined, title: 'Anonymous Compilation' })],
      total: 1,
    };
    stubFetch({
      '/api/identify/search?': search,
      '/api/identify/albums/al1/editions': { editions: [], total: 0 },
      '/api/identify/albums/al1/lidarr': { known: false, inLibrary: false },
    });
    const { onConfirm } = renderModal(group({ title: '[FLAC]', parent: 'Soulseek - Share' }));

    // Blank the artist field explicitly — parseFolderGuess would otherwise
    // have prefilled it from group.parent, which must NOT become the
    // fallback either (that is precisely the bug this disables).
    fireEvent.change(screen.getByRole('textbox', { name: t.search.identify.artistLabel }), { target: { value: '' } });
    fireEvent.change(screen.getByRole('textbox', { name: t.search.identify.albumLabel }), { target: { value: 'Anonymous Compilation' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => screen.getByRole('button', { name: /Anonymous Compilation/ }));
    fireEvent.click(screen.getByRole('button', { name: /Anonymous Compilation/ }));
    await waitFor(() => screen.getByText(t.search.identify.willBeRecordedAs));

    expect(screen.getByText(t.search.identify.noCanonicalArtist)).toBeInTheDocument();
    expect(screen.queryByText(/Soulseek - Share/)).not.toBeInTheDocument();
    const confirmButton = screen.getByRole('button', { name: t.search.identify.confirm });
    expect(confirmButton).toBeDisabled();

    fireEvent.click(confirmButton);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('closes on Escape and on a scrim mousedown+click, but not on a click inside the panel', () => {
    const { onClose, container } = renderModal(group());
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByText(t.search.identify.identifyingFolder));
    expect(onClose).toHaveBeenCalledTimes(1);

    const scrim = container.firstChild as HTMLElement;
    fireEvent.mouseDown(scrim);
    fireEvent.click(scrim);
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  // Review item C. jsdom does not compute a real click's target the way a
  // browser does (the nearest common ancestor of the mousedown and mouseup
  // targets), so this drives the two DOM events the same way the browser
  // itself would for that drag: mousedown fires on the actual element under
  // the cursor (the input), and the resulting click fires on the scrim —
  // exactly the sequence the reviewer probe-confirmed as a false close.
  it('does not close when a mousedown starts inside the panel (e.g. selecting text) even if the click lands on the scrim', () => {
    const { onClose, container } = renderModal(group());
    const artistInput = screen.getByRole('textbox', { name: t.search.identify.artistLabel });
    fireEvent.mouseDown(artistInput);
    const scrim = container.firstChild as HTMLElement;
    fireEvent.click(scrim);
    expect(onClose).not.toHaveBeenCalled();
  });

  it('gives the dialog the right ARIA role and label', () => {
    renderModal(group());
    expect(screen.getByRole('dialog', { name: t.search.identify.dialogLabel })).toBeInTheDocument();
  });
});

describe('lidarrAddAvailability', () => {
  it('is noArtistId when the result has no artistId, regardless of lookup results', () => {
    expect(lidarrAddAvailability(false, { known: true, inLibrary: false }, { known: true, inLibrary: false })).toBe('noArtistId');
  });

  it('is unknown when the album lookup failed', () => {
    expect(lidarrAddAvailability(true, { known: false, inLibrary: false }, { known: true, inLibrary: false })).toBe('unknown');
  });

  it('is unknown when the artist lookup failed', () => {
    expect(lidarrAddAvailability(true, { known: true, inLibrary: false }, { known: false, inLibrary: false })).toBe('unknown');
  });

  it('is unknown when the artist lookup never resolved at all', () => {
    expect(lidarrAddAvailability(true, { known: true, inLibrary: false }, undefined)).toBe('unknown');
  });

  it('is inLibrary when the album is already in the library', () => {
    expect(lidarrAddAvailability(true, { known: true, inLibrary: true }, { known: true, inLibrary: true })).toBe('inLibrary');
  });

  it('is offerAdd when both lookups succeeded and the album is not in the library', () => {
    expect(lidarrAddAvailability(true, { known: true, inLibrary: false }, { known: true, inLibrary: false })).toBe('offerAdd');
  });
});

describe('IdentifyModal — Lidarr add-artist flow (#331)', () => {
  it('states the library situation on ONE line when both artist and album are present', async () => {
    await selectWithLidarrStatus({ known: true, inLibrary: true }, artistMatch({ inLibrary: true }));
    expect(screen.getByText(t.search.identify.lidarrInLibrary)).toBeInTheDocument();
  });

  // The redundancy this replaced: "ARTIST NOT IN LIDARR LIBRARY" rendered
  // directly above "NOT IN LIDARR LIBRARY — …", two lines for one fact.
  it('states artist AND album absence on one line, not two', async () => {
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch({ inLibrary: false }));
    expect(screen.getByText(t.search.identify.lidarrArtistAndAlbumMissing)).toBeInTheDocument();
    expect(screen.queryByText(t.search.identify.lidarrAlbumMissing)).not.toBeInTheDocument();
  });

  it('names only the album when the artist is already in Lidarr', async () => {
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch({ inLibrary: true }));
    expect(screen.getByText(t.search.identify.lidarrAlbumMissing)).toBeInTheDocument();
  });

  it('hides the add path and shows the "no artist ID" explanation when the result has no artistId', async () => {
    await selectWithLidarrStatus(
      { known: true, inLibrary: false },
      undefined,
      { searchResult: searchResult({ artistId: undefined }) },
    );
    expect(screen.getByText(t.search.identify.noArtistId)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.search.identify.addToLidarr })).not.toBeInTheDocument();
    // The ordinary single Confirm button remains the only way forward.
    expect(screen.getByRole('button', { name: t.search.identify.confirm })).toBeInTheDocument();
    // The never-made artist lookup must not turn the line UNKNOWN — the
    // album answer we DO have is reported.
    expect(screen.queryByText(t.search.identify.lidarrUnknown)).not.toBeInTheDocument();
    expect(screen.getByText(t.search.identify.lidarrAlbumMissing)).toBeInTheDocument();
  });

  it('hides the add path when the album Lidarr status is unknown (known: false)', async () => {
    await selectWithLidarrStatus({ known: false, inLibrary: false }, artistMatch());
    expect(screen.queryByRole('button', { name: t.search.identify.addToLidarr })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.search.identify.confirm })).toBeInTheDocument();
  });

  it('hides the add path when the artist Lidarr status is unknown even if the album status is known', async () => {
    await selectWithLidarrStatus({ known: true, inLibrary: false }, { known: false, inLibrary: false });
    expect(screen.queryByRole('button', { name: t.search.identify.addToLidarr })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.search.identify.confirm })).toBeInTheDocument();
  });

  it('asks no extra question when both artist and album are already in the library', async () => {
    await selectWithLidarrStatus({ known: true, inLibrary: true }, artistMatch({ inLibrary: true }));
    expect(screen.queryByRole('button', { name: t.search.identify.addToLidarr })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.search.identify.downloadAnyway })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.search.identify.confirm })).toBeInTheDocument();
  });

  it('offers both "add to Lidarr" and "download anyway" when the album is not in the library and both lookups succeeded', async () => {
    const { onConfirm } = await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch());
    expect(screen.getByRole('button', { name: t.search.identify.addToLidarr })).toBeInTheDocument();
    const downloadAnyway = screen.getByRole('button', { name: t.search.identify.downloadAnyway });
    expect(downloadAnyway).toBeInTheDocument();
    // "download anyway" proceeds exactly like the ordinary confirm path —
    // it must not be visually or functionally demoted.
    fireEvent.click(downloadAnyway);
    expect(onConfirm).toHaveBeenCalledWith({ artist: 'Radiohead', album: 'In Rainbows' });
  });

  it('prefills quality/metadata from the selected root folder and re-prefills on change', async () => {
    const opts = addOptions({
      rootFolders: [
        { id: 1, path: '/music', accessible: true, freeSpace: 1000, defaultQualityProfileId: 3, defaultMetadataProfileId: 5 },
        { id: 2, path: '/music2', accessible: true, freeSpace: 2000, defaultQualityProfileId: 4, defaultMetadataProfileId: 6 },
      ],
    });
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), { addOptionsRoute: opts });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    const qualitySelect = screen.getByRole('combobox', { name: t.search.identify.qualityProfileLabel });
    const metadataSelect = screen.getByRole('combobox', { name: t.search.identify.metadataProfileLabel });
    expect(qualitySelect).toHaveValue('3');
    expect(metadataSelect).toHaveValue('5');

    const rootFolderSelect = screen.getByRole('combobox', { name: t.search.identify.rootFolderLabel });
    fireEvent.change(rootFolderSelect, { target: { value: '/music2' } });
    expect(qualitySelect).toHaveValue('4');
    expect(metadataSelect).toHaveValue('6');
  });

  it('marks an inaccessible root folder unselectable', async () => {
    const opts = addOptions({
      rootFolders: [
        { id: 1, path: '/broken', accessible: false, freeSpace: 0, defaultQualityProfileId: 3, defaultMetadataProfileId: 5 },
        { id: 2, path: '/music', accessible: true, freeSpace: 1000, defaultQualityProfileId: 3, defaultMetadataProfileId: 5 },
      ],
    });
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), { addOptionsRoute: opts });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    const option = screen.getByRole('option', { name: /broken/ }) as HTMLOptionElement;
    expect(option.disabled).toBe(true);
    // The form falls back to the first ACCESSIBLE folder, not the first one.
    const rootFolderSelect = screen.getByRole('combobox', { name: t.search.identify.rootFolderLabel });
    expect(rootFolderSelect).toHaveValue('/music');
  });

  it('posts monitor: "album" for the default choice', async () => {
    const opts = addOptions();
    const { onConfirm } = await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), {
      addOptionsRoute: opts,
      extraRoutes: [
        {
          method: 'POST',
          match: '/api/lidarr/artists',
          body: () => ({ artistId: 42, alreadyInLibrary: false, artistMonitored: true, albumMonitorState: 'monitored' }),
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    const fetchSpy = vi.mocked(fetch);
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addSubmit }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalled());

    const postCall = fetchSpy.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'POST');
    const postedBody = JSON.parse((postCall?.[1] as RequestInit).body as string);
    expect(postedBody).toMatchObject({ monitor: 'album', artistMbid: 'a1', albumMbid: 'al1' });
  });

  it('posts monitor: "all" and shows the discography warning when "Entire discography" is chosen', async () => {
    const opts = addOptions();
    const { onConfirm } = await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), {
      addOptionsRoute: opts,
      extraRoutes: [
        {
          method: 'POST',
          match: '/api/lidarr/artists',
          body: () => ({ artistId: 42, alreadyInLibrary: false, artistMonitored: true, albumMonitorState: 'monitored' }),
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    fireEvent.click(screen.getByRole('radio', { name: t.search.identify.monitorAll }));
    expect(screen.getByText(t.search.identify.monitorAllWarning)).toBeInTheDocument();

    const fetchSpy = vi.mocked(fetch);
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addSubmit }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalled());

    const postCall = fetchSpy.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'POST');
    const postedBody = JSON.parse((postCall?.[1] as RequestInit).body as string);
    expect(postedBody).toMatchObject({ monitor: 'all' });
  });

  it('shows the partial-success message (not failure, not silent success) when the album refresh had not finished, and only proceeds after Continue', async () => {
    const opts = addOptions();
    const { onConfirm } = await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), {
      addOptionsRoute: opts,
      extraRoutes: [
        {
          method: 'POST',
          match: '/api/lidarr/artists',
          body: () => ({ artistId: 42, alreadyInLibrary: false, artistMonitored: true, albumMonitorState: 'notVisibleYet' }),
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addSubmit }));
    await waitFor(() => expect(screen.getByText(t.search.identify.albumMonitorNotVisibleYet, { exact: false })).toBeInTheDocument());
    // Only the album's note is shown — the artist itself IS monitored here,
    // so its own line must not also appear.
    expect(screen.queryByText(t.search.identify.artistNotMonitored, { exact: false })).not.toBeInTheDocument();
    expect(onConfirm).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.continue }));
    expect(onConfirm).toHaveBeenCalledWith({ artist: 'Radiohead', album: 'In Rainbows' });
  });

  // Review item 5: artistMonitored and albumMonitorState are independent
  // facts — a retry of an add that had silently landed can find the artist
  // itself unmonitored even though the album's own state resolves cleanly.
  it('shows the artist-not-monitored note alongside a clean album state', async () => {
    const opts = addOptions();
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), {
      addOptionsRoute: opts,
      extraRoutes: [
        {
          method: 'POST',
          match: '/api/lidarr/artists',
          body: () => ({ artistId: 42, alreadyInLibrary: true, artistMonitored: false, albumMonitorState: 'monitored' }),
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addSubmit }));
    await waitFor(() => expect(screen.getByText(t.search.identify.artistNotMonitored, { exact: false })).toBeInTheDocument());
  });

  // Review item 6: a 502 addUncertain response must not be rendered with the
  // definite-failure copy, since the add may well have succeeded.
  it('shows addUncertain copy, not the definite-failure copy, on a 502 { code: "addUncertain" }', async () => {
    const opts = addOptions();
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), {
      addOptionsRoute: opts,
      extraRoutes: [
        {
          method: 'POST',
          match: '/api/lidarr/artists',
          status: 502,
          body: { error: 'lidarr did not confirm the add', code: 'addUncertain' },
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addSubmit }));
    await waitFor(() => expect(screen.getByText(t.search.identify.addUncertain)).toBeInTheDocument());
    expect(screen.queryByText(t.search.identify.addArtistFailed)).not.toBeInTheDocument();
  });

  // Review item 1: the add-options fetch failing must not be a dead end —
  // there has to be a way back to the two-path choice (and "download
  // anyway") without closing the whole modal.
  it('offers a way back to the two-path choice when the add-options fetch fails', async () => {
    await selectWithLidarrStatus(
      { known: true, inLibrary: false },
      artistMatch(),
      { extraRoutes: [{ method: 'GET', match: '/api/lidarr/add-options', status: 500, body: { error: 'down' } }] },
    );
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => expect(screen.getByText(t.search.identify.addOptionsFailed)).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addOptionsFailedBack }));
    expect(screen.getByRole('button', { name: t.search.identify.addToLidarr })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.search.identify.downloadAnyway })).toBeInTheDocument();
  });

  // Review item 2: when the root folder's default profile id isn't one of
  // the profiles Lidarr actually returned, the form must fall back to the
  // first available option rather than leaving state pointing at an id no
  // <select> can display.
  it('falls back to the first available quality/metadata profile when the root folder default is missing', async () => {
    const opts = addOptions({
      rootFolders: [
        { id: 1, path: '/music', accessible: true, freeSpace: 1000, defaultQualityProfileId: 0, defaultMetadataProfileId: 0 },
      ],
    });
    await selectWithLidarrStatus({ known: true, inLibrary: false }, artistMatch(), { addOptionsRoute: opts });
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.addToLidarr }));
    await waitFor(() => screen.getByText(t.search.identify.rootFolderLabel));

    const qualitySelect = screen.getByRole('combobox', { name: t.search.identify.qualityProfileLabel });
    const metadataSelect = screen.getByRole('combobox', { name: t.search.identify.metadataProfileLabel });
    expect(qualitySelect).toHaveValue(String(opts.qualityProfiles[0].id));
    expect(metadataSelect).toHaveValue(String(opts.metadataProfiles[0].id));
    expect(screen.getByRole('button', { name: t.search.identify.addSubmit })).not.toBeDisabled();
  });
});
