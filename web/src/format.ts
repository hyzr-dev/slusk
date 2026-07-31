// All user-facing formatting lives here so locale choices are made in one place.
// Dates use sv-SE regardless of UI language: ISO-like output (2026-07-20 14:32)
// is easier to scan in a technical tool than en-US ordering.
const DATE_LOCALE = 'sv-SE';

export function formatBytes(n: number | null | undefined): string {
  if (!n) return '0 MB';
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

// Same scaling as formatBytes, but for spots (the expansion panel's Size and
// Downloaded rows) where a 0 means "not known yet" rather than "an empty
// file", so it should read like the row's other placeholders rather than the
// misleading "0 MB".
export function formatBytesOrDash(n: number | null | undefined): string {
  if (!n) return '—';
  return formatBytes(n);
}

// Leaf filename from a remote (Soulseek peers commonly use backslashes) or
// local path, shared by the job detail page and the jobs-list expansion
// panel so both show the same short name instead of one showing full paths.
export function basename(path: string): string {
  const idx = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'));
  return idx === -1 ? path : path.slice(idx + 1);
}

export function percent(done: number, total: number): number {
  if (!total) return 0;
  const raw = Math.round((done / total) * 100);
  return Math.min(100, Math.max(0, raw));
}

export function formatDateTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString(DATE_LOCALE);
}

export function formatTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString(DATE_LOCALE);
}

export function formatShortTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString(DATE_LOCALE, { hour: '2-digit', minute: '2-digit' });
}

/**
 * The local calendar day an instant falls on, as `YYYY-MM-DD`.
 *
 * Deliberately local rather than UTC: a message sent at 00:30 in Stockholm
 * carries `…T22:30:00Z` from the previous UTC day, and grouping on the UTC date
 * would file it under yesterday for every reader east of Greenwich. Returns ''
 * for input that is missing or unparseable, so a caller can group those
 * together instead of crashing.
 *
 * `sv-SE` already renders dates as `YYYY-MM-DD`, so the key doubles as the
 * display label for any day too old to name (see Chat's day dividers, #247).
 */
export function localDayKey(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleDateString(DATE_LOCALE);
}

export function formatScore(n: number): string {
  return n.toFixed(2);
}

// Download speed in bytes/sec. Soulseek transfers commonly sit in the KB/s
// range, so scale to KB/s below 1 MB/s and MB/s above, unlike formatBytes which
// is always MB.
export function formatSpeed(bytesPerSec: number | null | undefined): string {
  if (!bytesPerSec) return '—';
  const kb = bytesPerSec / 1024;
  if (kb < 1024) return `${kb.toFixed(0)} KB/s`;
  return `${(kb / 1024).toFixed(1)} MB/s`;
}

// etaSeconds is a duration, not a timestamp (see api/types.ts Job.etaSeconds).
// Hours above 60 minutes, minutes below an hour, seconds below a minute — a
// slow peer on a large Soulseek album can easily push past an hour.
export function formatEta(etaSeconds: number | null | undefined): string {
  if (!etaSeconds) return '—';
  if (etaSeconds >= 3600) {
    let h = Math.floor(etaSeconds / 3600);
    let m = Math.round((etaSeconds % 3600) / 60);
    // Rounding the remainder can reach a full hour (7199s -> 1h 60min).
    if (m === 60) {
      h += 1;
      m = 0;
    }
    return m > 0 ? `${h} h ${m} min` : `${h} h`;
  }
  if (etaSeconds >= 60) return `${Math.round(etaSeconds / 60)} min`;
  return `${Math.round(etaSeconds)} s`;
}

/**
 * Formats an elapsed time that has already been *measured*, as opposed to
 * formatEta's projection of one still to come.
 *
 * The difference that matters is the zero case. formatEta renders any falsy
 * input as '—' on purpose: an absent ETA must not be dressed up as "0 s", and
 * its callers read an optional field where absent means "no claim". A measured
 * duration is the opposite — 0 means the work finished faster than the clock's
 * resolution, which is an answer, not a gap. Reconcile passes routinely
 * complete in a fraction of a second, so reusing formatEta for them rendered
 * every row of the DUR column as '—'.
 *
 * Sub-10s durations keep one decimal because that is the range these actually
 * land in, and rounding them to whole seconds throws away the distinction the
 * column exists to show. NaN (an unparseable timestamp) and negatives (clock
 * skew between the two stamps) are the only genuine unknowns, and those do
 * return '—'.
 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  if (seconds < 10) return `${seconds.toFixed(1)} s`;
  return formatEta(Math.round(seconds));
}

// The scaling byte formatter: use it wherever a value can leave the MB range,
// such as aggregate share/library sizes (routinely hundreds of GB or several
// TB) or an individual transfer of a large file. It steps up through GB and TB
// the same way formatSpeed steps through KB/MB. formatBytes stays deliberately
// locked to MB for parity with the legacy dashboard, which its test pins.
export function formatSize(bytes: number | null | undefined): string {
  if (!bytes) return '0 MB';
  const mb = bytes / (1024 * 1024);
  if (mb < 1024) return `${mb.toFixed(1)} MB`;
  const gb = mb / 1024;
  if (gb < 1024) return `${gb.toFixed(1)} GB`;
  return `${(gb / 1024).toFixed(1)} TB`;
}

// The compact byte formatter, for cells where the column width is fixed and
// two values share it. It follows the unit scale the TUI mock was drawn
// against (docs/design/slskdarr-tui.dc.html:958) — no decimal below 1 GB, no
// space before the unit letter — which is what makes "340M / 911M" fit the
// Overview TRANSFERS SIZE column where "340.1 MB / 911.8 MB" does not.
// Prefer formatSize everywhere the spelled-out form fits; this one trades
// legibility for width and should not spread beyond cells that need it.
export function formatSizeCompact(bytes: number | null | undefined): string {
  if (!bytes) return '0M';
  const mb = bytes / (1024 * 1024);
  if (mb < 1024) return `${Math.round(mb)}M`;
  const gb = mb / 1024;
  if (gb < 1024) return `${gb.toFixed(1)}G`;
  return `${(gb / 1024).toFixed(1)}T`;
}

// UploadEntry.filename is a Soulseek virtual path, which is always
// backslash-separated regardless of the host OS that shared it — never
// forward-slash. The full path stays available (e.g. in a `title`
// attribute); this is only for the compact row label.
export function formatVirtualPath(path: string): string {
  const parts = path.split('\\');
  return parts[parts.length - 1];
}

// How long ago something happened, for spots where the value is only ever
// shown once it has grown large enough to matter (the top bar's staleness
// warning). Deliberately coarse above a minute — nobody reads "312s".
export function formatAge(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  return m > 0 ? `${h}h ${m}m` : `${h}h`;
}

// A track's own measured length, mm:ss — distinct from formatEta (a duration
// still to come) and formatDuration (an already-measured span rendered in
// the same units formatEta uses, i.e. "3 min"): a Soulseek search result's
// per-track duration is short enough, and compared often enough against
// other tracks in the same folder, that the clock-face mm:ss reads better
// than "4 min". Undefined (the peer sent no Duration attribute — issue #58
// §1) renders as '—', matching every other optional search-result field.
export function formatTrackDuration(seconds: number | undefined): string {
  if (seconds === undefined || !Number.isFinite(seconds) || seconds < 0) return '—';
  const whole = Math.round(seconds);
  const m = Math.floor(whole / 60);
  const s = (whole % 60).toString().padStart(2, '0');
  return `${m}:${s}`;
}

// Collation for file listings. `numeric` makes "02" sort before "10" instead of
// after it, which is the whole trick here: a Soulseek transfer carries no track
// field — when a track number exists at all it is part of the filename — so one
// numeric collation gives track order where there is a number and plain
// alphabetical order where there is not, rather than two rules and a guess
// about which applies.
//
// sv-SE because the reader is Swedish: å, ä and ö belong after z, not folded
// into a and o the way an English collation would place them.
const FILE_COLLATOR = new Intl.Collator('sv-SE', { numeric: true, sensitivity: 'base' });

// Orders two paths by their leaf name, so the listing matches what is shown
// rather than sorting on directories the reader cannot see.
export function compareFileNames(a: string, b: string): number {
  return FILE_COLLATOR.compare(basename(a), basename(b));
}
