# TUI-omstilning av SPA:n — implementationsplan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bygg om hela React-SPA:n till terminalidiomet i `docs/design/slusk-tui.dc.html`, utan backend-ändringar.

**Architecture:** Tokenlagret skrivs om till mockens palett; `Layout.tsx` delas i TopBar / SideNav / StatusBar; nya delade primitiver under `components/tui/` (`Ticks`, `Tag`, `SectionHeader`, `Button`, `Chip`, `EmptyState`) ersätter `ProgressBar`, `StatusPill`, `StatCard`, `SourceBadge` och `PageHeading`; varje vy byggs om mot dessa primitiver med oförändrad datahämtning.

**Tech Stack:** React 19, TypeScript, Vite, CSS Modules, TanStack Query, react-router-dom, Vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-07-25-tui-reskin-design.md`
**Issue:** #198 (tangentbordslagret ligger i #199 och ingår **inte** här)
**Gren:** `feat/ui-tui-reskin-198`, redan skapad från `origin/main`

## Global Constraints

- All användartext går genom `web/src/strings.ts`. Komponenter får aldrig inline:a text. Detta är i18n-förberedelse (#86).
- All formatering av tal, datum, bytes, hastighet och ETA går genom `web/src/format.ts`. Datum använder locale `sv-SE`; UI-språket är engelska.
- Inga backend-ändringar. Inga nya API-anrop utöver de hooks som redan finns i `web/src/api/queries.ts`.
- Inga nya beroenden i `web/package.json`. Diagram är handrullad SVG (YAGNI-beslutet från #88 står fast).
- CSS Modules genomgående. Inline `style` bara för genuint dynamiska värden (bredd i procent, rutnätskolumner).
- **Inga tangentglyfer i UI:t.** Inga siffror i navposter, inga `[r]`-prefix på knappar, ingen tips-sektion i statusraden. De hör till #199 och landar med bindningarna.
- Inga rundade hörn. `--radius` och `--radius-sm` finns inte kvar efter Task 1.
- Kommentarer och identifierare på engelska. Exporterade symboler får doc-kommentar som förklarar *varför*, inte vad signaturen redan säger.
- Efter varje task ska `npm test` och `npm run build` (körda i `web/`) vara gröna innan commit.
- Commit-ämne: `<type>: <description> (#198)`.

---

### Task 1: Tokenlager och global stil

**Files:**
- Modify: `web/src/styles/tokens.css` (ersätt hela filen)
- Modify: `web/src/styles/global.css` (ersätt hela filen)
- Modify: `web/index.html` (Google Fonts-länken)

**Interfaces:**
- Consumes: inget
- Produces: CSS-variablerna nedan på `:root`. Alla senare tasks refererar dessa namn och inga andra. `--font-sans`, `--radius`, `--radius-sm`, `--panel`, `--panel-raised`, `--border`, `--border-subtle`, `--text`, `--text-muted`, `--text-detail`, `--accent`, `--done`, `--active`, `--queued`, `--stalled`, `--orphaned`, `--failed`, `--manual` och deras `-bg`-varianter finns **inte** kvar.

- [ ] **Step 1: Ersätt `web/src/styles/tokens.css`**

```css
/* Palette from the TUI design mock (docs/design/slusk-tui.dc.html, the
   rootStyle object). The mock has no per-status hue: queued, active and
   importing all render in --fg/--dim, and only OK and error carry a color.
   The status colors that used to live here were retired with that decision —
   see docs/superpowers/specs/2026-07-25-tui-reskin-design.md. */
:root {
  --bg: #08090a;

  --fg: #e8eaec;
  --dim: #7d868c;
  --faint: #78828a;
  /* The quietest readable step. The mock uses this raw hex in ~30 places for
     column headers, timestamps and disabled glyphs; it is named here rather
     than repeated. */
  --text-dim: #5f696f;

  --line: #191d20;
  --line2: #252b30;

  --ok: #4f9e80;
  --bad: #b5595c;
  /* Fill color for progress ticks. Deliberately not --fg: a full bar of --fg
     next to --fg text has no contrast, and the mock draws bars one step down. */
  --bar: #c8ccd0;

  --panel-hover: #0e1012;
  /* Recessed surface for nested disclosure content — expanded job and result
     rows. Darker than --bg, not lighter: the mock recesses, never elevates. */
  --panel-inset: #0b0c0e;
  --nav-active: #111417;
  --btn: #14181b;

  --tick-off: #1d2125;
  --tick-queued: #2a2f34;

  --font-mono: 'IBM Plex Mono', ui-monospace, monospace;
}
```

- [ ] **Step 2: Ersätt `web/src/styles/global.css`**

```css
@import './tokens.css';

*, *::before, *::after { box-sizing: border-box; }

/* The TUI is a fixed 100vh frame: the header, sidebar and status bar never
   scroll, only <main> does. Locking overflow here stops a long view from
   scrolling the whole frame out of sight. */
html, body { height: 100%; overflow: hidden; }

body {
  margin: 0;
  background: var(--bg);
  color: var(--fg);
  font-family: var(--font-mono);
  font-feature-settings: 'tnum' 1;
  font-variant-numeric: tabular-nums;
}

::selection { background: rgba(255, 255, 255, 0.16); }
::-webkit-scrollbar { width: 8px; height: 8px; }
::-webkit-scrollbar-track { background: var(--bg); }
::-webkit-scrollbar-thumb { background: #1e2226; }
::-webkit-scrollbar-thumb:hover { background: #2c3237; }

input, select, button { font-family: inherit; }

/* The status dot in the top bar. Kept global because it is referenced from
   two chrome components that have no other styling in common. */
@keyframes tui-pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.25; } }

/* The newest filled tick in a live progress bar flashes white and stretches
   for one beat, so a moving transfer is visible without reading the number. */
@keyframes tui-flare {
  0% { background: #ffffff; transform: scaleY(1.55); }
  100% { transform: scaleY(1); }
}
```

- [ ] **Step 3: Ta bort IBM Plex Sans ur `web/index.html`**

Hitta raden som laddar typsnitt från `fonts.googleapis.com` och byt familjelistan så att bara mono finns kvar:

```html
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600;700&display=swap" rel="stylesheet">
```

- [ ] **Step 4: Kör bygget och testerna**

Run: `cd web && npm run build && npm test`
Expected: PASS. Bygget får inte klaga på saknade variabler — CSS-variabler misslyckas tyst, så det här steget bevisar bara att inget *kompilerar* sönder. Vyerna ser trasiga ut tills Task 5–13 är klara; det är väntat.

- [ ] **Step 5: Commit**

```bash
git add web/src/styles/tokens.css web/src/styles/global.css web/index.html
git commit -m "refactor(ui): TUI-palett och monospace-typsnitt (#198)"
```

---

### Task 2: Ticks-primitiven

Ersätter `ProgressBar`. Mockens matte ligger i `ticks()` rad 820–833 i designfilen.

**Files:**
- Create: `web/src/components/tui/Ticks.tsx`
- Create: `web/src/components/tui/Ticks.module.css`
- Test: `web/src/components/tui/Ticks.test.tsx`

**Interfaces:**
- Consumes: tokens från Task 1 (`--bar`, `--ok`, `--bad`, `--tick-off`, `--tick-queued`)
- Produces:
  ```ts
  export type TickState = 'on' | 'partial' | 'off';
  export type TickTone = 'bar' | 'ok' | 'bad' | 'queued';
  export function tickStates(percent: number, count: number): TickState[];
  export default function Ticks(props: {
    percent: number;      // 0–100, clamped
    count: number;        // number of ticks to draw
    tone?: TickTone;      // default 'bar'
    live?: boolean;       // flare the newest filled tick, default false
    height?: number;      // px, default 12
  }): JSX.Element;
  ```

- [ ] **Step 1: Skriv det fallerande testet**

Create `web/src/components/tui/Ticks.test.tsx`:

```tsx
import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import Ticks, { tickStates } from './Ticks';

describe('tickStates', () => {
  it('marks every tick off at 0 percent', () => {
    expect(tickStates(0, 4)).toEqual(['off', 'off', 'off', 'off']);
  });

  it('marks every tick on at 100 percent', () => {
    expect(tickStates(100, 4)).toEqual(['on', 'on', 'on', 'on']);
  });

  it('marks the tick straddling the boundary as partial', () => {
    // 3 of 8 ticks filled exactly, so no tick straddles the edge.
    expect(tickStates(37.5, 8)).toEqual([
      'on', 'on', 'on', 'off', 'off', 'off', 'off', 'off',
    ]);
    // 3.2 ticks filled: the fourth is partially covered.
    expect(tickStates(40, 8)).toEqual([
      'on', 'on', 'on', 'partial', 'off', 'off', 'off', 'off',
    ]);
  });

  it('clamps out-of-range input rather than drawing extra or negative ticks', () => {
    expect(tickStates(140, 3)).toEqual(['on', 'on', 'on']);
    expect(tickStates(-20, 3)).toEqual(['off', 'off', 'off']);
  });
});

describe('Ticks', () => {
  it('renders exactly count elements', () => {
    const { container } = render(<Ticks percent={50} count={26} />);
    expect(container.querySelectorAll('[data-tick]')).toHaveLength(26);
  });

  it('flares only the newest filled tick, and only when live', () => {
    const live = render(<Ticks percent={50} count={4} live />);
    expect(live.container.querySelectorAll('[data-flare="true"]')).toHaveLength(1);

    const still = render(<Ticks percent={50} count={4} />);
    expect(still.container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  it('does not flare at 0 percent, where there is no filled tick', () => {
    const { container } = render(<Ticks percent={0} count={4} live />);
    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });
});
```

- [ ] **Step 2: Kör testet och bekräfta att det fallerar**

Run: `cd web && npx vitest run src/components/tui/Ticks.test.tsx`
Expected: FAIL — `Failed to resolve import "./Ticks"`.

- [ ] **Step 3: Skriv implementationen**

Create `web/src/components/tui/Ticks.module.css`:

```css
.row { display: flex; gap: 1px; align-items: flex-end; }

.tick { flex: 1; min-width: 1px; height: 100%; }

.flare { animation: tui-flare 0.9s ease-out; }
```

Create `web/src/components/tui/Ticks.tsx`:

```tsx
import { memo } from 'react';
import styles from './Ticks.module.css';

export type TickState = 'on' | 'partial' | 'off';
export type TickTone = 'bar' | 'ok' | 'bad' | 'queued';

const TONE_COLOR: Record<TickTone, string> = {
  bar: 'var(--bar)',
  ok: 'var(--ok)',
  bad: 'var(--bad)',
  queued: 'var(--tick-queued)',
};

/**
 * Which ticks are lit for a given fill percentage. Exported separately from
 * the component because the boundary behaviour — the single tick that
 * straddles the fill edge renders half-lit — is the only part worth testing,
 * and testing it through the DOM would mean asserting on inline styles.
 */
export function tickStates(percent: number, count: number): TickState[] {
  const clamped = Math.min(100, Math.max(0, percent));
  const filled = (clamped / 100) * count;
  const full = Math.floor(filled);
  return Array.from({ length: count }, (_, i) => {
    if (i < full) return 'on';
    if (i < filled) return 'partial';
    return 'off';
  });
}

interface Props {
  percent: number;
  count: number;
  tone?: TickTone;
  live?: boolean;
  height?: number;
}

function TicksImpl({ percent, count, tone = 'bar', live = false, height = 12 }: Props) {
  const states = tickStates(percent, count);
  // The head is the last fully lit tick. At 0% there is none, so nothing
  // flares — an idle transfer must not look like a moving one.
  const head = states.lastIndexOf('on');
  const color = TONE_COLOR[tone];

  return (
    <div className={styles.row} style={{ height }}>
      {states.map((state, i) => {
        const flare = live && i === head && head >= 0;
        return (
          <span
            key={i}
            data-tick
            data-flare={flare ? 'true' : undefined}
            className={flare ? styles.flare : undefined}
            style={{
              background: state === 'off' ? 'var(--tick-off)' : color,
              opacity: state === 'partial' ? 0.5 : 1,
            }}
          />
        );
      })}
    </div>
  );
}

/**
 * A dense bar of uniform ticks that recolour in place as a transfer advances.
 *
 * Memoised on the *integer* number of lit ticks rather than on `percent`: the
 * jobs list polls every 3 s and can hold ~150 rows of 26 ticks each, so a job
 * creeping from 41.2 % to 41.4 % must not repaint ~3900 nodes for a bar that
 * looks identical.
 */
const Ticks = memo(TicksImpl, (prev, next) => {
  const lit = (p: Props) => Math.floor((Math.min(100, Math.max(0, p.percent)) / 100) * p.count);
  return (
    lit(prev) === lit(next) &&
    prev.count === next.count &&
    prev.tone === next.tone &&
    prev.live === next.live &&
    prev.height === next.height
  );
});

export default Ticks;
```

- [ ] **Step 4: Kör testet och bekräfta att det passerar**

Run: `cd web && npx vitest run src/components/tui/Ticks.test.tsx`
Expected: PASS, 7 tester.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/tui/Ticks.tsx web/src/components/tui/Ticks.module.css web/src/components/tui/Ticks.test.tsx
git commit -m "feat(ui): Ticks-primitiv ersätter progressbaren (#198)"
```

---

### Task 3: Tag-primitiven

Ersätter `StatusPill`. Mockens mappning ligger i `meta()` rad 854–858.

Notera att `JobStatus` inte har något `importing` — mocken har det, men hos oss är importering ett `JobState` (`IMPORTING`). Och kötillståndet, som `StatusPill` inte kunde uttrycka, härleds ur `queuePosition`. Det var precis läckan #190 beskrev: `Jobs.tsx` importerar `StatusPill.module.css` för att rita en kö-pill för hand. `Tag` tar båda som riktiga props.

**Files:**
- Create: `web/src/components/tui/Tag.tsx`
- Create: `web/src/components/tui/Tag.module.css`
- Test: `web/src/components/tui/Tag.test.tsx`
- Modify: `web/src/strings.ts` (nytt `tag`-block)

**Interfaces:**
- Consumes: `JobStatus`, `JobState` från `web/src/api/types.ts`
- Produces:
  ```ts
  export type TagKind = 'DL' | 'QU' | 'ST' | 'OR' | 'FA' | 'OK' | 'IM';
  export function tagFor(status: JobStatus, state: JobState, queuePosition?: number): TagKind;
  export default function Tag(props: {
    status: JobStatus;
    state: JobState;
    queuePosition?: number;
  }): JSX.Element;
  ```
  `tagFor` används även av Task 6 för att välja `TickTone` per rad.

- [ ] **Step 1: Lägg till strängarna**

I `web/src/strings.ts`, efter `status`-blocket, lägg till:

```ts
  // Two-letter status tags in the TUI job grid. The long labels in `status`
  // and `state` are still used wherever there is room for them.
  tag: {
    DL: 'DL',
    QU: 'QU',
    ST: 'ST',
    OR: 'OR',
    FA: 'FA',
    OK: 'OK',
    IM: 'IM',
  },
  tagTitle: {
    DL: 'Downloading',
    QU: 'Queued',
    ST: 'Stalled',
    OR: 'Orphaned',
    FA: 'Failed',
    OK: 'Done',
    IM: 'Importing',
  },
```

- [ ] **Step 2: Skriv det fallerande testet**

Create `web/src/components/tui/Tag.test.tsx`:

```tsx
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Tag, { tagFor } from './Tag';

describe('tagFor', () => {
  it('maps each job status to its tag', () => {
    expect(tagFor('queued', 'WANTED')).toBe('QU');
    expect(tagFor('active', 'DOWNLOADING')).toBe('DL');
    expect(tagFor('stalled', 'DOWNLOADING')).toBe('ST');
    expect(tagFor('done', 'DONE')).toBe('OK');
    expect(tagFor('failed', 'FAILED')).toBe('FA');
    expect(tagFor('orphaned', 'ORPHANED')).toBe('OR');
  });

  it('reports importing from the state, which the status cannot express', () => {
    expect(tagFor('active', 'IMPORTING')).toBe('IM');
  });

  it('reports a job waiting in a peer queue as queued, not downloading', () => {
    // This is the case StatusPill could not express (issue #190): the job is
    // active, but no bytes move until the peer reaches us in its own queue.
    expect(tagFor('active', 'DOWNLOADING', 4)).toBe('QU');
    expect(tagFor('active', 'DOWNLOADING', 0)).toBe('DL');
    expect(tagFor('active', 'DOWNLOADING', undefined)).toBe('DL');
  });

  it('does not let a peer queue position override a terminal status', () => {
    expect(tagFor('done', 'DONE', 3)).toBe('OK');
    expect(tagFor('failed', 'FAILED', 3)).toBe('FA');
  });
});

describe('Tag', () => {
  it('renders the tag text with a readable title', () => {
    render(<Tag status="stalled" state="DOWNLOADING" />);
    const el = screen.getByText('ST');
    expect(el).toHaveAttribute('title', 'Stalled');
  });
});
```

- [ ] **Step 3: Kör testet och bekräfta att det fallerar**

Run: `cd web && npx vitest run src/components/tui/Tag.test.tsx`
Expected: FAIL — `Failed to resolve import "./Tag"`.

- [ ] **Step 4: Skriv implementationen**

Create `web/src/components/tui/Tag.module.css`:

```css
.tag {
  font-size: 9.5px;
  letter-spacing: 1px;
  padding: 1px 5px;
  border: 1px solid var(--line2);
  white-space: nowrap;
}

.bordered { border: 1px solid var(--line2); }
.bare { border: none; padding: 0; }

.neutral { color: var(--fg); }
.quiet { color: var(--faint); }
.ok { color: var(--ok); }
.bad { color: var(--bad); }
```

Create `web/src/components/tui/Tag.tsx`:

```tsx
import type { JobState, JobStatus } from '../../api/types';
import { t } from '../../strings';
import styles from './Tag.module.css';

export type TagKind = 'DL' | 'QU' | 'ST' | 'OR' | 'FA' | 'OK' | 'IM';

const BY_STATUS: Record<JobStatus, TagKind> = {
  queued: 'QU',
  active: 'DL',
  stalled: 'ST',
  done: 'OK',
  failed: 'FA',
  orphaned: 'OR',
};

const TONE: Record<TagKind, string> = {
  DL: styles.neutral,
  IM: styles.neutral,
  QU: styles.quiet,
  OK: styles.ok,
  ST: styles.bad,
  FA: styles.bad,
  OR: styles.bad,
};

/**
 * The two-letter tag for a job row.
 *
 * Reads three inputs because no single one of them is sufficient: `status` is
 * the coarse bucket, `state` is the only place importing appears, and a
 * non-zero `queuePosition` means an "active" job is in fact waiting in a
 * peer's queue with no bytes moving. Terminal statuses ignore the queue
 * position — a finished job may still carry a stale one.
 */
export function tagFor(
  status: JobStatus,
  state: JobState,
  queuePosition?: number,
): TagKind {
  if (status === 'active') {
    if (state === 'IMPORTING') return 'IM';
    if (queuePosition && queuePosition > 0) return 'QU';
  }
  return BY_STATUS[status] ?? 'QU';
}

interface Props {
  status: JobStatus;
  state: JobState;
  queuePosition?: number;
  /** Omit the border, for dense grids where the box is visual noise. */
  bare?: boolean;
}

export default function Tag({ status, state, queuePosition, bare = false }: Props) {
  const kind = tagFor(status, state, queuePosition);
  return (
    <span
      className={`${bare ? styles.bare : styles.tag} ${TONE[kind]}`}
      title={t.tagTitle[kind]}
    >
      {t.tag[kind]}
    </span>
  );
}
```

- [ ] **Step 5: Kör testet och bekräfta att det passerar**

Run: `cd web && npx vitest run src/components/tui/Tag.test.tsx`
Expected: PASS, 6 tester.

- [ ] **Step 6: Commit**

```bash
git add web/src/components/tui/Tag.tsx web/src/components/tui/Tag.module.css web/src/components/tui/Tag.test.tsx web/src/strings.ts
git commit -m "feat(ui): Tag-primitiv med kötillstånd som riktig prop (#198, #190)"
```

---

### Task 4: Övriga TUI-primitiver

Rena presentationskomponenter utan logik. De testas genom de vyer som använder dem (Task 5–13); egna tester här vore tester på JSX-struktur och skulle bara låsa fast markup.

**Files:**
- Create: `web/src/components/tui/SectionHeader.tsx` + `.module.css`
- Create: `web/src/components/tui/Button.tsx` + `.module.css`
- Create: `web/src/components/tui/Chip.tsx` + `.module.css`
- Create: `web/src/components/tui/EmptyState.tsx` + `.module.css`

**Interfaces:**
- Consumes: tokens från Task 1
- Produces:
  ```ts
  export default function SectionHeader(props: {
    label: string;
    meta?: ReactNode;   // right-aligned, quiet
  }): JSX.Element;

  export default function Button(props: {
    variant?: 'primary' | 'ghost' | 'danger';   // default 'ghost'
    onClick?: () => void;
    disabled?: boolean;
    type?: 'button' | 'submit';
    children: ReactNode;
  }): JSX.Element;

  export default function Chip(props: {
    label: string;
    count?: number;
    active?: boolean;
    onClick: () => void;
  }): JSX.Element;

  export default function EmptyState(props: { message: string }): JSX.Element;
  ```

- [ ] **Step 1: Skriv `SectionHeader`**

`web/src/components/tui/SectionHeader.module.css`:

```css
.header {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 16px;
  border-bottom: 1px solid var(--line);
  font-size: 9.5px;
  letter-spacing: 1.4px;
  color: var(--faint);
}

.spacer { flex: 1; }
.meta { color: var(--dim); letter-spacing: 0.4px; }
```

`web/src/components/tui/SectionHeader.tsx`:

```tsx
import type { ReactNode } from 'react';
import styles from './SectionHeader.module.css';

/**
 * The rule that opens every panel: an all-caps label on the left and quiet
 * meta on the right. `label` is expected to arrive already upper-cased from
 * strings.ts rather than transformed here, so a translation can opt out of
 * casing that does not survive in its script.
 */
export default function SectionHeader({ label, meta }: { label: string; meta?: ReactNode }) {
  return (
    <div className={styles.header}>
      <span>{label}</span>
      <span className={styles.spacer} />
      {meta ? <span className={styles.meta}>{meta}</span> : null}
    </div>
  );
}
```

- [ ] **Step 2: Skriv `Button`**

`web/src/components/tui/Button.module.css`:

```css
.btn {
  padding: 5px 11px;
  font-size: 10.5px;
  letter-spacing: 0.7px;
  cursor: pointer;
  background: transparent;
}

.btn:disabled { cursor: default; opacity: 0.45; }

.primary { border: 1px solid var(--dim); background: var(--btn); color: var(--fg); }
.ghost { border: 1px solid var(--line2); color: var(--dim); }
.ghost:hover:not(:disabled) { border-color: var(--dim); color: var(--fg); }
.danger { border: 1px solid rgba(181, 89, 92, 0.4); color: var(--bad); }
.danger:hover:not(:disabled) { border-color: var(--bad); }
```

`web/src/components/tui/Button.tsx`:

```tsx
import type { ReactNode } from 'react';
import styles from './Button.module.css';

interface Props {
  variant?: 'primary' | 'ghost' | 'danger';
  onClick?: () => void;
  disabled?: boolean;
  type?: 'button' | 'submit';
  children: ReactNode;
}

export default function Button({
  variant = 'ghost',
  onClick,
  disabled = false,
  type = 'button',
  children,
}: Props) {
  return (
    <button
      type={type}
      className={`${styles.btn} ${styles[variant]}`}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </button>
  );
}
```

- [ ] **Step 3: Skriv `Chip`**

`web/src/components/tui/Chip.module.css`:

```css
.chip {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 10px;
  border: 1px solid var(--line2);
  background: transparent;
  color: var(--faint);
  font-size: 10.5px;
  letter-spacing: 0.7px;
  cursor: pointer;
}

.active { border-color: var(--dim); background: #14181b; color: var(--fg); }

.count { color: var(--text-dim); }
.activeCount { color: var(--dim); }
```

`web/src/components/tui/Chip.tsx`:

```tsx
import styles from './Chip.module.css';

interface Props {
  label: string;
  count?: number;
  active?: boolean;
  onClick: () => void;
}

/**
 * A filter or sort toggle. `aria-pressed` rather than a radio group: the
 * chips in the jobs view are a single-select filter, but the sort chips in a
 * later view are not, and one component serving both keeps the visual
 * treatment identical.
 */
export default function Chip({ label, count, active = false, onClick }: Props) {
  return (
    <button
      type="button"
      aria-pressed={active}
      className={`${styles.chip} ${active ? styles.active : ''}`}
      onClick={onClick}
    >
      {label}
      {count === undefined ? null : (
        <span className={active ? styles.activeCount : styles.count}>{count}</span>
      )}
    </button>
  );
}
```

- [ ] **Step 4: Skriv `EmptyState`**

`web/src/components/tui/EmptyState.module.css`:

```css
.empty {
  padding: 40px;
  text-align: center;
  color: var(--text-dim);
  font-size: 11.5px;
  letter-spacing: 1px;
}
```

`web/src/components/tui/EmptyState.tsx`:

```tsx
import styles from './EmptyState.module.css';

/**
 * The `── NOTHING HERE ──` rule. The dashes are added here rather than baked
 * into every string in strings.ts so the decoration stays a styling decision.
 */
export default function EmptyState({ message }: { message: string }) {
  return <div className={styles.empty}>{`── ${message} ──`}</div>;
}
```

- [ ] **Step 5: Kör bygget**

Run: `cd web && npx tsc --noEmit && npm test`
Expected: PASS. Inga nya tester, men typkontrollen bevisar att komponenterna kompilerar.

- [ ] **Step 6: Commit**

```bash
git add web/src/components/tui
git commit -m "feat(ui): SectionHeader, Button, Chip och EmptyState (#198)"
```

---

### Task 5: Global chrome och flash

Delar `Layout.tsx` i tre och inför flash-mekanismen som Task 6 och 9 behöver.

**Files:**
- Create: `web/src/components/chrome/FlashContext.tsx`
- Create: `web/src/components/chrome/TopBar.tsx` + `.module.css`
- Create: `web/src/components/chrome/SideNav.tsx` + `.module.css`
- Create: `web/src/components/chrome/StatusBar.tsx` + `.module.css`
- Test: `web/src/components/chrome/SideNav.test.tsx`
- Test: `web/src/components/chrome/StatusBar.test.tsx`
- Modify: `web/src/components/Layout.tsx` (ersätt hela filen)
- Modify: `web/src/components/Layout.module.css` (ersätt hela filen)
- Modify: `web/src/strings.ts` (nya `nav`-poster och `chrome`-block)
- Modify: `web/src/App.tsx` (nya rutter `search`, `chat`, `setup`)

**Interfaces:**
- Consumes: `Ticks`? nej. `useStatus`, `useJobs`, `useCharts` från `web/src/api/queries.ts`
- Produces:
  ```ts
  // FlashContext.tsx
  export function FlashProvider(props: { children: ReactNode }): JSX.Element;
  export function useFlash(): (message: string) => void;
  export function useFlashMessage(): string | null;   // consumed by StatusBar only

  // SideNav.tsx
  export interface NavItem { to: string; label: string; end?: boolean; badge?: number; alert?: boolean }
  export interface NavGroup { label: string; items: NavItem[] }
  export default function SideNav(props: { groups: NavGroup[] }): JSX.Element;

  // TopBar.tsx / StatusBar.tsx take no props; they read their own queries.
  ```

- [ ] **Step 1: Lägg till strängarna**

I `web/src/strings.ts`, ersätt `nav`-blocket och lägg till `chrome`:

```ts
  nav: {
    overview: 'overview',
    jobs: 'jobs',
    events: 'events',
    peers: 'peers',
    health: 'health',
    search: 'search',
    shares: 'shares',
    chat: 'chat',
    setup: 'setup',
    settings: 'config',
    groupMonitor: 'MONITOR',
    groupSoulseek: 'SOULSEEK',
    groupSystem: 'SYSTEM',
  },
  chrome: {
    live: 'LIVE',
    updatedNow: 'now',
    updatedAgo: (seconds: number) => `${seconds}s`,
    reconcile: 'RECONCILE',
    reconcileNever: '—',
    reconcileCountdown: (seconds: number) => `T–${seconds}s`,
    down: 'DOWN',
    up: 'UP',
    idle: 'idle',
    depLidarr: 'LIDARR',
    depSoulseek: 'SOULSEEK',
    depShares: 'SHARES',
  },
```

- [ ] **Step 2: Skriv `FlashContext`**

`web/src/components/chrome/FlashContext.tsx`:

```tsx
import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';

const SetContext = createContext<(message: string) => void>(() => {});
const ValueContext = createContext<string | null>(null);

const FLASH_MS = 3200;

/**
 * Transient confirmations for actions that have no other visible result —
 * "cancelled #2291" after a mutation the row itself does not immediately
 * reflect, because the next poll is up to 3 s away.
 *
 * Setter and value live in separate contexts so a component that only fires
 * flashes (every mutation button) does not re-render when a message appears.
 */
export function FlashProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const flash = useCallback((next: string) => {
    if (timer.current) clearTimeout(timer.current);
    setMessage(next);
    timer.current = setTimeout(() => setMessage(null), FLASH_MS);
  }, []);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  return (
    <SetContext.Provider value={flash}>
      <ValueContext.Provider value={message}>{children}</ValueContext.Provider>
    </SetContext.Provider>
  );
}

export function useFlash() {
  return useContext(SetContext);
}

export function useFlashMessage() {
  return useContext(ValueContext);
}
```

- [ ] **Step 3: Skriv `SideNav` och dess test**

Create `web/src/components/chrome/SideNav.test.tsx`:

```tsx
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import SideNav from './SideNav';

const groups = [
  {
    label: 'MONITOR',
    items: [
      { to: '/', label: 'overview', end: true },
      { to: '/jobs', label: 'jobs', badge: 12 },
      { to: '/health', label: 'health', badge: 3, alert: true },
      { to: '/peers', label: 'peers', badge: 0 },
    ],
  },
];

describe('SideNav', () => {
  it('renders a badge when the count is above zero', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    expect(screen.getByText('12')).toBeInTheDocument();
  });

  it('hides a zero badge rather than drawing a 0', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    // 'peers' has badge 0; nothing in the nav should render the digit.
    expect(screen.queryByText('0')).not.toBeInTheDocument();
  });

  it('marks an alerting badge so it can be styled apart', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    expect(screen.getByText('3')).toHaveAttribute('data-alert', 'true');
  });

  it('renders each group label', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    expect(screen.getByText('MONITOR')).toBeInTheDocument();
  });
});
```

Run: `cd web && npx vitest run src/components/chrome/SideNav.test.tsx` → FAIL (import saknas).

`web/src/components/chrome/SideNav.module.css`:

```css
.nav {
  width: 178px;
  flex: 0 0 178px;
  border-right: 1px solid var(--line);
  display: flex;
  flex-direction: column;
  padding: 6px 0;
  overflow-y: auto;
}

.group {
  font-size: 9.5px;
  color: var(--text-dim);
  letter-spacing: 1.4px;
  padding: 13px 15px 6px;
}

.item, .itemActive {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 6px 15px;
  font-size: 12px;
  letter-spacing: 0.4px;
  text-decoration: none;
}

.item { color: var(--dim); }
.item:hover { color: var(--fg); background: var(--panel-hover); }

.itemActive {
  background: var(--nav-active);
  color: var(--fg);
  box-shadow: inset 2px 0 0 var(--fg);
}

.label { flex: 1; text-align: left; }
.badge { font-size: 10px; color: var(--text-dim); }
.badgeAlert { font-size: 10px; color: var(--bad); }
```

`web/src/components/chrome/SideNav.tsx`:

```tsx
import { NavLink } from 'react-router-dom';
import styles from './SideNav.module.css';

export interface NavItem {
  to: string;
  label: string;
  end?: boolean;
  badge?: number;
  /** Colour the badge as a problem rather than a count. */
  alert?: boolean;
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

/**
 * The grouped sidebar. Badges are suppressed at zero rather than rendered as
 * "0" — a nav full of zeros reads as broken, and the absence of a number is
 * the same information.
 *
 * Keyboard hints (the digit next to each entry in the design mock) are
 * deliberately absent until the bindings exist; see issue #199.
 */
export default function SideNav({ groups }: { groups: NavGroup[] }) {
  return (
    <nav className={styles.nav}>
      {groups.map((group) => (
        <div key={group.label}>
          <div className={styles.group}>{group.label}</div>
          {group.items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? styles.itemActive : styles.item)}
            >
              <span className={styles.label}>{item.label}</span>
              {item.badge ? (
                <span
                  className={item.alert ? styles.badgeAlert : styles.badge}
                  data-alert={item.alert ? 'true' : undefined}
                >
                  {item.badge}
                </span>
              ) : null}
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  );
}
```

Run: `cd web && npx vitest run src/components/chrome/SideNav.test.tsx` → PASS, 4 tester.

- [ ] **Step 4: Skriv `StatusBar` och dess test**

Create `web/src/components/chrome/StatusBar.test.tsx`:

```tsx
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { FlashProvider, useFlash } from './FlashContext';
import StatusBar from './StatusBar';

function Trigger() {
  const flash = useFlash();
  return <button onClick={() => flash('cancelled #2291')}>fire</button>;
}

describe('StatusBar', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('shows a flash message and clears it again', () => {
    render(
      <FlashProvider>
        <Trigger />
        <StatusBar />
      </FlashProvider>,
    );

    expect(screen.queryByText(/cancelled/)).not.toBeInTheDocument();

    act(() => { screen.getByText('fire').click(); });
    expect(screen.getByText(/cancelled #2291/)).toBeInTheDocument();

    act(() => { vi.advanceTimersByTime(3300); });
    expect(screen.queryByText(/cancelled/)).not.toBeInTheDocument();
  });
});
```

Run it → FAIL.

`web/src/components/chrome/StatusBar.module.css`:

```css
.bar {
  display: flex;
  align-items: center;
  border-top: 1px solid var(--line);
  flex: 0 0 auto;
  font-size: 10.5px;
}

.spacer { flex: 1; }

.flash {
  padding: 7px 13px;
  color: var(--ok);
  letter-spacing: 0.4px;
  border-left: 1px solid var(--line);
}

.clock {
  padding: 7px 14px;
  color: var(--text-dim);
  letter-spacing: 0.5px;
  border-left: 1px solid var(--line);
}
```

`web/src/components/chrome/StatusBar.tsx`:

```tsx
import { useEffect, useState } from 'react';
import { useFlashMessage } from './FlashContext';
import styles from './StatusBar.module.css';

function useClock(): string {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(id);
  }, []);
  return [now.getHours(), now.getMinutes(), now.getSeconds()]
    .map((n) => String(n).padStart(2, '0'))
    .join(':');
}

/**
 * The bottom rule: transient confirmations and a wall clock.
 *
 * The key-hint section from the design mock is absent on purpose — it would
 * advertise bindings that do not exist yet. It returns with issue #199.
 */
export default function StatusBar() {
  const flash = useFlashMessage();
  const clock = useClock();

  return (
    <div className={styles.bar}>
      <span className={styles.spacer} />
      {flash ? <span className={styles.flash}>{`✓ ${flash}`}</span> : null}
      <span className={styles.clock}>{clock}</span>
    </div>
  );
}
```

Run the test → PASS.

- [ ] **Step 5: Skriv `TopBar`**

`web/src/components/chrome/TopBar.module.css`:

```css
.bar {
  display: flex;
  align-items: center;
  border-bottom: 1px solid var(--line);
  flex: 0 0 auto;
}

.cell {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 11px 16px;
  border-right: 1px solid var(--line);
  font-size: 11px;
  color: var(--faint);
  letter-spacing: 0.6px;
  white-space: nowrap;
}

.brand { display: flex; align-items: baseline; gap: 10px; }
.brandName { color: var(--fg); font-weight: 600; font-size: 13px; letter-spacing: 1.5px; }

.dot { width: 6px; height: 6px; background: var(--ok); animation: tui-pulse 1.8s infinite; }
.dotStale { width: 6px; height: 6px; background: var(--bad); }

.spacer { flex: 1; }

.deps {
  padding: 11px 16px;
  font-size: 11px;
  color: var(--faint);
  letter-spacing: 0.6px;
  border-left: 1px solid var(--line);
  display: flex;
  gap: 12px;
}

.value { color: var(--fg); }
.quiet { color: var(--dim); }
.ok { color: var(--ok); }
.bad { color: var(--bad); }
```

`web/src/components/chrome/TopBar.tsx`:

```tsx
import { useCharts, useJobs, useStatus } from '../../api/queries';
import { formatSpeed } from '../../format';
import { t } from '../../strings';
import styles from './TopBar.module.css';

/**
 * Aggregate download speed across jobs that are actually moving bytes.
 *
 * A job with a peer queue position is "active" but transferring nothing, and
 * counting its stale `speed` would inflate the headline figure.
 */
function totalSpeed(jobs: { status: string; speed?: number; queuePosition?: number }[]): number {
  return jobs
    .filter((j) => j.status === 'active' && !j.queuePosition)
    .reduce((sum, j) => sum + (j.speed ?? 0), 0);
}

export default function TopBar() {
  const jobs = useJobs();
  const status = useStatus();
  const charts = useCharts();

  const down = totalSpeed(jobs.data ?? []);
  const modules = status.data?.modules ?? {};
  // moduleDetails carries a `live` flag per pipeline module; the top bar only
  // needs the coarse verdict, and an absent module reads as unknown, not down.
  const depTone = (name: string) => {
    const state = modules[name];
    if (state === undefined) return styles.quiet;
    return state === 'ok' ? styles.ok : styles.bad;
  };

  const lastPass = charts.data?.passes?.at(-1);

  return (
    <div className={styles.bar}>
      <div className={`${styles.cell} ${styles.brand}`}>
        <span className={styles.brandName}>{t.app.name.toUpperCase()}</span>
      </div>

      <div className={styles.cell}>
        <span className={status.isError ? styles.dotStale : styles.dot} />
        <span className={styles.quiet}>{t.chrome.live}</span>
        <span>{status.isFetching ? t.chrome.updatedNow : ''}</span>
      </div>

      <div className={styles.cell}>
        {t.chrome.reconcile}{' '}
        <span className={styles.quiet}>
          {lastPass ? formatShortTime(lastPass.finishedAt) : t.chrome.reconcileNever}
        </span>
      </div>

      <div className={styles.cell}>
        {t.chrome.down} <span className={styles.value}>{down ? formatSpeed(down) : t.chrome.idle}</span>
      </div>

      <span className={styles.spacer} />

      <div className={styles.deps}>
        <span>
          {t.chrome.depLidarr} <span className={depTone('lidarr')}>■</span>
        </span>
        <span>
          {t.chrome.depSoulseek} <span className={depTone('soulseek')}>■</span>
        </span>
      </div>
    </div>
  );
}
```

Importera `formatShortTime` från `web/src/format.ts` tillsammans med `formatSpeed`.

**Nycklarna i `StatusReport.modules` måste verifieras innan raden skrivs.** `modules` är en `Record<string, string>` vars nycklar är pipeline-modulernas namn, inte `lidarr` och `soulseek`. Läs hur `web/src/routes/Health.tsx` itererar över dem idag — den renderar redan varje modul — och avgör därifrån vilka som representerar Lidarr respektive Soulseek. Om ingen modul gör det, visa i stället de två modulerna med sämst tillstånd, och byt `t.chrome.depLidarr`/`depSoulseek` mot modulnamnen. Att hårdkoda gissade nycklar ger en prick som alltid är grå, vilket är värre än ingen prick.

- [ ] **Step 6: Skriv om `Layout` och lägg till rutterna**

`web/src/components/Layout.module.css`:

```css
.app {
  display: flex;
  flex-direction: column;
  width: 100%;
  height: 100vh;
  background: var(--bg);
  color: var(--fg);
}

.body { display: flex; flex: 1; min-height: 0; }

.main { flex: 1; min-width: 0; overflow-y: auto; }
```

`web/src/components/Layout.tsx`:

```tsx
import { Outlet } from 'react-router-dom';
import { useJobs, useStatus } from '../api/queries';
import { t } from '../strings';
import { FlashProvider } from './chrome/FlashContext';
import SideNav from './chrome/SideNav';
import type { NavGroup } from './chrome/SideNav';
import StatusBar from './chrome/StatusBar';
import TopBar from './chrome/TopBar';
import styles from './Layout.module.css';

export default function Layout() {
  const status = useStatus();
  const jobs = useJobs();

  const s = status.data;
  const inFlight = (s?.active ?? 0) + (s?.queued ?? 0) + (s?.stalled ?? 0);
  const needsAttention = (s?.stalled ?? 0) + (s?.orphaned ?? 0);
  const unreadChat = 0; // no messages API yet; see #183

  const groups: NavGroup[] = [
    {
      label: t.nav.groupMonitor,
      items: [
        { to: '/', label: t.nav.overview, end: true },
        { to: '/jobs', label: t.nav.jobs, badge: inFlight },
        { to: '/events', label: t.nav.events },
        { to: '/peers', label: t.nav.peers },
        { to: '/health', label: t.nav.health, badge: needsAttention, alert: true },
      ],
    },
    {
      label: t.nav.groupSoulseek,
      items: [
        { to: '/search', label: t.nav.search },
        { to: '/shares', label: t.nav.shares },
        { to: '/chat', label: t.nav.chat, badge: unreadChat },
      ],
    },
    {
      label: t.nav.groupSystem,
      items: [
        { to: '/setup', label: t.nav.setup },
        { to: '/settings', label: t.nav.settings },
      ],
    },
  ];

  // jobs is read so the badge and the jobs view share one polled query
  // instead of two; TanStack dedupes by key.
  void jobs;

  return (
    <FlashProvider>
      <div className={styles.app}>
        <TopBar />
        <div className={styles.body}>
          <SideNav groups={groups} />
          <main className={styles.main}>
            <Outlet />
          </main>
        </div>
        <StatusBar />
      </div>
    </FlashProvider>
  );
}
```

I `web/src/App.tsx`, lägg till tre rutter inuti `<Route element={<Layout />}>`:

```tsx
            <Route path="search" element={<Search />} />
            <Route path="chat" element={<Chat />} />
            <Route path="setup" element={<Setup />} />
```

med importer överst. `Search`, `Chat` och `Setup` skapas i Task 6 respektive Task 13 — lägg till rutterna först när komponenterna finns, alltså i slutet av Task 6. **Gör inte** App.tsx-ändringen i den här tasken.

- [ ] **Step 7: Kör hela sviten**

Run: `cd web && npm test && npm run build`
Expected: PASS. `App.test.tsx` kan brista om den asserterar på gammal nav-text — uppdatera den till de nya gemena etiketterna, brist är signal.

- [ ] **Step 8: Commit**

```bash
git add web/src/components/chrome web/src/components/Layout.tsx web/src/components/Layout.module.css web/src/strings.ts web/src/App.test.tsx
git commit -m "feat(ui): global chrome med TopBar, SideNav, StatusBar och flash (#198)"
```

---

### Task 6: Platshållarvyer för Search och Chat

**Files:**
- Create: `web/src/routes/Search.tsx`
- Create: `web/src/routes/Chat.tsx`
- Create: `web/src/routes/Placeholder.module.css`
- Test: `web/src/routes/Placeholder.test.tsx`
- Modify: `web/src/strings.ts`
- Modify: `web/src/App.tsx`

**Interfaces:**
- Consumes: `SectionHeader` (Task 4)
- Produces: rutterna `/search` och `/chat`

- [ ] **Step 1: Lägg till strängarna**

```ts
  placeholder: {
    searchTitle: 'SEARCH',
    searchBody:
      'Manual Soulseek search is not built yet. When it lands, results will group per peer and folder, and anything downloaded can be matched and imported into Lidarr.',
    searchIssue: 'Tracked as issue #58.',
    chatTitle: 'CHAT',
    chatBody:
      'The native Soulseek client can send and receive private messages, but there is no HTTP surface for them yet, so this view has nothing to read.',
    chatIssue: 'Tracked as issue #183.',
  },
```

- [ ] **Step 2: Skriv det fallerande testet**

Create `web/src/routes/Placeholder.test.tsx`:

```tsx
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Search from './Search';
import Chat from './Chat';

describe('placeholder views', () => {
  it('search says what is missing and points at the issue', () => {
    render(<Search />);
    expect(screen.getByText(/not built yet/)).toBeInTheDocument();
    expect(screen.getByText(/#58/)).toBeInTheDocument();
  });

  it('chat says what is missing and points at the issue', () => {
    render(<Chat />);
    expect(screen.getByText(/no HTTP surface/)).toBeInTheDocument();
    expect(screen.getByText(/#183/)).toBeInTheDocument();
  });
});
```

Run: `cd web && npx vitest run src/routes/Placeholder.test.tsx` → FAIL.

- [ ] **Step 3: Skriv komponenterna**

`web/src/routes/Placeholder.module.css`:

```css
.wrap { padding: 64px 20px; text-align: center; }

.title { font-size: 11.5px; color: var(--faint); letter-spacing: 1.4px; }

.body {
  font-size: 11.5px;
  color: var(--text-dim);
  margin: 12px auto 0;
  max-width: 460px;
  line-height: 1.9;
}

.issue { font-size: 11px; color: var(--faint); margin-top: 16px; letter-spacing: 0.4px; }
```

`web/src/routes/Search.tsx`:

```tsx
import { t } from '../strings';
import styles from './Placeholder.module.css';

/**
 * Placeholder until issue #58 builds a search endpoint. The view exists now
 * so the nav has its final shape and the keyboard bindings in #199 do not
 * have to be renumbered when it fills in.
 */
export default function Search() {
  return (
    <div className={styles.wrap}>
      <div className={styles.title}>{t.placeholder.searchTitle}</div>
      <div className={styles.body}>{t.placeholder.searchBody}</div>
      <div className={styles.issue}>{t.placeholder.searchIssue}</div>
    </div>
  );
}
```

`web/src/routes/Chat.tsx`: samma form med `chatTitle`, `chatBody`, `chatIssue`.

- [ ] **Step 4: Koppla in rutterna**

I `web/src/App.tsx`, lägg till importerna och rutterna `search` och `chat` (rutten `setup` läggs till i Task 13).

- [ ] **Step 5: Kör testerna**

Run: `cd web && npx vitest run src/routes/Placeholder.test.tsx && npm test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add web/src/routes/Search.tsx web/src/routes/Chat.tsx web/src/routes/Placeholder.module.css web/src/routes/Placeholder.test.tsx web/src/App.tsx web/src/strings.ts
git commit -m "feat(ui): platshållarvyer för search och chat (#198)"
```

---

### Task 7: Overview

**Files:**
- Modify: `web/src/routes/Overview.tsx` (ersätt hela filen)
- Modify: `web/src/routes/Overview.module.css` (ersätt hela filen)
- Modify: `web/src/routes/Overview.test.tsx`
- Modify: `web/src/strings.ts`

**Interfaces:**
- Consumes: `Ticks`, `Tag`, `SectionHeader`, `EmptyState`; hooks `useStatus`, `useJobs`, `useCharts`
- Produces: inget som senare tasks konsumerar

**Layout** (mockens rad 72–147): fem statceller i ett rutnät överst, därunder `1.6fr 1fr` — TRANSFERS till vänster, THROUGHPUT + RECONCILE till höger.

- [ ] **Step 1: Bygg om vyn**

Statcellerna läser `StatusReport` (`active`, `queued`, `stalled`, `orphaned`) och antalet klara jobb ur `useJobs()`. **Sparklines ritas bara i ACTIVE-cellen**, från `charts.throughput` mappad genom en sparkline-variant; de övriga fyra cellerna har ingen historik och får ingen. Delta-siffrorna (`+2`, `+5`) utelämnas — spec, avsnitt Overview.

TRANSFERS-panelen listar högst åtta jobb med status `active` eller `stalled`, var och en som: `Tag`, albumtitel, hastighet via `formatSpeed`, procent, en `Ticks` med `count={104}` och `live` när jobbet faktiskt flyttar bytes (`status === 'active' && !queuePosition`), och en underrad med `artist · peer` till vänster och överförda bytes via `formatSize` till höger. Ett jobb i peer-kö visar `queue pos N` i stället för bytes och `tone="queued"` på sin `Ticks`.

THROUGHPUT-panelen återanvänder `CumulativeAreaChart` från `components/charts/` med `charts.throughput` mappad till `bytesPerSecond`. RECONCILE-listan visar de sju senaste ur `charts.passes` med tid, `#id`, matchningstext och varaktighet.

- [ ] **Step 2: Uppdatera testerna**

Behåll de fyra befintliga testerna i `Overview.test.tsx` och anpassa dem till den nya markupen. Lägg till ett test som bevisar att ett kö-jobb inte visas som nedladdande:

```tsx
it('shows a peer-queued job as queued rather than downloading', () => {
  // Job is active but has queuePosition 4 — no bytes are moving.
  renderOverview([
    { ...baseJob, status: 'active', state: 'DOWNLOADING', queuePosition: 4, speed: 0 },
  ]);
  expect(screen.getByText('QU')).toBeInTheDocument();
  expect(screen.queryByText('DL')).not.toBeInTheDocument();
});
```

- [ ] **Step 3: Kör testerna**

Run: `cd web && npx vitest run src/routes/Overview.test.tsx`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add web/src/routes/Overview.tsx web/src/routes/Overview.module.css web/src/routes/Overview.test.tsx web/src/strings.ts
git commit -m "feat(ui): Overview i TUI-idiom (#198)"
```

---

### Task 8: Jobs

Den största vyn. Griden ST / ALBUM / PEER / FMT / PROGRESS / SPEED / ETA / TRY med filterchips, textfilter och expandera-på-plats.

**Files:**
- Modify: `web/src/routes/Jobs.tsx`
- Modify: `web/src/routes/Jobs.module.css`
- Modify: `web/src/routes/JobExpansion.tsx`
- Modify: `web/src/routes/JobExpansion.module.css`
- Modify: `web/src/routes/Jobs.test.tsx`
- Modify: `web/src/strings.ts`
- Unchanged: `web/src/routes/jobFilter.ts` och dess test

**Interfaces:**
- Consumes: `Tag`, `tagFor`, `Ticks`, `Chip`, `Button`, `EmptyState`, `useFlash`
- Produces: inget

- [ ] **Step 1: Ta bort läckan från #190**

Radera raden `import pill from '../components/StatusPill.module.css';` ur `Jobs.tsx` och ersätt den handbyggda kö-pillen (`<span className={...pill.stalled}>{t.jobs.inPeerQueue}</span>`) med `<Tag status={job.status} state={job.state} queuePosition={job.queuePosition} />`. Det är hela poängen med `Tag`s tredje prop.

- [ ] **Step 2: Bygg om griden**

Kolumnbredder från mockens rad 1050:

```css
.grid { grid-template-columns: 28px minmax(0, 2fr) 116px 60px 148px 66px 44px 32px; }
```

Rubrikraden och varje jobbrad använder samma `grid-template-columns`. Progresscellen innehåller `<Ticks count={26} …/>` plus procenttalet. `speed` via `formatSpeed`, `eta` via `formatEta`, `tries` visar `retries` eller `·`.

Radens `tone` för `Ticks`: `queued` när `queuePosition > 0`, `ok` när status är `done`, `bad` vid `stalled`/`failed`/`orphaned`, annars `bar`. `live` bara när jobbet faktiskt rör bytes.

Memoisera raden med `memo` — 150 rader × 26 ticks vid varje 3-sekunderspoll är precis det fallet `Ticks` optimerades för, och en orörd rad ska inte återrendera.

- [ ] **Step 3: Filterchips**

Ersätt nuvarande filterkontroll med `Chip`-rader: ALL, ACTIVE, QUEUED, STALLED, FAILED, ORPHAN, DONE, var och en med räknare. Räknarna kommer ur jobblistan, inte ur `/status`, så de matchar vad filtret faktiskt visar.

Textfältet får prefixet `/` som visuell markör (ett tecken, inte en bindning) och behåller `jobFilter.ts`-logiken oförändrad.

- [ ] **Step 4: Bygg om expansionen**

`JobExpansion.tsx` visar metaträdet i vänsterkolumn (peer, source, queue pos, time in state, quality/format, transferred, job id) med `├`/`└`-glyfer, och FILES i högerkolumn.

Filerna hämtas med `useJobDetail(job.id)` — `AttemptDetail.transfers` bär `filename`, `bytesDone` och `bytesTotal`. Anropa hooken **bara när raden är expanderad** (`enabled`-flaggan i queryn), annars pollar vyn en detaljendpoint per rad.

Knappraden: `RETRY`, `CANCEL`, `DELETE` via `Button` med varianterna primary / ghost / danger. Inga tangentprefix. Varje mutation anropar `useFlash()` med en bekräftelse.

- [ ] **Step 5: Uppdatera och utöka testerna**

De 14 befintliga testerna i `Jobs.test.tsx` anpassas till ny markup. Lägg till:

```tsx
it('fetches file detail only for the expanded row', async () => {
  // Two jobs rendered, one expanded: exactly one detail request must go out.
  const detailCalls = vi.fn();
  server.use(http.get('/api/jobs/:id/detail', ({ params }) => {
    detailCalls(params.id);
    return HttpResponse.json({ id: params.id, title: 'x', artist: 'y', state: 'DOWNLOADING', attempts: [] });
  }));

  renderJobs([jobA, jobB]);
  await user.click(screen.getByText(jobA.title));

  await waitFor(() => expect(detailCalls).toHaveBeenCalledTimes(1));
  expect(detailCalls).toHaveBeenCalledWith(String(jobA.id));
});

it('flashes a confirmation after cancelling', async () => {
  renderJobsWithChrome([jobA]);
  await user.click(screen.getByText(jobA.title));
  await user.click(screen.getByRole('button', { name: /cancel/i }));
  expect(await screen.findByText(/cancelled/i)).toBeInTheDocument();
});
```

**Obs:** `renderJobsWithChrome` finns inte — skapa en hjälpare i testfilen som wrappar i `FlashProvider` och renderar `StatusBar` bredvid, så flash-assertionen har någonstans att landa. Anpassa mock-mekanismen (`server.use`/`http`) till den som redan används i `Jobs.test.tsx`; om filen mockar `fetch` direkt i stället för MSW, följ det mönstret.

- [ ] **Step 6: Kör testerna**

Run: `cd web && npx vitest run src/routes/Jobs.test.tsx src/routes/jobFilter.test.ts`
Expected: PASS. `jobFilter.test.ts` ska passera oförändrad — om den brister har filterlogiken rörts, vilket den inte ska.

- [ ] **Step 7: Commit**

```bash
git add web/src/routes/Jobs.tsx web/src/routes/Jobs.module.css web/src/routes/JobExpansion.tsx web/src/routes/JobExpansion.module.css web/src/routes/Jobs.test.tsx web/src/strings.ts
git commit -m "feat(ui): jobbvyn i TUI-idiom, kö-pillen blir en riktig Tag (#198, #190)"
```

---

### Task 9: JobDetail

**Files:**
- Modify: `web/src/routes/JobDetail.tsx`
- Modify: `web/src/routes/JobDetail.module.css`
- Modify: `web/src/routes/JobDetail.test.tsx`

- [ ] **Step 1: Bygg om vyn**

Sidhuvudet blir en `SectionHeader` med albumtitel och artist. Försökslistan (`attempts`) blir sektioner per kandidat, varje överföring en rad med `Ticks count={104}`, filnamn via `basename`, bytes via `formatSize`, och köposition/hastighet när de finns.

`JobActions.tsx` behålls funktionellt men byter till `Button`-primitiven och anropar `useFlash()`.

- [ ] **Step 2: Kör och anpassa de 12 befintliga testerna**

Run: `cd web && npx vitest run src/routes/JobDetail.test.tsx`
Expected: PASS efter anpassning till ny markup. Ingen ändring i vilka fält som visas — brister som handlar om *innehåll* snarare än markup är regressioner och ska rättas, inte anpassas bort.

- [ ] **Step 3: Commit**

```bash
git add web/src/routes/JobDetail.tsx web/src/routes/JobDetail.module.css web/src/routes/JobDetail.test.tsx web/src/components/JobActions.tsx web/src/components/JobActions.module.css
git commit -m "feat(ui): jobbdetaljvyn i TUI-idiom (#198)"
```

---

### Task 10: Health

**Files:**
- Modify: `web/src/routes/Health.tsx`
- Modify: `web/src/routes/Health.module.css`
- Modify: `web/src/routes/Health.test.tsx`
- Modify: `web/src/components/charts/*.module.css` (ny stil, oförändrad matte)
- Modify: `web/src/strings.ts`

- [ ] **Step 1: Beroendekorten**

Tre kort i ett `repeat(3, 1fr)`-rutnät ur `status.moduleDetails`: färgad fyrkant, namn, tillstånd till höger, detaljtext under. **Uppetidsstaplarna utelämnas** — de kräver 30 minuters historik per beroende som ingen lagrar (spec, avsnitt Health).

- [ ] **Step 2: Diagrammen**

`PassBarChart` (ur `charts.passes`) och `CumulativeAreaChart` (ur `charts.completedByHour`) behåller sin matte helt. Bara CSS-modulerna byter färg och tar bort radier.

- [ ] **Step 3: METRICS-sektionen**

En `SectionHeader` med etiketten METRICS och meta `prometheus`, följd av sex rader `nyckel → värde`:

| Nyckel | Källa |
|---|---|
| `slusk_downloads_active` | `status.active` |
| `slusk_downloads_queued` | `status.queued` |
| `slusk_downloads_stalled` | `status.stalled` |
| `slusk_unknown_transfers` | `status.orphaned` |
| `slusk_uploads_active` | `uploads.active` från `useUploads()` |
| `slusk_shared_files` | `shares.files` från `useShares()` |

Mockens `slusk_reconcile_total` är utelämnad: `ChartsReport` bär bara de 20 senaste passen, inte en total sedan start, och `passes.length` är alltså inte den siffran.

**Verifiera varje nyckelnamn mot `internal/observ` innan raden skrivs.** Kör `grep -rn "prometheus.NewGauge\|prometheus.NewCounter\|Name:" internal/observ/` och använd de namn som faktiskt exporteras. En rad märkt med ett metriknamn som inte finns i `/metrics` är påhittad, och en användare som kopierar den in i en Grafana-query får ingen data och ingen förklaring. Finns ingen motsvarande metrik för en rad, ta bort raden i stället för att uppfinna ett namn.

- [ ] **Step 4: Kör testerna**

Run: `cd web && npx vitest run src/routes/Health.test.tsx src/components/charts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/Health.tsx web/src/routes/Health.module.css web/src/routes/Health.test.tsx web/src/components/charts web/src/strings.ts
git commit -m "feat(ui): hälsovyn i TUI-idiom (#198)"
```

---

### Task 11: Shares och uploads

**Files:**
- Modify: `web/src/routes/Shares.tsx`
- Modify: `web/src/routes/Shares.module.css`
- Modify: `web/src/routes/UploadsPanel.tsx`
- Modify: `web/src/routes/Shares.test.tsx`
- Modify: `web/src/strings.ts`

- [ ] **Step 1: Bygg om vyn**

Rubrikrad med SHARED FOLDERS, sammanfattning (`N folders · N files · N TB` via `formatSize`), och RESCAN-knappen. Under en pågående scan: spinner och ordet `indexing` — **ingen procent och ingen tickstapel**, `SharesReport` bär bara `scanning: bool` (spec, avsnitt Shares).

Mappraster med kolumnerna PATH / FILES / SIZE / INDEXED. En mapp vars `indexedAt` är äldre än ett dygn visas i `--bad`, som i mocken.

Varningskortet när inga mappar är delade behåller sin nuvarande text exakt — den innehåller ett TOML-utdrag där båda nycklarna är obligatoriska, och kommentaren i `strings.ts` förklarar varför. Rör inte den strängen.

`UploadsPanel` blir en `SectionHeader` med UPLOADS plus rader i samma form som Overviews TRANSFERS: `Tag`-liknande UL/QU-märke, filnamn via `formatVirtualPath`, hastighet, `Ticks count={104}`.

RESCAN behåller sin 202/409-hantering oförändrad och anropar `useFlash()` vid lyckad start.

- [ ] **Step 2: Kör de 16 befintliga testerna**

Run: `cd web && npx vitest run src/routes/Shares.test.tsx`
Expected: PASS efter anpassning. Testerna som rör 409-konflikt och den inaktiverade delningen ska passera **utan** ändring i sin assertionslogik — de handlar om beteende, inte stil.

- [ ] **Step 3: Commit**

```bash
git add web/src/routes/Shares.tsx web/src/routes/Shares.module.css web/src/routes/UploadsPanel.tsx web/src/routes/Shares.test.tsx web/src/strings.ts
git commit -m "feat(ui): delningsvyn och uppladdningar i TUI-idiom (#198)"
```

---

### Task 12: Events och Peers

Vyer som inte finns i mocken men behålls (spec, beslut 4). De byggs i samma idiom.

**Files:**
- Modify: `web/src/routes/Events.tsx`
- Modify: `web/src/routes/Peers.tsx`
- Modify: `web/src/routes/Peers.module.css`
- Create: `web/src/routes/Events.module.css`
- Modify: `web/src/routes/Peers.test.tsx`
- Unchanged: `web/src/routes/eventFilter.ts` och dess test

- [ ] **Step 1: Events**

Filterfält överst i samma form som jobbvyns, sedan en tät lista: tid via `formatShortTime`, `#jobId`, händelseetikett via `eventLabel`, detaljtext. `EmptyState` när filtret inte matchar. `eventFilter.ts` rörs inte.

- [ ] **Step 2: Peers**

Rutnät med kolumnerna PEER / SCORE / OK / FAIL / LAST SEEN. Artisthistoriken blir en expanderbar rad i samma mönster som jobbexpansionen.

- [ ] **Step 3: Kör testerna**

Run: `cd web && npx vitest run src/routes/Peers.test.tsx src/routes/eventFilter.test.ts`
Expected: PASS. `eventFilter.test.ts` oförändrad.

- [ ] **Step 4: Commit**

```bash
git add web/src/routes/Events.tsx web/src/routes/Events.module.css web/src/routes/Peers.tsx web/src/routes/Peers.module.css web/src/routes/Peers.test.tsx web/src/strings.ts
git commit -m "feat(ui): händelse- och peer-vyerna i TUI-idiom (#198)"
```

---

### Task 13: Setup

Ny vy. Tre steg med fältvärden ur konfigurationen och testknappar.

**Files:**
- Create: `web/src/routes/Setup.tsx` + `.module.css`
- Test: `web/src/routes/Setup.test.tsx`
- Modify: `web/src/App.tsx` (rutten `setup`)
- Modify: `web/src/strings.ts`

**Interfaces:**
- Consumes: `useConfig`, `useTestConnection`, `useShares`, `Button`, `SectionHeader`

- [ ] **Step 1: Lägg till strängarna**

Mockens inledning är falsk hos oss och skrivs om:

```ts
  setup: {
    title: 'GUIDED SETUP',
    // The mock says slusk never writes the config file. That stopped being
    // true with issue #134 — the Config view writes it. This copy points there
    // instead of describing a workflow we no longer have.
    intro:
      'Check that each dependency answers before letting the pipeline run. Anything that fails can be corrected in the Config view, or in the configuration file directly.',
    stepSoulseek: 'Soulseek login',
    stepLidarr: 'Lidarr connection',
    stepShares: 'Shared folders',
    test: 'TEST',
    testing: 'TESTING',
    stateOk: 'OK',
    stateFailed: 'FAILED',
    stateUntested: 'UNTESTED',
    stateDisabled: 'NOT ENABLED',
    fieldUrl: 'url',
    fieldApiKey: 'api key',
    fieldUsername: 'username',
    fieldPassword: 'password',
    fieldFolders: 'folders',
    fieldIndex: 'index',
    secretSet: 'configured',
    secretUnset: 'not set',
    foldersCount: (n: number) => `${n} configured`,
    indexCount: (n: number) => `${n} files`,
    sharesNoTest:
      'There is no connection test for shares. The state is derived from whether the index has found any files.',
  },
```

- [ ] **Step 2: Skriv det fallerande testet**

```tsx
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Setup from './Setup';
import { renderWithQuery } from '../test-utils'; // create if missing, mirroring Shares.test.tsx

describe('Setup', () => {
  it('never renders a secret, only whether it is configured', async () => {
    renderWithQuery(<Setup />, {
      config: { lidarr: { url: 'http://lidarr:8686', apiKeyConfigured: true }, /* … */ },
    });
    expect(await screen.findByText('configured')).toBeInTheDocument();
    expect(screen.queryByText(/ab12/)).not.toBeInTheDocument();
  });

  it('derives the shares step from the index rather than a test call', async () => {
    renderWithQuery(<Setup />, { shares: { enabled: true, files: 0, folders: [] } });
    expect(await screen.findByText('UNTESTED')).toBeInTheDocument();
  });

  it('reports soulseek as not enabled when the backend is off', async () => {
    renderWithQuery(<Setup />, { config: { soulseek: { enabled: false } } });
    expect(await screen.findByText('NOT ENABLED')).toBeInTheDocument();
  });
});
```

**Obs:** `renderWithQuery` och den exakta mock-formen finns inte färdiga — spegla hur `Shares.test.tsx` sätter upp sin `QueryClientProvider` och sina svarsmockar, och fyll `config`-objektet med de fält `AppConfig` faktiskt kräver.

Run → FAIL.

- [ ] **Step 3: Skriv vyn**

Tre steg. Soulseek och Lidarr har `useTestConnection('soulseek' | 'lidarr')`; deras tillstånd är `idle` → UNTESTED, `pending` → TESTING, `success` med `ok: true` → OK, annars FAILED med felmeddelandet i ett kort med `--bad`-ram.

Soulseek-steget visar NOT ENABLED och döljer testknappen när `config.soulseek.enabled` är falskt.

Shares-steget har **ingen** testknapp. Dess tillstånd härleds: OK när `shares.files > 0`, annars UNTESTED, med `sharesNoTest` som förklaring.

Hemligheter renderas aldrig — bara `configured` / `not set` ur `*Configured`-flaggorna.

- [ ] **Step 4: Koppla in rutten och kör**

Lägg till `<Route path="setup" element={<Setup />} />` i `App.tsx`.

Run: `cd web && npx vitest run src/routes/Setup.test.tsx && npm test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/Setup.tsx web/src/routes/Setup.module.css web/src/routes/Setup.test.tsx web/src/App.tsx web/src/strings.ts
git commit -m "feat(ui): guidad setup-vy (#198)"
```

---

### Task 14: Config

Enbart stil. `Settings.tsx` är 1248 rader och dess 25 tester ska passera med minimala ändringar.

**Files:**
- Modify: `web/src/routes/Settings.module.css` (huvudarbetet)
- Modify: `web/src/routes/Settings.tsx` (endast där markup måste ändras för stilen)
- Modify: `web/src/routes/Settings.test.tsx` (endast där markup ändrats)

- [ ] **Step 1: Bygg om CSS-modulen**

Sektionskort blir `SectionHeader`-lister ovanför en oramad grupp. Fältrader blir rutnät `190px 1fr` med etikett i `--faint`. Inputs får `background: var(--panel-inset)`, `1px solid var(--line2)`, ingen radie. Selects likaså. Danger zone får `border-left: 2px solid var(--bad)` och behåller sin tvåkliksbekräftelse **oförändrad**.

- [ ] **Step 2: Rör ingen logik**

Ingen ändring i validering, `useUpdateConfig`, fältfelshantering, omstartspollning eller hanteringen av write-only-hemligheter. Om ett test brister av annan anledning än en klassnamnsändring är det en regression.

- [ ] **Step 3: Kör testerna**

Run: `cd web && npx vitest run src/routes/Settings.test.tsx`
Expected: PASS, 25 tester.

- [ ] **Step 4: Commit**

```bash
git add web/src/routes/Settings.tsx web/src/routes/Settings.module.css web/src/routes/Settings.test.tsx
git commit -m "feat(ui): konfigurationsvyn i TUI-idiom, oförändrat beteende (#198)"
```

---

### Task 15: Städning och verifiering

**Files:**
- Delete: `web/src/components/ProgressBar.tsx`, `ProgressBar.module.css`
- Delete: `web/src/components/StatusPill.tsx`, `StatusPill.module.css`, `StatusPill.test.tsx`
- Delete: `web/src/components/StatCard.tsx`, `StatCard.module.css`
- Delete: `web/src/components/SourceBadge.tsx`, `SourceBadge.module.css`
- Delete: `web/src/components/PageHeading.tsx`
- Modify: `web/src/strings.ts` (ta bort strängar utan användare)

- [ ] **Step 1: Bevisa att komponenterna är oanvända**

Run: `cd web && grep -rn "ProgressBar\|StatusPill\|StatCard\|SourceBadge\|PageHeading" src/`
Expected: inga träffar utanför filerna som ska raderas. Finns det träffar är en vy inte färdigkonverterad — gå tillbaka till den tasken i stället för att radera.

- [ ] **Step 2: Radera**

```bash
git rm web/src/components/ProgressBar.tsx web/src/components/ProgressBar.module.css \
       web/src/components/StatusPill.tsx web/src/components/StatusPill.module.css web/src/components/StatusPill.test.tsx \
       web/src/components/StatCard.tsx web/src/components/StatCard.module.css \
       web/src/components/SourceBadge.tsx web/src/components/SourceBadge.module.css \
       web/src/components/PageHeading.tsx
```

- [ ] **Step 3: Städa strängkatalogen**

Kör `strings.test.ts` — den kontrollerar katalogens form. Ta bort nycklar som inte längre har någon användare, men **bara** efter att ha bekräftat det med grep per nyckel. En oanvänd sträng är billig; en raderad sträng som någon läser är ett krascha-vid-rendering.

- [ ] **Step 4: Full svit**

Run:
```bash
cd web && npm test && npm run build
cd .. && go test ./... && go vet ./... && gofmt -l .
```
Expected: allt grönt. `gofmt -l .` ska inte lista något. Notera att `internal/store` `TestOpenRecyclesIdleConnections` är känd flakig under last (#171) — den är inte en regression.

- [ ] **Step 5: Verifiera i labbet**

```bash
./testenv/lab.sh reset
./testenv/lab.sh info
```

Gå igenom alla tio vyer i webbläsaren på `:9090`. Kontrollera särskilt:
- Jobbvyn med 150 seedade album — mät att raderna inte staplar upp renderingar. Öppna React DevTools Profiler och bekräfta att en poll utan förändring inte återrenderar orörda rader. **Detta är hypotesen från specen och den ska verifieras, inte antas.**
- Att inga tangentglyfer syns någonstans.
- Att Config sparar och startar om som förut.

- [ ] **Step 6: Commit**

```bash
git add -u web/src
git commit -m "refactor(ui): ta bort komponenterna TUI-primitiverna ersatte (#198, #190)"
```

**Obs:** använd aldrig `git add -A` i det här repot — agentverktyg lämnar otrackade kataloger som svepts med i en commit förut.

---

## Efter planen

- PR mot `main` med `tea pulls create --head feat/ui-tui-reskin-198 --base main`. Passera `--head` explicit; `tea` använder annars den utcheckade grenen.
- Skriv `Closes #198` i klartext, inte inuti bakåtfästingar — Gitea parsar bara nyckelordet som ren text.
- Nämn i PR-texten att #190 löses av det här arbetet och att #181 blir ersatt.
- **Merge är deploy.** Ingen ny confignyckel tillkommer, så prod-`config.toml` behöver inget förarbete.
