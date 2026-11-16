import test from 'node:test'
import assert from 'node:assert/strict'
import { deployment } from './deployment.js'
import { diskEligibility, observedDisk, planEligibility, poolPlanBody, volumeReadOnlyReason } from './storage-model.js'

const now = Date.parse('2026-11-16T08:00:00Z')
const disk = { id: 'disk-one', uid: 'disk-uid', node: 'node-1', nodeUID: 'node-uid', path: '/dev/disk/by-id/wwn-1', serial: 'SER1', sizeBytes: 100 * 1024 ** 3, state: 'Available', fresh: true, canCreatePool: true, validUntil: new Date(now + 1000).toISOString() }

test('storage credentials use the declared serving process, never the hash or a remote API', () => {
  assert.deepEqual(deployment(), { storageOnly: false, storageAPI: '/storage/api', defaultModule: 'pulsar' })
  assert.deepEqual(deployment({ pathname: '/storage/' }), { storageOnly: false, storageAPI: '/storage/api', defaultModule: 'storage' })
  assert.equal(deployment({ service: 'storage', storageAPI: 'api' }).storageAPI, '/api')
  assert.equal(deployment({ service: 'storage', storageAPI: 'api', pathname: '/storage/' }).storageAPI, '/storage/api')
  assert.equal(deployment({ service: 'storage', storageAPI: 'https://untrusted.test/api' }).storageAPI, '/storage/api')
  assert.equal(deployment({ pathname: '/storage-other/' }).defaultModule, 'pulsar')
})

test('Available alone cannot approve a disk; freshness expires at the exact deadline', () => {
  assert.deepEqual(diskEligibility(disk, now), [])
  assert.ok(diskEligibility(disk, now + 1000).length)
  for (const change of [{ canCreatePool: false, reasons: ['Agent 未就绪'] }, { canCreatePool: undefined }, { fresh: false }, { validUntil: '' }, { validUntil: 'invalid' }, { state: 'Protected' }, { state: 'Offline' }]) {
    assert.ok(diskEligibility({ ...disk, ...change }, now).length, JSON.stringify(change))
  }
  assert.ok(diskEligibility({ ...disk, canCreatePool: false }, now, 'demo').length)
  assert.deepEqual(diskEligibility({ state: 'Available' }, now, 'demo'), [])
})

test('plan review accepts a newer scan of the same identity but rejects UID or device changes', () => {
  const entry = { disk, plan: { expiresAt: new Date(now + 10000).toISOString() } }
  const latest = { ...disk, validUntil: new Date(now + 5000).toISOString() }
  assert.ok(planEligibility(entry, now + 2000).length)
  assert.deepEqual(planEligibility(entry, now + 2000, 'cluster', latest), [])
  assert.ok(planEligibility(entry, now + 10000, 'cluster', { ...latest, validUntil: new Date(now + 20000).toISOString() }).length)
  for (const key of ['uid', 'nodeUID', 'node', 'path', 'serial', 'sizeBytes']) {
    const changed = { ...latest, [key]: 'different' }
    assert.equal(observedDisk(disk, changed).canCreatePool, false)
    assert.ok(planEligibility(entry, now, 'cluster', changed).some(reason => reason.includes('身份已变化')))
  }
  assert.ok(planEligibility(entry, now, 'cluster', { ...latest, canCreatePool: false, reasons: ['节点未就绪'] }).includes('节点未就绪'))
})

test('external ownership and binding/release uncertainty always prevent volume writes', () => {
  const managed = { status: 'Bound', canManage: true, management: 'diskfleet' }
  assert.equal(volumeReadOnlyReason(managed), '')
  for (const change of [{ management: 'external' }, { owners: [{ kind: 'StatefulSet', name: 'bookie' }] }, { canManage: false }, { canManage: undefined }, { bindingMessage: 'PV UID mismatch' }, { status: 'Released' }, { status: 'ReleaseRequested' }, { status: 'Releasing' }]) {
    assert.ok(volumeReadOnlyReason({ ...managed, ...change }), JSON.stringify(change))
  }
  assert.equal(volumeReadOnlyReason({ status: 'Bound' }, 'demo'), '')
})

test('pool preview preserves server plan request shape and safe generated names', () => {
  assert.deepEqual(poolPlanBody(disk, 'pool', 0), { name: 'pool-1-diskone', node: 'node-1', diskIds: ['disk-one'] })
})
