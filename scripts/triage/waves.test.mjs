import { test } from 'node:test'
import assert from 'node:assert/strict'
import { filesConflict, contractsTouched, conflicts, rank, isAssessable, computeWaves, namesDirectory, hasDirectoryTouch } from './waves.mjs'

test('issues touching the same file conflict', () => {
  const a = { number: 1, touches: ['internal/pipeline/importing.go'] }
  const b = { number: 2, touches: ['internal/pipeline/importing.go', 'x.go'] }
  assert.equal(filesConflict(a, b), true)
})

test('issues touching different files in the same directory do not conflict', () => {
  const a = { number: 1, touches: ['internal/pipeline/importing.go'] }
  const b = { number: 2, touches: ['internal/pipeline/discovery.go'] }
  assert.equal(filesConflict(a, b), false)
})

test('an issue with no known touches conflicts with everything', () => {
  const a = { number: 1, touches: [] }
  const b = { number: 2, touches: ['web/src/App.tsx'] }
  assert.equal(filesConflict(a, b), true)
  assert.equal(filesConflict(b, a), true)
})

test('a touches entry naming a directory is caught, with and without a trailing slash', () => {
  assert.equal(namesDirectory('web/src/api'), true)
  assert.equal(namesDirectory('web/src/api/'), true)
  assert.equal(namesDirectory('internal/pipeline/'), true)
  assert.equal(namesDirectory('internal/store/migrations'), true)
})

test('a normal file path is not caught', () => {
  assert.equal(namesDirectory('web/src/api/stream.tsx'), false)
  assert.equal(namesDirectory('internal/pipeline/importing.go'), false)
  assert.equal(namesDirectory('config.example.toml'), false)
})

test('hasDirectoryTouch flags an issue with one directory among real files', () => {
  // The shape the first real run actually produced: issue 58 reported
  // "web/src/api" beside two genuine files.
  const issue = { number: 58, touches: ['web/src/api', 'web/src/views/Jobs.tsx'] }
  assert.equal(hasDirectoryTouch(issue), true)
  assert.equal(hasDirectoryTouch({ number: 59, touches: ['web/src/api/stream.tsx'] }), false)
  assert.equal(hasDirectoryTouch({ number: 60 }), false)
})

test('an issue whose touches name a directory is reported, not scheduled', () => {
  const issues = [
    { number: 58, prodImpact: 'degraded', effort: 'M', touches: ['web/src/api'] },
    { number: 59, prodImpact: 'outage', effort: 'S', touches: ['web/src/api/stream.tsx'] },
  ]
  const result = computeWaves(issues, [])
  // Without the exclusion #58's directory entry never collides with #59's file,
  // so both would share wave one and two agents would land in web/src/api.
  assert.deepEqual(result.waves, [[59]])
  assert.deepEqual(result.unschedulable, [58])
  assert.deepEqual(result.unassessable, [])
})

test('a trailing-slash directory touch is excluded the same way', () => {
  const issues = [
    { number: 70, prodImpact: 'degraded', effort: 'M', touches: ['internal/pipeline/'] },
    { number: 71, prodImpact: 'cosmetic', effort: 'S', touches: ['internal/pipeline/importing.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[71]])
  assert.deepEqual(result.unschedulable, [70])
})

test('an issue failing both checks is reported as unassessable only', () => {
  const issues = [
    { number: 80, prodImpact: 'sever', effort: 'S', touches: ['web/src/api'] },
    { number: 81, prodImpact: 'outage', effort: 'S', touches: ['a.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[81]])
  assert.deepEqual(result.unassessable, [80])
  assert.deepEqual(result.unschedulable, [])
})

const CONTRACTS = [
  { name: 'sse-events', sides: [['internal/observ/stream.go'], ['web/src/api/stream.tsx']] },
  { name: 'config', sides: [['internal/config/'], ['config.example.toml']] },
]

test('an issue touching one side of a contract touches the contract', () => {
  const issue = { number: 275, touches: ['internal/observ/stream.go'] }
  assert.deepEqual(contractsTouched(issue, CONTRACTS), ['sse-events'])
})

test('a path prefix matches a directory side', () => {
  const issue = { number: 89, touches: ['internal/config/config.go'] }
  assert.deepEqual(contractsTouched(issue, CONTRACTS), ['config'])
})

test('opposite sides of one contract conflict despite disjoint files', () => {
  const producer = { number: 275, touches: ['internal/observ/stream.go'] }
  const consumer = { number: 267, touches: ['web/src/api/stream.tsx'] }
  assert.equal(filesConflict(producer, consumer), false)
  assert.equal(conflicts(producer, consumer, CONTRACTS), true)
})

test('issues sharing no file and no contract do not conflict', () => {
  const a = { number: 1, touches: ['internal/lidarr/client.go'] }
  const b = { number: 2, touches: ['internal/matcher/rank.go'] }
  assert.equal(conflicts(a, b, CONTRACTS), false)
})

test('a directory side without a trailing slash does not match a longer sibling directory', () => {
  const contracts = [{ name: 'config', sides: [['internal/config'], ['config.example.toml']] }]
  const unrelated = { number: 1, touches: ['internal/configuration/x.go'] }
  const real = { number: 2, touches: ['internal/config/config.go'] }
  assert.deepEqual(contractsTouched(unrelated, contracts), [])
  assert.deepEqual(contractsTouched(real, contracts), ['config'])
})

test('rank orders by prod impact, then by ascending effort', () => {
  const dataloss = { number: 1, prodImpact: 'dataloss', effort: 'L', touches: ['a'] }
  const degraded = { number: 2, prodImpact: 'degraded', effort: 'S', touches: ['b'] }
  assert.ok(rank(dataloss) > rank(degraded))

  const cheap = { number: 3, prodImpact: 'degraded', effort: 'S', touches: ['c'] }
  const dear = { number: 4, prodImpact: 'degraded', effort: 'L', touches: ['d'] }
  assert.ok(rank(cheap) > rank(dear))
})

test('disjoint issues share wave one, ordered by rank', () => {
  const issues = [
    { number: 10, prodImpact: 'cosmetic', effort: 'S', touches: ['a.go'] },
    { number: 11, prodImpact: 'outage', effort: 'M', touches: ['b.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[11, 10]])
  assert.deepEqual(result.unassessable, [])
})

test('conflicting issues are split across waves, the urgent one first', () => {
  const issues = [
    { number: 20, prodImpact: 'cosmetic', effort: 'S', touches: ['same.go'] },
    { number: 21, prodImpact: 'outage', effort: 'S', touches: ['same.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[21], [20]])
  assert.deepEqual(result.unassessable, [])
})

test('statedBlockers force ordering even without a file conflict', () => {
  const issues = [
    { number: 30, prodImpact: 'outage', effort: 'S', touches: ['a.go'], statedBlockers: [31] },
    { number: 31, prodImpact: 'cosmetic', effort: 'S', touches: ['b.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[31], [30]])
  assert.deepEqual(result.unassessable, [])
})

test('computeWaves is deterministic for the same input', () => {
  const issues = [
    { number: 40, prodImpact: 'degraded', effort: 'M', touches: ['a.go'] },
    { number: 41, prodImpact: 'degraded', effort: 'M', touches: ['b.go'] },
    { number: 42, prodImpact: 'degraded', effort: 'M', touches: ['a.go'] },
  ]
  const result1 = computeWaves(issues, [])
  const result2 = computeWaves([...issues].reverse(), [])
  assert.deepEqual(result1.waves, result2.waves)
  assert.deepEqual(result1.unassessable, result2.unassessable)
})

test('an issue with typo\'d prodImpact lands in unassessable and appears in no wave', () => {
  const issues = [
    { number: 50, prodImpact: 'sever', effort: 'S', touches: ['a.go'] },
    { number: 51, prodImpact: 'outage', effort: 'S', touches: ['b.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[51]])
  assert.deepEqual(result.unassessable, [50])
})

test('an issue missing effort entirely lands in unassessable', () => {
  const issues = [
    { number: 60, prodImpact: 'outage', touches: ['a.go'] },
    { number: 61, prodImpact: 'cosmetic', effort: 'S', touches: ['b.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[61]])
  assert.deepEqual(result.unassessable, [60])
})

test('a backlog where every issue is fine returns empty unassessable', () => {
  const issues = [
    { number: 70, prodImpact: 'degraded', effort: 'M', touches: ['a.go'] },
    { number: 71, prodImpact: 'cosmetic', effort: 'S', touches: ['b.go'] },
  ]
  const result = computeWaves(issues, [])
  assert.ok(result.waves.length > 0)
  assert.deepEqual(result.unassessable, [])
})

test('an issue whose blocker is not in the backlog is still scheduled', () => {
  const issues = [
    { number: 80, prodImpact: 'outage', effort: 'S', touches: ['a.go'], statedBlockers: [999] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[80]])
  assert.deepEqual(result.unassessable, [])
})

test('circular blockers resolve arbitrarily', () => {
  const issues = [
    { number: 90, prodImpact: 'outage', effort: 'S', touches: ['a.go'], statedBlockers: [91] },
    { number: 91, prodImpact: 'outage', effort: 'S', touches: ['b.go'], statedBlockers: [90] },
  ]
  const result = computeWaves(issues, [])
  assert.deepEqual(result.waves, [[90], [91]])
  assert.deepEqual(result.unassessable, [])
})

import { invalidate } from './waves.mjs'

const state = {
  computedAt: 'f5e1f5b',
  issues: {
    '10': { number: 10, digest: 'abc123def456', touches: ['internal/store/jobview.go'] },
    '11': { number: 11, digest: 'xyz789uvw012', touches: ['web/src/App.tsx'] },
  },
}

test('an unchanged issue over unchanged code is fresh', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 10, digest: 'abc123def456' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [10], stale: [] })
})

test('an issue with a different digest is stale', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 10, digest: 'changed1234567' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [], stale: [10] })
})

test('an issue whose touched code moved is stale even if the digest did not', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 10, digest: 'abc123def456' }],
    changedPaths: ['internal/store/jobview.go'],
  })
  assert.deepEqual(out, { fresh: [], stale: [10] })
})

test('an issue absent from the cache is stale', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 99, digest: 'newdigest1234' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [], stale: [99] })
})

test('a missing state file makes every open issue stale', () => {
  const out = invalidate({
    state: null,
    openIssues: [{ number: 10, digest: 'abc123def456' }, { number: 11, digest: 'xyz789uvw012' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [], stale: [10, 11] })
})

test('the two invalidation axes are independent: same digest, code changed', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 11, digest: 'xyz789uvw012' }],
    changedPaths: ['web/src/App.tsx'],
  })
  assert.deepEqual(out, { fresh: [], stale: [11] })
})

import { execFileSync } from 'node:child_process'
import { decodeChunks } from './waves.mjs'

test('the CLI computes waves from stdin JSON', () => {
  const input = JSON.stringify({
    issues: [
      { number: 1, prodImpact: 'outage', effort: 'S', touches: ['a.go'] },
      { number: 2, prodImpact: 'cosmetic', effort: 'S', touches: ['a.go'] },
    ],
    contracts: [],
  })
  const out = execFileSync('node', ['scripts/triage/waves.mjs', 'waves'], { input })
  assert.deepEqual(JSON.parse(out), { waves: [[1], [2]], unassessable: [], unschedulable: [] })
})

test('the CLI reports unassessable issues alongside the waves', () => {
  const input = JSON.stringify({
    issues: [
      { number: 1, prodImpact: 'outage', effort: 'S', touches: ['a.go'] },
      { number: 2, prodImpact: 'urgent', effort: 'S', touches: ['b.go'] },
    ],
    contracts: [],
  })
  const out = execFileSync('node', ['scripts/triage/waves.mjs', 'waves'], { input })
  assert.deepEqual(JSON.parse(out), { waves: [[1]], unassessable: [2], unschedulable: [] })
})

test('the CLI rejects an unknown mode with a non-zero exit', () => {
  assert.throws(() =>
    execFileSync('node', ['scripts/triage/waves.mjs', 'nonsense'], { input: '{}', stdio: 'pipe' }))
})

test('the CLI round-trips a large multi-byte payload through stdin', () => {
  // A long dense run of two-byte characters guarantees that stdin arrives in
  // more than one chunk, exercising the same multi-chunk path a large
  // real-world (Swedish) backlog would take. This alone cannot prove chunks
  // are reassembled correctly -- see decodeChunks below for that -- but it
  // does confirm the CLI survives a payload this size without truncating or
  // throwing.
  const path = 'å'.repeat(200_000) + '.go'
  const input = JSON.stringify({
    issues: [{ number: 1, prodImpact: 'outage', effort: 'S', touches: [path] }],
    contracts: [],
  })
  const out = execFileSync('node', ['scripts/triage/waves.mjs', 'waves'], {
    input, maxBuffer: 10 * 1024 * 1024,
  })
  assert.deepEqual(JSON.parse(out), { waves: [[1]], unassessable: [], unschedulable: [] })
})

test('decodeChunks reassembles a multi-byte character split across a chunk boundary', () => {
  // Hand-split the buffer inside the two-byte UTF-8 encoding of "å" (0xC3
  // 0xA5), rather than relying on the OS to chunk stdin at a particular size.
  // Real stdin chunking is deterministic for a given payload and platform,
  // but where exactly a boundary lands relative to a multi-byte character
  // depends on incidental byte offsets earlier in the payload -- picking the
  // split by hand is what makes this test fail on the bug every time instead
  // of only when the surrounding JSON happens to have an unlucky length.
  const utf8 = Buffer.from('touches å', 'utf8')
  const splitAt = utf8.indexOf(0xc3) + 1 // midway through å's two bytes
  const chunks = [utf8.subarray(0, splitAt), utf8.subarray(splitAt)]
  assert.equal(decodeChunks(chunks), 'touches å')
})
