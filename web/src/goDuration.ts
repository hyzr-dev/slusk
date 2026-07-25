// Parses the subset of Go's time.Duration.String() output the backend sends
// for config values such as pipeline.wantedSyncInterval (see api/types.ts
// PipelineConfigDTO) — strings like "15m0s", "1h30m0s", "45s". Returns whole
// or fractional seconds, or null when the input is empty or not a valid Go
// duration string, so callers can degrade gracefully instead of rendering NaN.
const UNIT_SECONDS: Record<string, number> = {
  h: 3600,
  m: 60,
  s: 1,
  ms: 0.001,
  us: 0.000001,
  // Go also accepts the micro sign (µ, U+00B5) in addition to "us".
  µs: 0.000001,
  ns: 0.000000001,
};

// Longer unit suffixes ("ms", "us", "µs", "ns") must be tried before the
// single-letter ones ("m", "s") they contain, or "500ms" would match "m" and
// leave a dangling "s" that fails the whole-string check below.
//
// A repeated unit (e.g. "5s5s") is accepted and summed to 10s rather than
// rejected — Go's time.Duration.String() never emits this, so it's not a
// real input we need to guard against, just a side effect of matching
// unit-value pairs globally instead of validating overall duration shape.
const DURATION_RE = /(\d+(?:\.\d+)?)(ms|us|µs|ns|h|m|s)/g;

export function parseGoDuration(input: string | null | undefined): number | null {
  if (!input) return null;

  let matchedAny = false;
  let consumed = 0;
  let totalSeconds = 0;

  DURATION_RE.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = DURATION_RE.exec(input)) !== null) {
    matchedAny = true;
    consumed += match[0].length;
    totalSeconds += parseFloat(match[1]) * UNIT_SECONDS[match[2]];
  }

  if (!matchedAny || consumed !== input.length) return null;
  return totalSeconds;
}
