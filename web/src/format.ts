// All user-facing formatting lives here so locale choices are made in one place.
// Dates use sv-SE regardless of UI language: ISO-like output (2026-07-20 14:32)
// is easier to scan in a technical tool than en-US ordering.
const DATE_LOCALE = 'sv-SE';

export function formatBytes(n: number | null | undefined): string {
  if (!n) return '0 MB';
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
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

// UploadEntry.filename is a Soulseek virtual path, which is always
// backslash-separated regardless of the host OS that shared it — never
// forward-slash. The full path stays available (e.g. in a `title`
// attribute); this is only for the compact row label.
export function formatVirtualPath(path: string): string {
  const parts = path.split('\\');
  return parts[parts.length - 1];
}
