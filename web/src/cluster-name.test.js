import { test } from 'node:test'
import assert from 'node:assert/strict'
import { validateClusterName } from './cluster-name.js'

test('cluster names reserve room for generated Services', () => {
  for (const name of ['a', 'pulsar-01', 'a'.repeat(47)]) {
    assert.doesNotThrow(() => validateClusterName(name))
  }
  for (const name of ['', 'a'.repeat(48), 'a'.repeat(60), '1cluster', 'a.b', 'a-', '-a', 'A']) {
    assert.throws(() => validateClusterName(name), /1–47/)
  }
})

test('standalone BookKeeper reserves its metadata Job suffix', () => {
  assert.doesNotThrow(() => validateClusterName('a'.repeat(51), 'BookKeeperCluster'))
  assert.throws(() => validateClusterName('a'.repeat(52), 'BookKeeperCluster'), /1–51/)
})
