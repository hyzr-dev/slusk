import { test } from 'node:test'
import assert from 'node:assert/strict'
import { filesConflict, contractsTouched, conflicts, rank, isAssessable, computeWaves } from './waves.mjs'

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
  assert.ok(result.waves.length === 2)
  assert.ok(result.waves[0].length === 1)
  assert.ok(result.waves[1].length === 1)
  assert.deepEqual(result.unassessable, [])
})
