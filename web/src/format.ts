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

// Aggregate share/library sizes routinely reach hundreds of GB or several TB,
// unlike the per-file/per-transfer sizes formatBytes covers (deliberately
// locked to MB), so this scales up through GB and TB the same way formatSpeed
// scales through KB/MB.
export function formatSize(bytes: number | null | undefined): string {
  if (!bytes) return '0 MB';
  const mb = bytes / (1024 * 1024);
  if (mb < 1024) return `${mb.toFixed(1)} MB`;
  const gb = mb / 1024;
  if (gb < 1024) return `${gb.toFixed(1)} GB`;
  return `${(gb / 1024).toFixed(1)} TB`;
}
