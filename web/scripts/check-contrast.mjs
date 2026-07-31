// Fails the build when the palette in tokens.css stops meeting its contrast
// budget.
//
// Like check-css-tokens.mjs, this exists because the test suite structurally
// cannot catch what it checks: jsdom computes no style and paints nothing, so
// no render test can observe a colour at all, let alone measure it.
//
// Two rules, and the second is the reason this file is not just an AA check.
//
//   1. Floor      — every text colour clears WCAG AA (4.5:1) against EVERY
//                   surface it can land on. Checking only --bg is how --bad and
//                   --text-dim shipped at 4.48 and 4.47 on --btn (#335): they
//                   passed on the page background and failed inside a button.
//
//   2. Separation — adjacent steps in the text ladder differ by at least
//                   MIN_STEP_LSTAR. A pure AA check cannot see a collapsed
//                   ladder. The palette this replaced passed AA on every single
//                   token while rendering as two perceptual steps instead of
//                   four, because --dim, --faint and --text-dim sat within
//                   1.6 L* of one another. "Legible" and "distinguishable" are
//                   different questions and need different tests.
//
// Separation is measured in CIE L*, not in contrast ratio. A ratio answers "can
// I read this on that"; it says nothing about whether two greys look different
// from each other, which is the only thing a ladder is for.
//
// Every colour token must appear in exactly one bucket below. An unclassified
// token is an error rather than a pass — otherwise the next token added to the
// palette silently escapes the budget, which is precisely how this drifted the
// first time.

import { readFileSync } from 'node:fs';

const MIN_STEP_LSTAR = 8; // adjacent ladder steps must differ by at least this
const AA_TEXT = 4.5; // WCAG 1.4.3, normal-size text
const NON_TEXT = 3; // WCAG 1.4.11, meaningful non-text

// Surfaces text can be rendered on. --btn is the lightest and therefore the
// strictest; it is what pins the bottom of the ladder.
const SURFACES = ['--bg', '--panel', '--panel-inset', '--panel-hover', '--nav-active', '--btn'];

// Colours used for text. Each is checked against every surface.
const TEXT = ['--fg', '--dim', '--text-dim', '--ok', '--bad'];

// The neutral text ladder, brightest first. Only these are checked for
// separation: --ok and --bad carry hue and are distinguished by it, not by
// lightness, so they are text but not rungs.
const LADDER = ['--fg', '--dim', '--text-dim'];

// Non-text pairs that must remain mutually distinguishable. The tick row is how
// a user reads how far a job has got, so its three states qualify under 1.4.11.
const NON_TEXT_PAIRS = [
  ['--tick-queued', '--tick-off'],
  ['--bar', '--tick-queued'],
  ['--bar', '--tick-off'],
];

// Purely decorative: hairlines and scrollbar chrome. WCAG asks nothing of these
// because losing them loses no information.
const DECORATIVE = ['--line', '--line2', '--line-inner', '--scroll-thumb', '--scroll-thumb-hover'];

// Not colours at all.
const NOT_A_COLOUR = ['--font-mono'];

const tokensPath = new URL('../src/styles/tokens.css', import.meta.url).pathname;
const source = readFileSync(tokensPath, 'utf8');

const palette = new Map();
for (const [, name, hex] of source.matchAll(/^\s*(--[a-z0-9-]+)\s*:\s*(#[0-9a-fA-F]{6})\s*;/gm)) {
  palette.set(name, hex.toLowerCase());
}
const declared = [...source.matchAll(/^\s*(--[a-z0-9-]+)\s*:/gm)].map((m) => m[1]);

if (palette.size === 0) {
  console.error('check-contrast: parsed no colours from tokens.css — the check would be vacuous');
  process.exit(1);
}

const channel = (v) => {
  const c = v / 255;
  return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
};

/** Relative luminance per WCAG 2.x. */
function luminance(hex) {
  const n = parseInt(hex.slice(1), 16);
  return 0.2126 * channel((n >> 16) & 255) + 0.7152 * channel((n >> 8) & 255) + 0.0722 * channel(n & 255);
}

/** WCAG contrast ratio between two hex colours, order-independent. */
function ratio(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** CIE L*. Y is already D65-normalised, so this is the plain L* transfer. */
function lstar(hex) {
  const y = luminance(hex);
  return y > 216 / 24389 ? 116 * Math.cbrt(y) - 16 : (y * 24389) / 27;
}

const failures = [];
const colourOf = (token) => {
  const hex = palette.get(token);
  if (!hex) failures.push(`${token} is referenced by the contrast budget but not defined in tokens.css`);
  return hex;
};

// Every colour token must be classified. A new token that nobody placed in a
// bucket is a gap in the budget, not a pass.
const classified = new Set([...SURFACES, ...TEXT, ...DECORATIVE, ...NOT_A_COLOUR, ...NON_TEXT_PAIRS.flat()]);
for (const token of declared) {
  if (!classified.has(token)) {
    failures.push(
      `${token} is not classified in check-contrast.mjs — add it to SURFACES, TEXT, NON_TEXT_PAIRS, DECORATIVE or NOT_A_COLOUR`,
    );
  }
}

// Rule 1 — floor.
for (const token of TEXT) {
  const hex = colourOf(token);
  if (!hex) continue;
  for (const surface of SURFACES) {
    const surfaceHex = colourOf(surface);
    if (!surfaceHex) continue;
    const r = ratio(hex, surfaceHex);
    if (r < AA_TEXT) {
      failures.push(`${token} (${hex}) on ${surface} (${surfaceHex}) is ${r.toFixed(2)}:1, below AA ${AA_TEXT}:1`);
    }
  }
}

// Rule 2 — separation.
for (let i = 0; i < LADDER.length - 1; i += 1) {
  const [upper, lower] = [LADDER[i], LADDER[i + 1]];
  const [a, b] = [colourOf(upper), colourOf(lower)];
  if (!a || !b) continue;
  const gap = lstar(a) - lstar(b);
  if (gap < MIN_STEP_LSTAR) {
    failures.push(
      `${upper} (L* ${lstar(a).toFixed(1)}) and ${lower} (L* ${lstar(b).toFixed(1)}) differ by ${gap.toFixed(1)} L*, ` +
        `below the ${MIN_STEP_LSTAR} L* needed to read as separate steps`,
    );
  }
}

// Rule 3 — meaningful non-text.
for (const [a, b] of NON_TEXT_PAIRS) {
  const [x, y] = [colourOf(a), colourOf(b)];
  if (!x || !y) continue;
  const r = ratio(x, y);
  if (r < NON_TEXT) {
    failures.push(`${a} (${x}) against ${b} (${y}) is ${r.toFixed(2)}:1, below the ${NON_TEXT}:1 WCAG 1.4.11 asks`);
  }
}

if (failures.length > 0) {
  console.error(`check-contrast: ${failures.length} violation(s) of the palette's contrast budget\n`);
  console.error(failures.map((f) => `  ${f}`).join('\n'));
  console.error('\nThe budget and its reasoning are documented at the top of src/styles/tokens.css.');
  process.exit(1);
}

const worst = (token) => Math.min(...SURFACES.map((s) => ratio(palette.get(token), palette.get(s))));
const summary = LADDER.map((t) => `${t} L* ${lstar(palette.get(t)).toFixed(1)}`).join(' > ');
console.log(
  `check-contrast: ${palette.size} colours, ladder ${summary}, ` +
    `worst text contrast ${Math.min(...TEXT.map(worst)).toFixed(2)}:1`,
);
