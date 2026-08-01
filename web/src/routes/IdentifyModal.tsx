import { useEffect, useRef, useState } from 'react';
import { useIdentifyEditions, useIdentifyLidarr, useIdentifySearch } from '../api/queries';
import type { LidarrMatch, MusicBrainzEdition, MusicBrainzSearchResult } from '../api/types';
import type { SearchGroup } from '../api/types';
import Button from '../components/tui/Button';
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
 * The Lidarr status line. `known: false` is UNKNOWN (Lidarr unreachable or
 * the check itself failed), never treated as "not in library" — those are
 * different facts and the copy must not conflate them.
 */
export function lidarrLine(match: LidarrMatch | undefined): { text: string; tone: Tone } {
  if (!match || !match.known) return { text: t.search.identify.lidarrUnknown, tone: 'quiet' };
  if (match.inLibrary) return { text: t.search.identify.lidarrInLibrary, tone: 'ok' };
  return { text: t.search.identify.lidarrNotInLibrary, tone: 'quiet' };
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
  onConfirm: (payload: { artist: string; album: string }) => void;
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

  const identifySearch = useIdentifySearch();
  const identifyEditions = useIdentifyEditions();
  const identifyLidarr = useIdentifyLidarr();

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
    setState('selected');

    const editionsPromise = identifyEditions.mutateAsync(picked.id).catch(() => ({ editions: [], total: 0 }));
    const lidarrPromise = identifyLidarr.mutateAsync(picked.id).catch((): LidarrMatch => ({ known: false, inLibrary: false }));
    const [editionsResult, lidarrResult] = await Promise.all([editionsPromise, lidarrPromise]);
    if (seq !== pickSeq.current) return; // superseded by a later pick
    setEditions(editionsResult.editions);
    setEditionsTotal(editionsResult.total);
    setSelectedEditionId(pickDefaultEdition(editionsResult.editions, group.trackCount)?.id);
    setLidarr(lidarrResult);
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

  function confirm() {
    if (!selectedResult) return;
    const canonicalArtist = canonicalArtistOf(selectedResult);
    if (!canonicalArtist) return;
    onConfirm({ artist: canonicalArtist, album: selectedResult.title });
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
                const lidarrStatus = lidarrLine(lidarr);
                return (
                  <div className={styles.statusLines}>
                    <div className={styles[`tone-${verdict.tone}`]}>{verdict.text}</div>
                    <div className={styles[`tone-${lidarrStatus.tone}`]}>{lidarrStatus.text}</div>
                  </div>
                );
              })()}

              <div className={styles.actions}>
                <Button variant="ghost" onClick={() => setState('suggestions')}>{t.search.identify.back}</Button>
                <span className={styles.spacer} />
                <Button
                  variant="primary"
                  onClick={confirm}
                  disabled={!canonicalArtist}
                  title={!canonicalArtist ? t.search.identify.noCanonicalArtist : undefined}
                >
                  {t.search.identify.confirm}
                </Button>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
