// Fails the build when a stylesheet references a CSS custom property that
// tokens.css does not define.
//
// This is the one class of CSS bug the test suite structurally cannot catch.
// Vite's ambient type for *.module.css is an index signature, so tsc never
// looks inside a stylesheet, and jsdom computes no style, so no render test
// can observe a colour that failed to apply. A var(--gone) with no fallback is
// invalid at computed-value time, and CSS then fails silently and *differently
// per property*: inherited ones (color) inherit from the parent, non-inherited
// ones (background, border) fall back to initial. One mistake, two unrelated
// symptoms — which is part of why six of them survived the #198 reskin (#211).
//
// It lives here rather than in vitest for two reasons: vitest stubs CSS
// imports out entirely (test.css defaults to false, so `?raw` yields an empty
// string), and reading from disk inside src/ would mean adding @types/node to
// a tsconfig whose `types` array is otherwise browser-only.

import { readFileSync, readdirSync } from 'node:fs';
import { join, relative } from 'node:path';

const srcDir = new URL('../src/', import.meta.url).pathname;

function sourceFiles(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(path);
    return /\.(css|tsx)$/.test(entry.name) ? [path] : [];
  });
}

function matchAll(text, re) {
  return [...text.matchAll(re)].map((m) => m[1]);
}

const tokens = readFileSync(join(srcDir, 'styles/tokens.css'), 'utf8');
const defined = new Set(matchAll(tokens, /^\s*(--[a-z0-9-]+)\s*:/gm));

if (defined.size === 0) {
  console.error('check-css-tokens: parsed no tokens from styles/tokens.css — the check would be vacuous');
  process.exit(1);
}

const files = sourceFiles(srcDir);
const problems = files.flatMap((file) =>
  // var(--x) and var(--x, fallback) alike: a fallback masks the mistake at
  // runtime, but the reference is still to a token that no longer exists.
  matchAll(readFileSync(file, 'utf8'), /var\(\s*(--[a-z0-9-]+)/g)
    .filter((token) => !defined.has(token))
    .map((token) => `  ${relative(srcDir, file)}: ${token}`),
);

if (problems.length > 0) {
  console.error(`check-css-tokens: ${problems.length} reference(s) to undefined custom properties\n`);
  console.error([...new Set(problems)].sort().join('\n'));
  console.error('\nDefine them in src/styles/tokens.css or point them at an existing token.');
  process.exit(1);
}

console.log(`check-css-tokens: ${defined.size} tokens, ${files.length} files, no undefined references`);
