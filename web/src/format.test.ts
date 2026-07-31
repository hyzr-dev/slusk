import { describe, expect, it } from 'vitest';
import { basename, compareFileNames, formatAge, formatBytes, formatBytesOrDash, formatDateTime, formatDuration, formatEta, formatScore, formatShortTime, formatSize, formatSpeed, formatTime, formatVirtualPath, localDayKey, percent } from './format';

describe('formatBytes', () => {
  it('returns "0 MB" for zero and nullish input', () => {
    expect(formatBytes(0)).toBe('0 MB');
    expect(formatBytes(null)).toBe('0 MB');
    expect(formatBytes(undefined)).toBe('0 MB');
  });

  it('formats megabytes with one decimal', () => {
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    expect(formatBytes(1536 * 1024)).toBe('1.5 MB');
  });

  // Matches the legacy dashboard: everything is expressed in MB, never scaled.
  it('does not scale to GB', () => {
    expect(formatBytes(5 * 1024 * 1024 * 1024)).toBe('5120.0 MB');
  });
});

describe('formatSpeed', () => {
  it('returns an em dash for zero and nullish input, never "0 B/s"', () => {
    expect(formatSpeed(0)).toBe('—');
    expect(formatSpeed(null)).toBe('—');
    expect(formatSpeed(undefined)).toBe('—');
  });

  it('scales to KB/s below 1 MB/s', () => {
    expect(formatSpeed(524288)).toBe('512 KB/s');
  });

  it('scales to MB/s at or above 1 MB/s', () => {
    expect(formatSpeed(1024 * 1024)).toBe('1.0 MB/s');
    expect(formatSpeed(1536 * 1024)).toBe('1.5 MB/s');
  });
});

describe('formatDuration', () => {
  // The whole reason this exists apart from formatEta: a reconcile pass that
  // finishes in a fraction of a second is a measurement, not a missing value,
  // so it must never render as the em dash formatEta gives any falsy input.
  it('reports a sub-second measurement instead of an em dash', () => {
    expect(formatDuration(0)).toBe('0.0 s');
    expect(formatDuration(0.4)).toBe('0.4 s');
    expect(formatEta(0)).toBe('—');
  });

  it('keeps one decimal below ten seconds, where these actually land', () => {
    expect(formatDuration(1.24)).toBe('1.2 s');
    expect(formatDuration(9.96)).toBe('10.0 s');
  });

  it('defers to whole units from ten seconds up', () => {
    expect(formatDuration(12.4)).toBe('12 s');
    expect(formatDuration(90)).toBe('2 min');
  });

  it('returns an em dash only for genuinely unknown input', () => {
    // NaN is an unparseable timestamp; a negative is clock skew between the
    // two stamps. Both are real unknowns, unlike zero.
    expect(formatDuration(Number.NaN)).toBe('—');
    expect(formatDuration(-3)).toBe('—');
    expect(formatDuration(Number.POSITIVE_INFINITY)).toBe('—');
  });
});

describe('formatEta', () => {
  it('returns an em dash for zero and nullish input', () => {
    expect(formatEta(0)).toBe('—');
    expect(formatEta(null)).toBe('—');
    expect(formatEta(undefined)).toBe('—');
  });

  it('shows seconds below a minute', () => {
    expect(formatEta(45)).toBe('45 s');
  });

  it('shows rounded minutes at or above 60 seconds', () => {
    expect(formatEta(60)).toBe('1 min');
    expect(formatEta(150)).toBe('3 min');
  });

  it('shows hours and minutes at or above one hour', () => {
    expect(formatEta(3600)).toBe('1 h');
    expect(formatEta(5400)).toBe('1 h 30 min');
    expect(formatEta(7260)).toBe('2 h 1 min');
  });

  it('carries the rounded remainder instead of reporting 60 minutes', () => {
    expect(formatEta(7199)).toBe('2 h');
    expect(formatEta(86399)).toBe('24 h');
  });
});

describe('formatBytesOrDash', () => {
  it('returns an em dash for zero and nullish input, unlike formatBytes', () => {
    expect(formatBytesOrDash(0)).toBe('—');
    expect(formatBytesOrDash(null)).toBe('—');
    expect(formatBytesOrDash(undefined)).toBe('—');
  });

  it('formats non-zero sizes the same as formatBytes', () => {
    expect(formatBytesOrDash(1024 * 1024)).toBe('1.0 MB');
  });
});

describe('basename', () => {
  it('returns the leaf name for a forward-slash path', () => {
    expect(basename('music/Album/01 Track.flac')).toBe('01 Track.flac');
  });

  it('returns the leaf name for a backslash path', () => {
    expect(basename('music\\Album\\01 Track.flac')).toBe('01 Track.flac');
  });

  it('returns the input unchanged when there is no separator', () => {
    expect(basename('01 Track.flac')).toBe('01 Track.flac');
  });
});

describe('formatSize', () => {
  it('returns "0 MB" for zero and nullish input', () => {
    expect(formatSize(0)).toBe('0 MB');
    expect(formatSize(null)).toBe('0 MB');
    expect(formatSize(undefined)).toBe('0 MB');
  });

  it('formats megabytes with one decimal below 1 GB', () => {
    expect(formatSize(1536 * 1024)).toBe('1.5 MB');
  });

  it('scales to GB at or above 1024 MB', () => {
    expect(formatSize(742 * 1024 * 1024 * 1024)).toBe('742.0 GB');
  });

  it('scales to TB at or above 1024 GB', () => {
    expect(formatSize(1.4 * 1024 * 1024 * 1024 * 1024)).toBe('1.4 TB');
  });
});

describe('formatVirtualPath', () => {
  it('returns the last backslash-separated segment', () => {
    expect(formatVirtualPath('Library\\Radiohead\\Kid A\\01 Everything In Its Right Place.flac')).toBe(
      '01 Everything In Its Right Place.flac',
    );
  });

  it('returns the whole string when there is no separator', () => {
    expect(formatVirtualPath('track.mp3')).toBe('track.mp3');
  });

  it('returns an empty string for empty input', () => {
    expect(formatVirtualPath('')).toBe('');
  });
});

describe('percent', () => {
  it('returns 0 when total is zero', () => {
    expect(percent(100, 0)).toBe(0);
  });

  it('rounds to nearest integer', () => {
    expect(percent(1, 3)).toBe(33);
    expect(percent(2, 3)).toBe(67);
  });

  // Deliberate change from the legacy dashboard, which let the bar overflow.
  it('clamps to 100', () => {
    expect(percent(150, 100)).toBe(100);
  });

  it('clamps negatives to 0', () => {
    expect(percent(-5, 100)).toBe(0);
  });
});

describe('date formatting', () => {
  // sv-SE is kept deliberately: ISO-like dates read better in a technical tool.
  // The exact wall-clock date/time depends on the runner's TZ, so these assert
  // shape only — asserting a literal date broke under TZ=Pacific/Auckland
  // (UTC+12/13), where 2026-07-20T14:32:05Z rolls over to the 21st locally.
  it('formats a full timestamp in sv-SE shape', () => {
    expect(formatDateTime('2026-07-20T14:32:05Z')).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/);
  });

  it('formats short time as HH:MM', () => {
    expect(formatShortTime('2026-07-20T14:32:05Z')).toMatch(/^\d{2}:\d{2}$/);
  });

  it('returns an em dash for empty input rather than "Invalid Date"', () => {
    expect(formatDateTime('')).toBe('—');
    expect(formatShortTime('')).toBe('—');
  });
});

describe('localDayKey', () => {
  // Shape only, for the same TZ-rollover reason as formatDateTime above.
  it('returns a sv-SE calendar date', () => {
    expect(localDayKey('2026-07-20T14:32:05Z')).toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });

  it('gives one key per calendar day, whatever the runner TZ', () => {
    // Built by local calendar arithmetic rather than by subtracting 24h, so
    // the two instants are exactly one local day apart even across a DST
    // transition — which is the property the day dividers rely on.
    const a = new Date('2026-07-20T12:00:00Z');
    const b = new Date(a);
    b.setDate(b.getDate() - 1);
    expect(localDayKey(a.toISOString())).not.toBe(localDayKey(b.toISOString()));
  });

  it('groups two instants on the same local day under one key', () => {
    const morning = new Date('2026-07-20T12:00:00Z');
    const later = new Date(morning.getTime() + 60_000);
    expect(localDayKey(later.toISOString())).toBe(localDayKey(morning.toISOString()));
  });

  it('returns an empty key for empty or unparseable input rather than "Invalid Date"', () => {
    expect(localDayKey('')).toBe('');
    expect(localDayKey('not a date')).toBe('');
  });
});

describe('formatTime', () => {
  it('returns an em dash for empty input', () => {
    expect(formatTime('')).toBe('—');
  });

  // Shape only, for the same TZ-rollover reason as formatDateTime above.
  it('formats time as HH:MM:SS', () => {
    expect(formatTime('2026-07-20T14:32:05Z')).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });
});

describe('formatScore', () => {
  it('always shows two decimals', () => {
    expect(formatScore(1)).toBe('1.00');
    expect(formatScore(0.456)).toBe('0.46');
  });
});

describe('formatAge', () => {
  it('counts seconds below a minute', () => {
    expect(formatAge(0)).toBe('0s');
    expect(formatAge(59)).toBe('59s');
  });

  it('switches to whole minutes rather than counting to hundreds of seconds', () => {
    expect(formatAge(60)).toBe('1m');
    expect(formatAge(3599)).toBe('59m');
  });

  it('carries hours, and drops a zero minute remainder', () => {
    expect(formatAge(3600)).toBe('1h');
    expect(formatAge(3600 + 4 * 60)).toBe('1h 4m');
  });

  it('keeps hours right up to the day boundary', () => {
    expect(formatAge(24 * 3600 - 60)).toBe('23h 59m');
  });

  // Days drop the hour remainder rather than reading "4d 12h". At this scale
  // the hours change no decision — what the column conveys is "this is old" —
  // and the exact instant is in the cell's title instead (issue #333).
  it('switches to whole days at 24h and carries no remainder', () => {
    expect(formatAge(24 * 3600)).toBe('1d');
    expect(formatAge(24 * 3600 + 12 * 3600)).toBe('1d');
    expect(formatAge(4 * 24 * 3600 + 23 * 3600)).toBe('4d');
  });

  // The value from the issue: 108h 23m was unreadable, and this is what it
  // becomes.
  it('renders the reported unreadable case as days', () => {
    expect(formatAge(108 * 3600 + 23 * 60)).toBe('4d');
  });
});

describe('compareFileNames', () => {
  const sorted = (names: string[]) => [...names].sort(compareFileNames);

  it('orders track numbers numerically, not as text', () => {
    // The bug this exists to prevent: plain string sort puts 10 before 2.
    expect(sorted(['10 Ten.flac', '2 Two.flac', '01 One.flac'])).toEqual([
      '01 One.flac',
      '2 Two.flac',
      '10 Ten.flac',
    ]);
  });

  it('falls back to alphabetical when there is no track number', () => {
    expect(sorted(['Zebra.flac', 'Apple.flac', 'Mango.flac'])).toEqual([
      'Apple.flac',
      'Mango.flac',
      'Zebra.flac',
    ]);
  });

  it('keeps discs in order when the number is a prefix', () => {
    expect(sorted(['2-01 B.flac', '1-02 A.flac', '1-01 A.flac'])).toEqual([
      '1-01 A.flac',
      '1-02 A.flac',
      '2-01 B.flac',
    ]);
  });

  it('sorts on the leaf name, ignoring the directories the reader cannot see', () => {
    expect(sorted(['zzz\\01 First.flac', 'aaa\\02 Second.flac'])).toEqual([
      'zzz\\01 First.flac',
      'aaa\\02 Second.flac',
    ]);
  });

  it('places the Swedish vowels after z rather than folding them into a and o', () => {
    expect(sorted(['Ost.flac', 'Apple.flac', 'Ähre.flac'])).toEqual([
      'Apple.flac',
      'Ost.flac',
      'Ähre.flac',
    ]);
  });
});
