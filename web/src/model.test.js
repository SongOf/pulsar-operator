// node --test web/src  (no dependencies)
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { bookieRemoval, nest, pulsarView, upgradePlan } from './model.js'

const ready = (n, image) => ({ replicas: n, readyReplicas: n, updatedReplicas: n, image, rolledOut: true })

function cluster({ image = 'pulsar:1', zk, bk, broker, proxy, bookies = 3, min = 3, decommission, overrides = {} } = {}) {
  const pc = {
    metadata: { name: 'demo', namespace: 'pulsar' },
    spec: {
      image,
      zookeeper: { replicas: 3, image: overrides.zookeeper },
      bookkeeper: { replicas: bookies, minReplicas: min, image: overrides.bookkeeper, storage: {} },
      broker: { replicas: 2, image: overrides.broker },
      proxy: { enabled: true, replicas: 1, image: overrides.proxy },
    },
    status: { zookeeper: zk ?? ready(3, image), bookkeeper: bk ?? ready(bookies, image), broker: broker ?? ready(2, image), proxy: proxy ?? ready(1, image) },
  }
  const child = { status: { decommission, bookieDetails: Array.from({ length: bookies }, (_, i) => ({ name: `demo-bookie-${i}`, bookieID: `b${i}` })) } }
  return pulsarView(pc, child)
}

const bookie = v => v.components.find(c => c.key === 'bookie')

test('nest builds a merge patch', () => {
  assert.deepEqual(nest(['bookkeeper', 'replicas'], 2), { bookkeeper: { replicas: 2 } })
  assert.deepEqual(nest(['broker', 'image'], null), { broker: { image: null } })
})

test('upgrade plan follows the operator order', () => {
  const plan = upgradePlan(cluster())
  assert.deepEqual(plan.map(c => c.key), ['zookeeper', 'bookie', 'broker', 'proxy'])
  assert.ok(plan.every(c => c.state === 'current'))
})

test('a cluster upgrade shows the first component rolling and the rest waiting for it', () => {
  const v = cluster({ image: 'pulsar:2', zk: { replicas: 3, readyReplicas: 3, updatedReplicas: 1, image: 'pulsar:2', rolledOut: false },
    bk: ready(3, 'pulsar:1'), broker: ready(2, 'pulsar:1'), proxy: ready(1, 'pulsar:1') })
  const [zk, bk, broker, proxy] = upgradePlan(v)
  assert.equal(zk.state, 'rolling')
  assert.match(zk.detail, /1\/3 updated/)
  assert.equal(bk.state, 'waiting')
  assert.match(bk.detail, /ZooKeeper/)
  assert.equal(broker.state, 'waiting')
  assert.equal(proxy.state, 'waiting')
})

test('a single-component upgrade targets only that component', () => {
  const v = cluster({ overrides: { broker: 'pulsar:2' }, broker: ready(2, 'pulsar:1') })
  const plan = upgradePlan(v)
  const broker = plan.find(c => c.key === 'broker')
  assert.equal(broker.desired, 'pulsar:2')
  assert.equal(broker.override, 'pulsar:2')
  assert.equal(broker.state, 'waiting')
  assert.equal(plan.find(c => c.key === 'zookeeper').desired, 'pulsar:1')
  assert.equal(plan.find(c => c.key === 'zookeeper').state, 'current')
})

test('one bookie can be removed from a healthy cluster above its minimum', () => {
  const v = cluster({ bookies: 4, min: 3 })
  const r = bookieRemoval(v, bookie(v))
  assert.equal(r.blocked, null)
  assert.equal(r.ordinal, 3)
  assert.equal(r.target.bookieID, 'b3')
})

test('no removal while another bookie is being decommissioned', () => {
  const v = cluster({ bookies: 4, decommission: { ordinal: 4, phase: 'Replicating' } })
  assert.match(bookieRemoval(v, bookie(v)).blocked, /being removed/)
})

test('no removal while the bookie StatefulSet has not caught up with the spec', () => {
  const v = cluster({ bookies: 4, bk: ready(5, 'pulsar:1') })
  assert.match(bookieRemoval(v, bookie(v)).blocked, /being removed/)
})

test('no removal at the minimum', () => {
  const v = cluster({ bookies: 3, min: 3 })
  assert.match(bookieRemoval(v, bookie(v)).blocked, /minimum of 3/)
})

test('no removal while a bookie is not ready', () => {
  const v = cluster({ bookies: 4, bk: { replicas: 4, readyReplicas: 3, updatedReplicas: 4, image: 'pulsar:1', rolledOut: false } })
  assert.match(bookieRemoval(v, bookie(v)).blocked, /must be ready/)
})
