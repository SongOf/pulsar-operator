#!/usr/bin/env node
'use strict'

// Integration entrypoint: real Go demo data rendered by the same React views
// used in the browser. No network listener, host devices or existing cluster.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const os = require('node:os')
const { spawnSync } = require('node:child_process')
const root = path.resolve(__dirname, '../..')

async function check() {
  const configured = process.env.DISKFLEET_API || path.join(root, 'bin/diskfleet-api')
  const binary = fs.existsSync(configured) ? configured : null
  const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), 'diskfleet-react-check-'))
  let snapshot
  try {
    snapshot = spawnSync(binary || 'go', [...(binary ? [] : ['run', './cmd/storage-api']), '--mode', 'demo', '--host', '127.0.0.1', '--snapshot', '--state-dir', stateDir], {
      cwd: root, env: { ...process.env, MODE: 'demo', API_HOST: '127.0.0.1', API_PORT: '8080', API_TOKEN: '' },
      encoding: 'utf8', maxBuffer: 8 * 1024 * 1024, timeout: 120000,
    })
  } finally { fs.rmSync(stateDir, { recursive: true, force: true }) }
  if (snapshot.error) throw snapshot.error
  if (snapshot.status !== 0) throw new Error(snapshot.stderr || 'Go API snapshot failed')
  const data = JSON.parse(snapshot.stdout)
  assert.equal(data.overview.mode, 'demo')
  const { reactViews } = await import('./react-test-loader.js')
  const { poolAllocationReasons, poolReviewReasons } = await import('../src/pool-model.js')
  const renderer = await reactViews()
  const noop = () => {}
  try {
    for (const pool of data.pools.items) {
      if (pool.canCreateVolume === true) assert.deepEqual(poolAllocationReasons(pool), [], 'Go approved pool must be allocatable by the UI')
      if (pool.canReview === true) assert.deepEqual(poolReviewReasons(pool), [], 'Go reviewable report must be understood by the UI')
    }
    const cases = [
      ['StorageOverview', { data: { summary: data.overview, nodes: data.nodes, disks: { ...data.disks, items: data.disks.items.slice(0, 24) } }, onNavigate: noop, onNode: noop, onDisk: noop }],
      ['NodesTable', { items: data.nodes.items, onNode: noop }],
      ['NodeSummary', { node: data.nodes.items[0], mode: 'demo', onEnable: noop }],
      ['DisksTable', { items: data.disks.items.slice(0, 20), mode: 'demo', now: Date.now(), onToggle: noop, onDetail: noop, onNode: noop }],
      ['PoolsTable', { items: data.pools.items, onVolume: noop, onNode: noop, onPool: noop }],
      ['VolumesTable', { items: data.volumes.items, mode: 'demo', onDetail: noop, onExpand: noop, onRelease: noop }],
      ['EventsTable', { items: data.events.items }], ['StorageGuide', {}],
    ]
    if (data.pools.items.length) cases.push(['PoolDetails', { pool: data.pools.items[0], mode: 'demo', onReview: noop, onVolume: noop, onNode: noop }])
    for (const [name, props] of cases) {
      const html = renderer.render(name, props)
      assert.ok(html.length > 40, `${name} is empty`)
      assert.ok(!html.includes('undefined'), `${name} contains a missing field`)
      console.log(`PASS React ${name} with Go demo snapshot`)
    }
  } finally { await renderer.close() }
}
check().catch(error => { console.error(error); process.exitCode = 1 })
