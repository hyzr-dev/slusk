import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type {
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
  it('reads IN LIBRARY (ok) when known and in the library', () => {
    expect(lidarrLine({ known: true, inLibrary: true }).tone).toBe('ok');
  });

  it('reads NOT IN LIBRARY (quiet, not bad) when known but absent', () => {
    const line = lidarrLine({ known: true, inLibrary: false });
    expect(line.tone).toBe('quiet');
    expect(line.text).toBe(t.search.identify.lidarrNotInLibrary);
  });

  it('reads UNKNOWN when known is false — distinct from "not in library"', () => {
    const line = lidarrLine({ known: false, inLibrary: false });
    expect(line.text).toBe(t.search.identify.lidarrUnknown);
  });

  it('reads UNKNOWN when no match was fetched at all (e.g. the lookup failed)', () => {
    expect(lidarrLine(undefined).text).toBe(t.search.identify.lidarrUnknown);
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
