import { useEffect, useRef, useState } from 'react';
import { ApiError } from '../api/client';
import {
  useAddLidarrArtist,
  useIdentifyEditions,
  useIdentifyLidarr,
  useIdentifySearch,
  useLidarrAddOptions,
  useLidarrArtistStatus,
} from '../api/queries';
import type {
  LidarrAddOptions,
  LidarrArtistMatch,
  LidarrMatch,
  MusicBrainzEdition,
  MusicBrainzSearchResult,
} from '../api/types';
import type { SearchGroup } from '../api/types';
import Button from '../components/tui/Button';
import { formatSizeCompact } from '../format';
import { t } from '../strings';
import styles from './IdentifyModal.module.css';

type IdentifyState = 'initial' | 'searching' | 'suggestions' | 'empty' | 'unavailable' | 'selected';
// Not 'faint': --faint was retired in #335 and folded into --text-dim (see
// tokens.css), and a union member spelled 'faint' would read to the next
// person as a live token even though .tone-quiet below (styles[`tone-${t}`])
// has always mapped it to the --text-dim token, not the retired one. 'quiet'
// names what the tone IS (the least emphatic of the four) rather than a
// token that no longer exists.
type Tone = 'ok' | 'bad' | 'dim' | 'quiet';

// Strips ONE trailing bracketed/parenthetical tag — "(2007)", "[FLAC]",
// "{WEB}" — from the end of a folder title. Looped by parseFolderGuess so a
// folder carrying more than one ("Album (2007) [FLAC 24/96]") loses all of
// them, not just the last.
const TRAILING_TAG_RE = /\s*[[({][^[\](){}]*[\])}]\s*$/;

// Matches a bare 4-digit year and nothing else — the left side of a
// "YYYY - Album" folder name, never an artist. Deliberately strict: no
// 2-digit years, no "1980s". See parseFolderGuess's #355 note.
const YEAR_RE = /^\d{4}$/;

/**
 * Best-effort artist/album split for the Identify modal's two editable
 * inputs (issue #321) — never trusted as final data, just a starting point
 * the user is told to check. `group.title` is the release folder's own
 * display name; the common peer convention is "Artist - Album [tags…]", so
 * tags are stripped first and the FIRST " - " (if any) splits the rest.
 *
 * A folder with no such separator (bare "Ride the Lightning", or a folder
 * named after nothing but a format tag) carries no artist information at
 * all, so this falls back to `group.parent` — the same peer-parent-directory
 * guess the pre-#321 download flow posted as `artist` outright (see
 * Search.tsx's `download`). It's an equally bad guess in that case, but here
 * it is only ever a prefilled, editable, explicitly-labelled-GUESSED
 * starting point, never the value actually sent to the server.
 *
 * The other common peer layout is "Artist/YYYY - Album" (#355): the left
 * side of the dash is a bare 4-digit year, not an artist. When that happens
 * `group.parent` — the same already-trusted-as-a-guess prior used above —
 * is preferred over the year, even if the parent itself looks unhelpful
 * ("Soulseek - Share"); it is still no worse a guess than a year, and it is
 * only ever a prefilled, editable, explicitly-labelled-GUESSED value. If
 * there is no parent to fall back on, the year is kept rather than left
 * blank — still editable, still labelled, and no worse than before. A
 * `parent` of "." also counts as "no parent": the server derives it as
 * `path.Base(path.Dir(dir))` (internal/app/search.go), and Go's `path.Dir`
 * returns "." rather than "" for a slashless path — the case where the
 * peer shares the release folder at the top level of their share, with no
 * artist directory above it. Using "." as the artist would be strictly
 * worse than keeping the year.
 */
export function parseFolderGuess(group: SearchGroup): { artist: string; album: string } {
  let stripped = group.title.trim();
  for (;;) {
    const next = stripped.replace(TRAILING_TAG_RE, '').trim();
    if (next === stripped) break;
    stripped = next;
  }
  const dashIdx = stripped.indexOf(' - ');
  if (dashIdx > 0) {
    const candidateArtist = stripped.slice(0, dashIdx).trim();
    const album = stripped.slice(dashIdx + 3).trim();
    const parent = group.parent.trim();
    const hasUsableParent = parent !== '' && parent !== '.';
    if (YEAR_RE.test(candidateArtist) && hasUsableParent) {
      return { artist: group.parent, album };
    }
    return { artist: candidateArtist, album };
  }
  return { artist: group.parent, album: stripped || group.title.trim() };
}

/**
 * Which edition to preselect once the user picks an album — see the brief's
 * "default to the most plausible edition rather than forcing a choice; the
 * user usually does not know which pressing they downloaded". Preference
 * order: an edition whose known track count matches the folder's exactly,
 * then the first known-track-count "Official" release, then simply the
 * first edition returned.
 */
export function pickDefaultEdition(editions: MusicBrainzEdition[], folderTracks: number): MusicBrainzEdition | undefined {
  if (editions.length === 0) return undefined;
  const exact = editions.find((e) => e.trackCountKnown && e.trackCount === folderTracks);
  if (exact) return exact;
  const official = editions.find((e) => e.trackCountKnown && (e.status ?? '').toLowerCase() === 'official');
  if (official) return official;
  return editions[0];
}

/**
 * The verdict line, computed against the SELECTED edition's track count —
 * never the release-group-wide min/max band the mock itself computes (see
 * the brief: with real data that band is 8-97 for a release-group whose
 * deluxe box set shares it with the plain album, which makes COMPLETE fire
 * for almost anything). "More tracks than the edition" is explicitly not an
 * error — 'dim' tone, matching --dim rather than --bad.
 */
export function computeVerdict(folderTracks: number, edition: MusicBrainzEdition | undefined): { text: string; tone: Tone } {
  // Two distinct "unknown" cases, not one shared string (review item B): no
  // edition at all (an empty editions list, or a failed editions fetch —
  // see pickResult's catch) is a different fact from an edition that DOES
  // exist but whose track listing MusicBrainz itself doesn't have. The old
  // shared wording ("this edition has no track listing") asserted a
  // referent that, in the first case, is not there.
  if (!edition) return { text: t.search.identify.verdictNoEdition, tone: 'quiet' };
  if (!edition.trackCountKnown) return { text: t.search.identify.verdictUnknownEdition, tone: 'quiet' };
  if (folderTracks < edition.trackCount) return { text: t.search.identify.verdictIncomplete(folderTracks, edition.trackCount), tone: 'bad' };
  if (folderTracks > edition.trackCount) return { text: t.search.identify.verdictMore(folderTracks, edition.trackCount), tone: 'dim' };
  return { text: t.search.identify.verdictComplete(folderTracks), tone: 'ok' };
}

/**
 * The single Lidarr status line, covering artist and album together.
 *
 * `known: false` on either lookup is UNKNOWN (Lidarr unreachable or the check
 * itself failed), never treated as "not in library" — those are different
 * facts and the copy must not conflate them. Unknown wins over everything
 * else: if we could not ask about one half, we cannot describe the state.
 *
 * This was two lines until #331 review feedback. An absent artist implies an
 * absent album, so rendering both produced two lines saying the same thing
 * ("ARTIST NOT IN LIDARR LIBRARY" directly above "NOT IN LIDARR LIBRARY —
 * …"). One line states which of the three real situations holds and what it
 * means for the import.
 */
export function lidarrLine(
  album: LidarrMatch | undefined,
  artist: LidarrArtistMatch | undefined,
  hasArtistId: boolean,
): { text: string; tone: Tone } {
  if (!album?.known) return { text: t.search.identify.lidarrUnknown, tone: 'quiet' };
  // An album in the library necessarily belongs to an artist in the library,
  // so this answer stands on its own — a failed artist lookup must not
  // downgrade a decisive album answer to UNKNOWN.
  if (album.inLibrary) return { text: t.search.identify.lidarrInLibrary, tone: 'ok' };
  // The album is genuinely absent. Only now does the artist matter, and only
  // when there was an artist to look up: with no artist MBID no lookup was
  // ever made, and reporting one we never made as unknown would be inventing
  // doubt rather than reporting it.
  if (!hasArtistId) return { text: t.search.identify.lidarrAlbumMissing, tone: 'quiet' };
  if (!artist?.known) return { text: t.search.identify.lidarrUnknown, tone: 'quiet' };
  return artist.inLibrary
    ? { text: t.search.identify.lidarrAlbumMissing, tone: 'quiet' }
    : { text: t.search.identify.lidarrArtistAndAlbumMissing, tone: 'quiet' };
}

/**
 * Which of the five cases in the #331 brief applies once an album is
 * selected — drives whether the "add to Lidarr and download" path is
 * offered alongside "download anyway", or whether the modal proceeds as it
 * always has (single CONFIRM, download-only):
 *
 *  - 'noArtistId': the picked result carries no artistId at all (an empty
 *    artist-credit) — adding to Lidarr needs to know which artist it is,
 *    and deriving one from the folder name is exactly what #321 exists to
 *    stop doing. Download-anyway only.
 *  - 'unknown': either lookup came back `known: false` (Lidarr unreachable,
 *    or the lookup itself failed) — we cannot tell whether the artist/album
 *    already exists, so adding is not offered. Download-anyway only.
 *  - 'inLibrary': both are already known and the album is in the library —
 *    the brief is explicit that no extra question is asked here.
 *  - 'offerAdd': the only case where both paths are shown side by side.
 */
export type LidarrAddAvailability = 'noArtistId' | 'unknown' | 'inLibrary' | 'offerAdd';

export function lidarrAddAvailability(
  hasArtistId: boolean,
  album: LidarrMatch | undefined,
  artist: LidarrArtistMatch | undefined,
): LidarrAddAvailability {
  if (!hasArtistId) return 'noArtistId';
  if (!album?.known || !artist?.known) return 'unknown';
  if (album.inLibrary) return 'inLibrary';
  return 'offerAdd';
}

function yearOf(date?: string): string {
  const m = date ? /^(\d{4})/.exec(date) : null;
  return m ? m[1] : '—';
}

function focusable(container: HTMLElement): HTMLElement[] {
  return Array.from(
    container.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ),
  );
}

interface Props {
  group: SearchGroup;
  onClose: () => void;
  // `albumMbid` (issue #59) is the release-group MBID to forward for import,
  // absent only when the user explicitly clicked "download anyway" — see
  // confirm() below for which of the five outcomes forward it.
  onConfirm: (payload: { artist: string; album: string; albumMbid?: string }) => void;
}

export default function IdentifyModal({ group, onClose, onConfirm }: Props) {
  const guess = useState(() => parseFolderGuess(group))[0];
  const [artist, setArtist] = useState(guess.artist);
  const [album, setAlbum] = useState(guess.album);
  const [state, setState] = useState<IdentifyState>('initial');
  const [results, setResults] = useState<MusicBrainzSearchResult[]>([]);
  const [resultsTotal, setResultsTotal] = useState(0);
  const [selectedResult, setSelectedResult] = useState<MusicBrainzSearchResult | undefined>(undefined);
  const [editions, setEditions] = useState<MusicBrainzEdition[]>([]);
  const [editionsTotal, setEditionsTotal] = useState(0);
  const [selectedEditionId, setSelectedEditionId] = useState<string | undefined>(undefined);
  const [lidarr, setLidarr] = useState<LidarrMatch | undefined>(undefined);
  const [lidarrArtist, setLidarrArtist] = useState<LidarrArtistMatch | undefined>(undefined);

  // The "add to Lidarr" sub-flow (issue #331), only ever reachable from the
  // 'offerAdd' case of lidarrAddAvailability. 'closed' is the two-button
  // choice (addToLidarr vs downloadAnyway); 'open' is the root
  // folder/profile form. The add never monitors anything — see
  // internal/app/lidarr_library.go — so the form has no monitoring choice and
  // the 201 carries no monitoring facts to report.
  const [addFlow, setAddFlow] = useState<'closed' | 'open'>('closed');
  const [addOptions, setAddOptions] = useState<LidarrAddOptions | undefined>(undefined);
  const [addOptionsFailed, setAddOptionsFailed] = useState(false);
  const [rootFolderPath, setRootFolderPath] = useState<string | undefined>(undefined);
  const [qualityProfileId, setQualityProfileId] = useState<number | undefined>(undefined);
  const [metadataProfileId, setMetadataProfileId] = useState<number | undefined>(undefined);
  const [addSubmitError, setAddSubmitError] = useState<string | undefined>(undefined);

  const identifySearch = useIdentifySearch();
  const identifyEditions = useIdentifyEditions();
  const identifyLidarr = useIdentifyLidarr();
  const lidarrArtistStatus = useLidarrArtistStatus();
  const lidarrAddOptions = useLidarrAddOptions();
  const addLidarrArtist = useAddLidarrArtist();

  const panelRef = useRef<HTMLDivElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const returnFocusRef = useRef<HTMLElement | null>(null);
  // See the scrim's onMouseDown/onClick below (review item C) for why this
  // exists.
  const scrimMouseDownOnBackground = useRef(false);

  // Focus management: capture whatever had focus before the modal opened (the
  // trigger button), move focus into the panel, and restore it on unmount.
  // Escape and the scrim both call onClose; a click inside the panel itself
  // must not (see the panel's own onClick stopping propagation below).
  useEffect(() => {
    returnFocusRef.current = document.activeElement as HTMLElement | null;
    closeRef.current?.focus();
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose();
        return;
      }
      if (e.key === 'Tab' && panelRef.current) {
        const els = focusable(panelRef.current);
        if (els.length === 0) return;
        const first = els[0];
        const last = els[els.length - 1];
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }
    }
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
      returnFocusRef.current?.focus();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // One combined artist+album call, not the artist-resolve-then-list-albums
  // two-step an earlier version of this modal used. That two-step had a real
  // hazard: a mistyped artist field resolved confidently to *some* artist
  // and listed *their* albums, with nothing in the UI signalling the wrong
  // artist had been picked. GET /api/identify/search ranks the whole
  // artist+album row together, so a bad guess now yields few or no matches
  // rather than a confident wrong list — and it also carries editionCount
  // per row for free, which the two-step design couldn't (a per-row edition
  // count needed a separate rate-limited call per album).
  async function searchMB() {
    const artistQuery = artist.trim();
    const albumQuery = album.trim();
    // `album` is required server-side (a blank one 422s before any upstream
    // call) — `artist` alone is never enough to search on.
    if (!albumQuery) return;
    setState('searching');
    try {
      const result = await identifySearch.mutateAsync({ artist: artistQuery || undefined, album: albumQuery });
      if (result.results.length === 0) {
        setState('empty');
        return;
      }
      setResults(result.results);
      setResultsTotal(result.total);
      setState('suggestions');
    } catch {
      setState('unavailable');
    }
  }

  // Never blocks on a failed edition/Lidarr lookup — both are best-effort
  // enrichment of the Selected state, not gates on reaching it. A failed
  // editions call leaves the picker empty (verdict reads COMPLETENESS
  // UNKNOWN); a failed Lidarr call reads exactly like `known: false`,
  // because to the user those are the same fact: "can't say".
  //
  // Guards a fast BACK-then-pick-another with `pickSeq`: react-query's
  // MutationObserver only dedupes the HOOK's own `.data`/`.error` (it always
  // tracks the latest `mutate()` call), which this function never reads —
  // it consumes `mutateAsync`'s own returned Promise directly, and that
  // Promise settles with its own call's real result regardless of any later
  // call, with nothing built in to drop it. Two overlapping `pickResult`
  // invocations could therefore resolve out of order and have the OLDER
  // one's `setEditions`/`setLidarr` win, showing the wrong album's data.
  // `pickSeq` makes this function's own state writes ignore a stale
  // resolution instead.
  const pickSeq = useRef(0);
  async function pickResult(picked: MusicBrainzSearchResult) {
    const seq = ++pickSeq.current;
    setSelectedResult(picked);
    setEditions([]);
    setEditionsTotal(0);
    setSelectedEditionId(undefined);
    setLidarr(undefined);
    setLidarrArtist(undefined);
    resetAddFlow();
    setState('selected');

    const editionsPromise = identifyEditions.mutateAsync(picked.id).catch(() => ({ editions: [], total: 0 }));
    const lidarrPromise = identifyLidarr.mutateAsync(picked.id).catch((): LidarrMatch => ({ known: false, inLibrary: false }));
    // Only fetched when the result carries an artistId at all (see
    // lidarrAddAvailability's 'noArtistId' case) — MusicBrainzSearchResult's
    // artist/artistId are genuinely absent, not empty strings, for a
    // release-group whose artist-credit is itself empty.
    const artistPromise: Promise<LidarrArtistMatch | undefined> = picked.artistId
      ? lidarrArtistStatus.mutateAsync(picked.artistId).catch((): LidarrArtistMatch => ({ known: false, inLibrary: false }))
      : Promise.resolve(undefined);
    const [editionsResult, lidarrResult, artistResult] = await Promise.all([editionsPromise, lidarrPromise, artistPromise]);
    if (seq !== pickSeq.current) return; // superseded by a later pick
    setEditions(editionsResult.editions);
    setEditionsTotal(editionsResult.total);
    setSelectedEditionId(pickDefaultEdition(editionsResult.editions, group.trackCount)?.id);
    setLidarr(lidarrResult);
    setLidarrArtist(artistResult);
  }

  // Resets every piece of "add to Lidarr" sub-flow state — shared by
  // pickResult (a fresh pick starts over) and the choice screen's own
  // "back to the two-path choice" affordance.
  function resetAddFlow() {
    setAddFlow('closed');
    setAddOptions(undefined);
    setAddOptionsFailed(false);
    setRootFolderPath(undefined);
    setQualityProfileId(undefined);
    setMetadataProfileId(undefined);
    setAddSubmitError(undefined);
  }

  // Opens the root-folder/profile form and loads its options on
  // first entry only — reopening after a Cancel reuses what's already
  // fetched rather than refetching.
  async function openAddFlow() {
    setAddFlow('open');
    if (addOptions) return;
    setAddOptionsFailed(false);
    try {
      const opts = await lidarrAddOptions.mutateAsync();
      setAddOptions(opts);
      const preferred = opts.rootFolders.find((r) => r.accessible) ?? opts.rootFolders[0];
      if (preferred) applyRootFolder(preferred.path, opts);
    } catch {
      setAddOptionsFailed(true);
    }
  }

  // Re-prefills quality/metadata from the newly selected root folder's own
  // defaults — see the brief: "re-prefill when the root folder changes". Falls
  // back to the first available profile when the folder's default id isn't
  // actually one of the profiles Lidarr returned (review item 2): posting a
  // default that doesn't exist would leave the <select> painting the first
  // option while state held a different (submitted) id — a silent mismatch
  // between what's shown and what's sent.
  function applyRootFolder(path: string, opts: LidarrAddOptions = addOptions ?? { rootFolders: [], qualityProfiles: [], metadataProfiles: [] }) {
    setRootFolderPath(path);
    const folder = opts.rootFolders.find((r) => r.path === path);
    if (!folder) return;
    const quality = opts.qualityProfiles.some((p) => p.id === folder.defaultQualityProfileId)
      ? folder.defaultQualityProfileId
      : opts.qualityProfiles[0]?.id;
    const metadata = opts.metadataProfiles.some((p) => p.id === folder.defaultMetadataProfileId)
      ? folder.defaultMetadataProfileId
      : opts.metadataProfiles[0]?.id;
    setQualityProfileId(quality);
    setMetadataProfileId(metadata);
  }

  async function submitAddArtist() {
    if (!selectedResult?.artistId || !rootFolderPath || !qualityProfileId || !metadataProfileId) return;
    setAddSubmitError(undefined);
    try {
      await addLidarrArtist.mutateAsync({
        artistMbid: selectedResult.artistId,
        artistName: canonicalArtistOf(selectedResult) ?? '',
        rootFolderPath,
        qualityProfileId,
        metadataProfileId,
      });
      confirm();
    } catch (err) {
      // A 502 { code: "addUncertain" } means the add may or may not have
      // happened server-side — a genuinely different fact from every other
      // failure status, which all mean the add definitely did not happen.
      // Rendering it with the definite-failure copy would invent certainty
      // the response doesn't have (review item 6).
      if (err instanceof ApiError && err.status === 502 && err.body?.code === 'addUncertain') {
        setAddSubmitError(t.search.identify.addUncertain);
      } else {
        setAddSubmitError(t.search.identify.addArtistFailed);
      }
    }
  }

  // MusicBrainzSearchResult.artist is absent, not empty, for a release-group
  // whose artist-credit is itself empty — POST /api/jobs requires a string
  // `artist`, so `undefined` can never be sent. Falls back to whatever is
  // currently in the (user-editable, user-confirmed) artist field rather
  // than silently reusing the raw folder guess — that field starts as the
  // folder guess but the user may have corrected it before searching, so it
  // is the more truthful of the two.
  //
  // Deliberately does NOT fall further back to `group.parent` (review item
  // A). That was the ORIGINAL bug: the peer's parent directory
  // (`Soulseek - Share`, `music/_emily/`, `Various Artists`) is the exact
  // value #321 exists to stop posting, and it was reachable here with no
  // "guessed" marker — the GUESSED-labelled inputs are hidden once a result
  // is selected. Returns undefined instead, meaning "no honest canonical
  // artist exists"; the render below disables CONFIRM and says so rather
  // than inventing one. Shared with the "WILL BE RECORDED AS" summary so the
  // modal never displays one artist and confirms a different one.
  function canonicalArtistOf(result: MusicBrainzSearchResult): string | undefined {
    const fromField = artist.trim();
    return result.artist || (fromField ? fromField : undefined);
  }

  // `withMbid` defaults to true: every path forwards the identified
  // release-group MBID EXCEPT the explicit "download anyway" button, which is
  // the user's deliberate choice to skip import (issue #59). That includes
  // the 'unknown' and 'noArtistId' availability cases (a Lidarr outage during
  // identify, or a result with no artist credit) — resolution happens later
  // at import time, so those must not be silently downgraded to
  // never-imports the way "download anyway" is.
  function confirm(withMbid = true) {
    if (!selectedResult) return;
    const canonicalArtist = canonicalArtistOf(selectedResult);
    if (!canonicalArtist) return;
    onConfirm({
      artist: canonicalArtist,
      album: selectedResult.title,
      albumMbid: withMbid ? selectedResult.id : undefined,
    });
  }

  const selectedEdition = editions.find((e) => e.id === selectedEditionId);
  const showFields = state === 'initial' || state === 'suggestions' || state === 'empty';
  // See MusicBrainzSearchResponse's doc comment: a relevance-ranked search,
  // not a paginated catalogue, so this reads "showing the best N" rather
  // than the editions picker's "showing N of total" below.
  const resultsNotice = resultsTotal > results.length ? t.search.identify.showingBestOf(results.length) : undefined;
  const editionsNotice = editionsTotal > editions.length ? t.search.identify.showingOf(editions.length, editionsTotal) : undefined;
  const canonicalArtist = selectedResult ? canonicalArtistOf(selectedResult) : undefined;
  // Review item H: the endpoint 422s on a blank album, so the button is
  // disabled rather than silently doing nothing when clicked.
  const albumBlank = album.trim().length === 0;
  // See lidarrAddAvailability's doc comment for what each case means.
  const availability = selectedResult
    ? lidarrAddAvailability(Boolean(selectedResult.artistId), lidarr, lidarrArtist)
    : 'unknown';
  const selectedRootFolder = addOptions?.rootFolders.find((r) => r.path === rootFolderPath);
  const addFormValid = Boolean(selectedRootFolder?.accessible && qualityProfileId && metadataProfileId);
  // Review item 3: whether the add form has anything usable to submit at
  // all. When it doesn't, every option in one of its selects is disabled and
  // addFormValid is permanently false with nothing on screen explaining why
  // — this drives an explicit line instead.
  const hasAccessibleRootFolder = Boolean(addOptions?.rootFolders.some((r) => r.accessible));
  const hasUsableProfiles = Boolean(addOptions?.qualityProfiles.length && addOptions?.metadataProfiles.length);

  return (
    <div
      className={styles.scrim}
      // Review item C: mousedown, not click, decides whether the scrim
      // closes the modal. A plain onClick on the scrim also fires for a
      // drag that STARTS inside the panel (e.g. selecting text in the
      // artist field) and ENDS on the scrim — the browser's `click` event
      // targets the nearest common ancestor of the mousedown and mouseup
      // targets, which is the scrim itself, and the panel's own
      // onClick-stopPropagation below never sees that click at all because
      // it never fired on the panel in the first place. Recording whether
      // the MOUSEDOWN landed on the scrim background itself (not a
      // descendant) is what distinguishes an actual scrim click from a drag
      // that merely ends there.
      onMouseDown={(e) => {
        scrimMouseDownOnBackground.current = e.target === e.currentTarget;
      }}
      onClick={() => {
        if (scrimMouseDownOnBackground.current) onClose();
      }}
    >
      <div
        ref={panelRef}
        className={styles.panel}
        role="dialog"
        aria-modal="true"
        aria-label={t.search.identify.dialogLabel}
        onClick={(e) => e.stopPropagation()}
      >
        <div className={styles.header}>
          <span className={styles.headerLabel}>{t.search.identify.dialogTitle}</span>
          <span className={styles.spacer} />
          <button ref={closeRef} type="button" className={styles.closeButton} aria-label={t.search.identify.close} onClick={onClose}>
            ✕
          </button>
        </div>

        <div className={styles.body}>
          <div className={styles.fieldLabel}>{t.search.identify.identifyingFolder}</div>
          <div className={styles.folderPath}>{group.folder}</div>

          {showFields && (
            <div className={styles.fields}>
              <label className={styles.field}>
                <span className={styles.fieldLabel}>{t.search.identify.artistLabel}</span>
                <input className={styles.input} value={artist} onChange={(e) => setArtist(e.target.value)} />
              </label>
              <label className={styles.field}>
                <span className={styles.fieldLabel}>{t.search.identify.albumLabel}</span>
                <input className={styles.input} value={album} onChange={(e) => setAlbum(e.target.value)} />
              </label>
            </div>
          )}

          {state === 'initial' && (
            <Button
              variant="primary"
              className={styles.fullWidthButton}
              onClick={searchMB}
              disabled={albumBlank}
              title={albumBlank ? t.search.identify.albumRequired : undefined}
            >
              {t.search.identify.searchButton}
            </Button>
          )}

          {state === 'searching' && (
            <div className={styles.centered}>
              <span aria-hidden className={styles.spinner} />
              {t.search.identify.searching}
            </div>
          )}

          {state === 'suggestions' && (
            <div className={styles.suggestions}>
              <div className={styles.suggestionsHead}>
                <span>{t.search.identify.colArtistAlbum}</span>
                <span>{t.search.identify.colType}</span>
                <span>{t.search.identify.colYear}</span>
                <span className={styles.suggestionEditionsHead}>{t.search.identify.colEditions}</span>
              </div>
              <ul className={styles.suggestionsList}>
                {results.map((r) => (
                  <li key={r.id}>
                    <button type="button" className={styles.suggestionRow} onClick={() => pickResult(r)}>
                      <span className={styles.suggestionText}>
                        <span className={styles.suggestionAlbum}>{r.title}</span>
                        {/* Absent, not a placeholder, when the release-group's
                            artist-credit is itself empty — see
                            MusicBrainzSearchResult's doc comment. */}
                        {r.artist && <span className={styles.suggestionArtist}>{r.artist}</span>}
                      </span>
                      <span className={styles.suggestionType}>{(r.primaryType ?? '—').toUpperCase()}</span>
                      <span className={styles.suggestionYear}>{yearOf(r.firstReleaseDate)}</span>
                      <span className={styles.suggestionEditions}>{t.search.identify.editionCountShort(r.editionCount)}</span>
                    </button>
                  </li>
                ))}
              </ul>
              {resultsNotice && <div className={styles.truncationNotice}>{resultsNotice}</div>}
              <div className={styles.notIt}>{t.search.identify.notIt}</div>
            </div>
          )}

          {state === 'empty' && (
            <div className={styles.centeredBlock}>
              <div className={styles.badTitle}>{t.search.identify.noMatchesTitle}</div>
              <div className={styles.bodyText}>{t.search.identify.noMatchesBody}</div>
              <Button variant="ghost" onClick={searchMB}>{t.search.identify.searchAgain}</Button>
            </div>
          )}

          {state === 'unavailable' && (
            <div className={styles.centeredBlock}>
              <div className={styles.badTitle}>{t.search.identify.unavailableTitle}</div>
              <div className={styles.bodyText}>{t.search.identify.unavailableBody}</div>
              <Button variant="ghost" onClick={searchMB}>{t.search.identify.retry}</Button>
            </div>
          )}

          {state === 'selected' && selectedResult && (
            <>
              <div className={styles.recorded}>
                <div className={styles.fieldLabel}>{t.search.identify.willBeRecordedAs}</div>
                <div className={styles.recordedAlbum}>{selectedResult.title}</div>
                <div className={styles.recordedMeta}>
                  {/* canonicalArtist omitted entirely (never a placeholder)
                      when there is no honest one to show — see
                      canonicalArtistOf. editionCount is the search result's
                      own field — already in hand from the same call that
                      produced this row, unlike the picker below which needs
                      the separate editions list for actual per-edition
                      detail. */}
                  {[
                    canonicalArtist,
                    (selectedResult.primaryType ?? '—').toUpperCase(),
                    yearOf(selectedResult.firstReleaseDate),
                    t.search.identify.editionCount(selectedResult.editionCount),
                  ]
                    .filter((part): part is string => Boolean(part))
                    .join(' · ')}
                </div>
              </div>

              {!canonicalArtist && (
                <div className={styles.noCanonicalArtist}>{t.search.identify.noCanonicalArtist}</div>
              )}

              {editions.length > 0 && (
                <div className={styles.editionPicker}>
                  <div className={styles.fieldLabel}>{t.search.identify.editionPickerLabel}</div>
                  <select
                    className={styles.select}
                    value={selectedEditionId}
                    onChange={(e) => setSelectedEditionId(e.target.value)}
                  >
                    {editions.map((ed) => (
                      <option key={ed.id} value={ed.id}>
                        {[
                          yearOf(ed.date),
                          ed.country,
                          ed.status,
                          ed.trackCountKnown ? `${ed.trackCount} tracks` : t.search.identify.editionUnknownTracks,
                        ]
                          .filter(Boolean)
                          .join(' · ')}
                      </option>
                    ))}
                  </select>
                  {editionsNotice && <div className={styles.truncationNotice}>{editionsNotice}</div>}
                </div>
              )}

              {(() => {
                const verdict = computeVerdict(group.trackCount, selectedEdition);
                const status = lidarrLine(lidarr, lidarrArtist, Boolean(selectedResult.artistId));
                return (
                  <div className={styles.statusLines}>
                    <div className={styles[`tone-${verdict.tone}`]}>{verdict.text}</div>
                    <div className={styles[`tone-${status.tone}`]}>{status.text}</div>
                    {availability === 'noArtistId' && (
                      <div className={styles['tone-quiet']}>{t.search.identify.noArtistId}</div>
                    )}
                  </div>
                );
              })()}

              {/* The two-path choice (issue #331 case 2) — only reachable
                  when the album is confirmed NOT in the library and both the
                  artist and album lookups succeeded. Neither path is
                  demoted: "download anyway" renders as a real button, not a
                  footnote. */}
              {availability === 'offerAdd' && addFlow === 'closed' && (
                <div className={styles.actions}>
                  <Button variant="ghost" onClick={() => setState('suggestions')}>{t.search.identify.back}</Button>
                  <span className={styles.spacer} />
                  <Button
                    variant="ghost"
                    onClick={() => confirm(false)}
                    disabled={!canonicalArtist}
                    title={!canonicalArtist ? t.search.identify.noCanonicalArtist : undefined}
                  >
                    {t.search.identify.downloadAnyway}
                  </Button>
                  <Button variant="primary" onClick={openAddFlow}>{t.search.identify.addToLidarr}</Button>
                </div>
              )}

              {availability === 'offerAdd' && addFlow === 'open' && (
                <div className={styles.addForm}>
                  {!addOptions && !addOptionsFailed && (
                    <div className={styles.centered}>
                      <span aria-hidden className={styles.spinner} />
                      {t.search.identify.addOptionsLoading}
                    </div>
                  )}

                  {addOptionsFailed && (
                    <div className={styles.centeredBlock}>
                      <div className={styles.bodyText}>{t.search.identify.addOptionsFailed}</div>
                      <Button variant="ghost" onClick={openAddFlow}>{t.search.identify.retry}</Button>
                      {/* Review item 1: without this, a failed fetch was a
                          dead end — Cancel lived inside the addOptions guard
                          below, unreachable once options never load, with no
                          way back to "download anyway" short of closing the
                          whole modal. */}
                      <Button variant="ghost" onClick={() => setAddFlow('closed')}>{t.search.identify.addOptionsFailedBack}</Button>
                    </div>
                  )}

                  {addOptions && (
                    <>
                      <label className={styles.field}>
                        <span className={styles.fieldLabel}>{t.search.identify.rootFolderLabel}</span>
                        <select
                          className={styles.select}
                          value={rootFolderPath ?? ''}
                          onChange={(e) => applyRootFolder(e.target.value)}
                        >
                          {addOptions.rootFolders.map((r) => (
                            <option key={r.id} value={r.path} disabled={!r.accessible}>
                              {r.path}
                              {r.accessible
                                ? ` (${formatSizeCompact(r.freeSpace)} free)`
                                : ` — ${t.search.identify.rootFolderInaccessible}`}
                            </option>
                          ))}
                        </select>
                      </label>
                      {!hasAccessibleRootFolder && (
                        <div className={styles['tone-bad']}>{t.search.identify.noAccessibleRootFolder}</div>
                      )}

                      <label className={styles.field}>
                        <span className={styles.fieldLabel}>{t.search.identify.qualityProfileLabel}</span>
                        <select
                          className={styles.select}
                          value={qualityProfileId ?? ''}
                          onChange={(e) => setQualityProfileId(Number(e.target.value))}
                        >
                          {addOptions.qualityProfiles.map((p) => (
                            <option key={p.id} value={p.id}>{p.name}</option>
                          ))}
                        </select>
                      </label>

                      <label className={styles.field}>
                        <span className={styles.fieldLabel}>{t.search.identify.metadataProfileLabel}</span>
                        <select
                          className={styles.select}
                          value={metadataProfileId ?? ''}
                          onChange={(e) => setMetadataProfileId(Number(e.target.value))}
                        >
                          {addOptions.metadataProfiles.map((p) => (
                            <option key={p.id} value={p.id}>{p.name}</option>
                          ))}
                        </select>
                      </label>
                      {!hasUsableProfiles && (
                        <div className={styles['tone-bad']}>{t.search.identify.noUsableProfiles}</div>
                      )}

                      {addSubmitError && <div className={styles['tone-bad']}>{addSubmitError}</div>}

                      <div className={styles.actions}>
                        <Button variant="ghost" onClick={() => setAddFlow('closed')}>{t.search.identify.addCancel}</Button>
                        <span className={styles.spacer} />
                        <Button
                          variant="primary"
                          onClick={submitAddArtist}
                          disabled={!addFormValid || addLidarrArtist.isPending}
                        >
                          {addLidarrArtist.isPending ? t.search.identify.addSubmitting : t.search.identify.addSubmit}
                        </Button>
                      </div>
                    </>
                  )}
                </div>
              )}

              {availability !== 'offerAdd' && (
                <div className={styles.actions}>
                  <Button variant="ghost" onClick={() => setState('suggestions')}>{t.search.identify.back}</Button>
                  <span className={styles.spacer} />
                  <Button
                    variant="primary"
                    onClick={() => confirm()}
                    disabled={!canonicalArtist}
                    title={!canonicalArtist ? t.search.identify.noCanonicalArtist : undefined}
                  >
                    {t.search.identify.confirm}
                  </Button>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
}
