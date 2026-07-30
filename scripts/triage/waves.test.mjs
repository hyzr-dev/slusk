import { test } from 'node:test'
import assert from 'node:assert/strict'
import { filesConflict } from './waves.mjs'

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
