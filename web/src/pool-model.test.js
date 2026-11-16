import test from 'node:test'
import assert from 'node:assert/strict'
import { benchmarkReasons, poolAllocationReasons, poolReviewReasons, reviewIdentity, metric } from './pool-model.js'
import { benchmarkPool, approvedPool } from './testdata/pools.js'

test('Ready alone, missing approval and stale approval identity cannot allocate a volume', () => {
  assert.ok(poolAllocationReasons({ state: 'Ready' }).length)
  assert.ok(poolAllocationReasons(benchmarkPool()).length)
  assert.deepEqual(poolAllocationReasons(approvedPool()), [])
  for (const change of [pool => { pool.canCreateVolume = false }, pool => { delete pool.canCreateVolume }, pool => { pool.allocationApproval.decision = 'Rejected' }, pool => { pool.allocationApproval.poolUID = 'old-pool' }, pool => { pool.allocationApproval.runID = 'old-run' }, pool => { pool.allocationApproval.resultDigest = 'old-digest' }, pool => { pool.benchmark.poolUID = 'old-pool' }]) {
    const pool = approvedPool(); change(pool)
    assert.ok(poolAllocationReasons(pool).length)
  }
})
test('success requires cleanup and a complete actual workload report before review or allocation', () => {
  assert.deepEqual(benchmarkReasons(benchmarkPool()), [])
  for (const mutate of [report => { report.phase = 'Running' }, report => { report.phase = 'Interrupted' }, report => { report.phase = 'Failed' }, report => { report.cleanupVerified = false }, report => { report.workloads.pop() }, report => { report.workloads[3].name = 'seq-write' }, report => { report.workloads[0].errors = 1 }, report => { delete report.workloads[0].latencyP99Ns }, report => { report.workloads[0].iops = null }, report => { delete report.device.serial }, report => { delete report.completedAt }, report => { report.vgName = 'other-vg' }]) {
    const pool = benchmarkPool(); mutate(pool.benchmark)
    assert.ok(poolReviewReasons(pool).length)
    const approved = approvedPool(); approved.benchmark = pool.benchmark
    assert.ok(poolAllocationReasons(approved).length)
  }
})
test('review baseline is immutable and any UID, resourceVersion, run or digest change blocks stale review', () => {
  const pool = benchmarkPool(), expected = reviewIdentity(pool)
  assert.deepEqual(poolReviewReasons(pool, expected), [])
  assert.ok(Object.isFrozen(expected))
  for (const mutate of [pool => { pool.uid = 'replacement' }, pool => { pool.resourceVersion = '18' }, pool => { pool.benchmark.runID = 'new-run' }, pool => { pool.benchmark.resultDigest = 'new-result' }]) {
    const changed = benchmarkPool(); mutate(changed)
    assert.ok(poolReviewReasons(changed, expected).some(reason => reason.includes('已变化')))
  }
  assert.equal(expected.resourceVersion, '17')
  assert.ok(poolReviewReasons(null, expected).length)
  assert.ok(poolReviewReasons({ ...pool, nodeUID: 'replacement-node' }, expected).length)
  assert.ok(poolReviewReasons({ ...pool, devices: [{ ...pool.devices[0], serial: 'replacement-device' }] }, expected).length)
})
test('Rejected report may be reviewed again while Approved reports cannot be changed', () => {
  const rejected = benchmarkPool()
  rejected.allocationApproval = { decision: 'Rejected', note: 'latency too high' }
  assert.deepEqual(poolReviewReasons(rejected), [])
  assert.ok(poolReviewReasons(approvedPool()).some(reason => reason.includes('不支持撤回')))
})
test('absent metrics are not fabricated as zero and unit conversions are explicit', () => {
  for (const value of [null, undefined, '0', NaN, -1]) assert.equal(metric(value), '—')
  assert.equal(metric(0), '0')
  assert.equal(metric(256 * 1024 ** 2, 1024 ** 2), '256')
  assert.equal(metric(1500000, 1e6, 6), '1.5')
})
