import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type {
  LidarrMatch,
  MusicBrainzAlbumListResult,
  MusicBrainzArtistSearchResult,
  MusicBrainzEdition,
  MusicBrainzEditionListResult,
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
    expect(v.tone).toBe('faint');
    expect(v.text).not.toContain('0');
  });

  it('is unknown when no edition is selected at all', () => {
    expect(computeVerdict(10, undefined).tone).toBe('faint');
  });
});

describe('lidarrLine', () => {
  it('reads IN LIBRARY (ok) when known and in the library', () => {
    expect(lidarrLine({ known: true, inLibrary: true }).tone).toBe('ok');
  });

  it('reads NOT IN LIBRARY (faint, not bad) when known but absent', () => {
    const line = lidarrLine({ known: true, inLibrary: false });
    expect(line.tone).toBe('faint');
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

  it('goes through searching, into suggestions, and shows the truncation notice when total exceeds the array', async () => {
    const artists: MusicBrainzArtistSearchResult = {
      artists: [{ id: 'a1', name: 'Radiohead', score: 100 }],
      total: 1,
    };
    const albums: MusicBrainzAlbumListResult = {
      albums: [{ id: 'al1', title: 'In Rainbows', firstReleaseDate: '2007-10-10', primaryType: 'Album', secondaryTypes: [] }],
      total: 412,
    };
    stubFetch({
      '/api/identify/artists?': artists,
      '/api/identify/artists/a1/albums': albums,
    });
    renderModal(group());
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    expect(screen.getByText(t.search.identify.searching)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: /In Rainbows/ })).toBeInTheDocument());
    expect(screen.getByText(t.search.identify.showingOf(1, 412))).toBeInTheDocument();
  });

  it('shows NO MATCHES when the artist search returns nothing', async () => {
    stubFetch({ '/api/identify/artists?': { artists: [], total: 0 } satisfies MusicBrainzArtistSearchResult });
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
    const artists: MusicBrainzArtistSearchResult = { artists: [{ id: 'a1', name: 'Radiohead', score: 100 }], total: 1 };
    const albums: MusicBrainzAlbumListResult = {
      albums: [{ id: 'al1', title: 'In Rainbows', firstReleaseDate: '2007-10-10', primaryType: 'Album', secondaryTypes: [] }],
      total: 1,
    };
    const editions: MusicBrainzEditionListResult = {
      editions: [
        edition({ id: 'e1', trackCount: 10, trackCountKnown: true }),
        edition({ id: 'e2', trackCountKnown: false, trackCount: 0, title: 'Unknown pressing' }),
      ],
      total: 25,
    };
    const lidarr: LidarrMatch = { known: true, inLibrary: true };
    stubFetch({
      '/api/identify/artists?': artists,
      '/api/identify/artists/a1/albums': albums,
      '/api/identify/albums/al1/editions': editions,
      '/api/identify/albums/al1/lidarr': lidarr,
    });
    renderModal(group());
    fireEvent.click(screen.getByRole('button', { name: t.search.identify.searchButton }));
    await waitFor(() => screen.getByRole('button', { name: /In Rainbows/ }));
    fireEvent.click(screen.getByRole('button', { name: /In Rainbows/ }));

    await waitFor(() => expect(screen.getByText(t.search.identify.willBeRecordedAs)).toBeInTheDocument());
    // Edition e1 (exact track match) is the default selection, so the
    // verdict reads COMPLETE against it.
    expect(screen.getByText(t.search.identify.verdictComplete(10))).toBeInTheDocument();
    expect(screen.getByText(t.search.identify.lidarrInLibrary)).toBeInTheDocument();
    expect(screen.getByText(t.search.identify.showingOf(2, 25))).toBeInTheDocument();

    // The unknown-track-count edition must say so, never show "0 tracks".
    expect(screen.getByText(t.search.identify.editionUnknownTracks, { exact: false })).toBeInTheDocument();
    expect(screen.queryByText(/(^|\s)0 tracks/)).not.toBeInTheDocument();
  });

  it('confirms with the canonical MusicBrainz artist/album, not the folder guess', async () => {
    const artists: MusicBrainzArtistSearchResult = { artists: [{ id: 'a1', name: 'Radiohead', score: 100 }], total: 1 };
    const albums: MusicBrainzAlbumListResult = {
      albums: [{ id: 'al1', title: 'In Rainbows', primaryType: 'Album', secondaryTypes: [] }],
      total: 1,
    };
    stubFetch({
      '/api/identify/artists?': artists,
      '/api/identify/artists/a1/albums': albums,
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

  it('closes on Escape and on a scrim click, but not on a click inside the panel', () => {
    const { onClose, container } = renderModal(group());
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByText(t.search.identify.identifyingFolder));
    expect(onClose).toHaveBeenCalledTimes(1);

    const scrim = container.firstChild as HTMLElement;
    fireEvent.click(scrim);
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('gives the dialog the right ARIA role and label', () => {
    renderModal(group());
    expect(screen.getByRole('dialog', { name: t.search.identify.dialogLabel })).toBeInTheDocument();
  });
});
