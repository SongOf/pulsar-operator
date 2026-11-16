import { after, before, test } from 'node:test'
import assert from 'node:assert/strict'
import { reactViews } from '../scripts/react-test-loader.js'
import { benchmarkPool, approvedPool } from './testdata/pools.js'

let renderer
before(async () => { renderer = await reactViews() })
after(async () => { await renderer?.close() })
const noop = () => {}
const now = Date.parse('2026-11-16T08:00:00Z')
const disk = { id: 'disk-1', uid: 'uid', node: 'worker-1', nodeUID: 'node-uid', name: 'sdb', path: '/dev/disk/by-id/wwn-1', serial: 'S1', sizeBytes: 100 * 1024 ** 3, state: 'Available', canCreatePool: true, fresh: true, validUntil: new Date(now + 1000).toISOString() }

test('physical node without Agent stays visible with truthful state and an enable action', () => {
  const node = { name: 'new-worker', uid: 'node-uid', resourceVersion: '3', state: 'Ready', agentStatus: 'NotEnrolled', canEnableAgent: true, canCreatePool: false, enrolled: false, disks: 0, reasons: ['等待 Agent 上报磁盘'] }
  const table = renderer.render('NodesTable', { items: [node], onNode: noop })
  assert.match(table, /new-worker/)
  assert.match(table, /未启用/)
  const detail = renderer.render('NodeSummary', { node, mode: 'cluster', onEnable: noop })
  assert.match(detail, /<button class="primary">启用 Agent<\/button>/)
  assert.match(detail, /等待 Agent 上报磁盘/)
  const missing = renderer.render('NodeSummary', { node: { ...node, canEnableAgent: false, agentStatus: 'NotInstalled', enableReasons: ['Agent DaemonSet 尚未部署'] }, mode: 'cluster', onEnable: noop })
  assert.match(missing, /<button[^>]*disabled=""[^>]*>启用 Agent/)
  assert.match(missing, /Agent DaemonSet 尚未部署/)
})
test('real JSX disables expired disk selection and plan preview, while allowing fresh scans', () => {
  const props = { items: [disk], now, mode: 'cluster', onToggle: noop, onDetail: noop, onNode: noop }
  const before = renderer.render('DisksTable', props)
  assert.doesNotMatch(before, /type="checkbox"[^>]*disabled/)
  const after = renderer.render('DisksTable', { ...props, now: now + 1000 })
  assert.match(after, /type="checkbox"[^>]*disabled=""/)
  assert.match(after, /上报已过期/)
  const dialog = renderer.render('PoolPlanDialog', { api: {}, disks: [disk], mode: 'cluster', now: now + 1000, onClose: noop, onCreated: noop })
  assert.match(dialog, /<button type="submit"[^>]*disabled=""/)
  const demo = renderer.render('DisksTable', { ...props, mode: 'demo', items: [{ ...disk, canCreatePool: false, reasons: ['物理节点尚未启用 Agent'] }] })
  assert.match(demo, /type="checkbox"[^>]*disabled=""/)
  assert.match(demo, /物理节点尚未启用 Agent/)
})
test('external, releasing and unverified PVCs expose details without destructive controls', () => {
  const volume = { id: 'default~data', name: 'data', namespace: 'default', status: 'Bound', canManage: true, management: 'diskfleet', sizeBytes: 1024 ** 3 }
  const render = row => renderer.render('VolumesTable', { items: [row], mode: 'cluster', onDetail: noop, onExpand: noop, onRelease: noop })
  assert.match(render(volume), />扩容<\/button>/)
  for (const change of [{ management: 'external' }, { canManage: false, bindingMessage: '实际 PV 归属待核验' }, { status: 'ReleaseRequested' }]) {
    const html = render({ ...volume, ...change })
    assert.match(html, /只读详情/)
    assert.doesNotMatch(html, />扩容<\/button>|>释放<\/button>/)
  }
})
test('React escapes device and audit metadata rather than evaluating HTML', () => {
  const attack = '<img src=x onerror="alert(1)">'
  const html = renderer.render('DisksTable', { items: [{ ...disk, name: attack }], now, mode: 'cluster', onToggle: noop, onDetail: noop, onNode: noop })
  assert.ok(!html.includes(attack))
  assert.match(html, /&lt;img/)
  const event = renderer.render('EventsTable', { items: [{ id: 1, message: attack }] })
  assert.ok(!event.includes(attack))
})

test('pool list and PVC form enforce the approved current report instead of Ready alone', () => {
  const props = { onPool: noop, onNode: noop, onVolume: noop }
  const unreviewed = { ...benchmarkPool(), state: 'Ready' }
  const table = renderer.render('PoolsTable', { ...props, items: [unreviewed] })
  assert.match(table, /人工审核/)
  assert.match(table, /<button disabled="">申请 PVC<\/button>/)
  const approved = renderer.render('PoolsTable', { ...props, items: [approvedPool()] })
  assert.match(approved, /<button>申请 PVC<\/button>/)
  const form = renderer.render('CreateVolumeDialog', { api: {}, pools: [unreviewed], mode: 'cluster', onClose: noop, onDone: noop })
  assert.match(form, /当前没有通过压测、清理与人工审核/)
  assert.match(form, /type="submit"[^>]*disabled=""/)
})

test('pool report shows actual workload metrics with units and identifies demo results', () => {
  const report = benchmarkPool().benchmark
  const html = renderer.render('BenchmarkReport', { report, mode: 'demo' })
  for (const label of ['模拟压测报告', '没有对真实磁盘执行', 'MiB/s', 'IOPS', '平均延迟（ms）', 'P95 延迟（ms）', 'P99 延迟（ms）', '错误数', '顺序写', '随机读', '1 GiB', '临时测试卷已清理']) assert.ok(html.includes(label), label)
  const missing = structuredClone(report)
  delete missing.workloads[0].latencyMeanNs
  assert.match(renderer.render('BenchmarkReport', { report: missing, mode: 'cluster' }), /<td>—<\/td>/)
})

test('review dialog remains bound to its old report and approved detail has no review action', () => {
  const pool = benchmarkPool()
  const stale = renderer.render('PoolReviewDialog', { pool, currentPool: { ...pool, resourceVersion: '18' }, api: {}, mode: 'cluster', onClose: noop, onDone: noop })
  assert.match(stale, /版本或压测结果已变化/)
  assert.match(stale, /type="submit"[^>]*disabled=""/)
  assert.match(stale, /<dd>17<\/dd>/)
  const detail = renderer.render('PoolDetails', { pool: approvedPool(), mode: 'cluster', onReview: noop, onNode: noop, onVolume: noop })
  assert.doesNotMatch(detail, />审核压测结果<\/button>/)
  assert.match(detail, /不提供撤回或重复审核/)
})
