import { test } from 'node:test'
import assert from 'node:assert/strict'
import { inventory } from './workspace.js'

function pulsar(name = 'main', namespace = 'team-a', uid = `${namespace}-${name}`) {
  return {
    apiVersion: 'pulsar.songof.dev/v1alpha1', kind: 'PulsarCluster',
    metadata: { name, namespace, uid },
    spec: {
      image: 'pulsar:1', zookeeper: { replicas: 3 },
      bookkeeper: { replicas: 4, minReplicas: 3, storage: { journal: { size: '1Gi' }, ledger: { size: '2Gi' } } },
      broker: { replicas: 2 }, proxy: { enabled: true, replicas: 1 },
    },
    status: { phase: 'Ready', serviceURL: 'pulsar://broker:6650', conditions: [{ type: 'Ready', status: 'True' }] },
  }
}

function bookkeeper(name = 'ledgers', namespace = 'team-a', parent) {
  const object = {
    apiVersion: 'pulsar.songof.dev/v1alpha1', kind: 'BookKeeperCluster',
    metadata: { name, namespace, uid: `${namespace}-${name}-bk` },
    spec: { image: 'pulsar:1', replicas: 4, minReplicas: 3, storage: {}, zookeeper: { servers: 'zk:2181' } },
    status: {
      phase: 'ScalingDown', bookies: { replicas: 4, readyReplicas: 4 },
      bookieDetails: [{ name: `${name}-0` }], decommission: { ordinal: 3 },
      conditions: [{ type: 'Ready', status: 'False' }],
    },
  }
  if (parent) object.metadata.ownerReferences = [{
    apiVersion: 'pulsar.songof.dev/v1alpha1', kind: 'PulsarCluster',
    name: parent.metadata.name, uid: parent.metadata.uid, controller: true,
  }]
  return object
}

test('the canonical child separates components and keeps managed bookie writes on its parent', () => {
  const pc = pulsar(), bk = bookkeeper('main', 'team-a', pc)
  const { brokers: [broker], bookies: [bookie], relationships } = inventory({ pulsar: [pc], allBookkeeper: [bk] })
  assert.deepEqual(broker.components.map(c => c.key), ['zookeeper', 'broker', 'proxy'])
  assert.deepEqual(bookie.components.map(c => c.key), ['bookie', 'autorecovery'])
  assert.deepEqual(broker.bookies, [])
  assert.equal(broker.decommission, null)
  assert.equal(broker.hideBookieStorage, true)
  assert.equal(broker.obj, pc)
  assert.equal(bookie.kind, 'PulsarCluster')
  assert.equal(bookie.name, 'main')
  assert.equal(bookie.ns, 'team-a')
  assert.equal(bookie.obj, pc)
  assert.equal(bookie.displayName, 'main')
  assert.equal(bookie.key, 'BookKeeperCluster/team-a/main')
  assert.equal(bookie.phase, 'ScalingDown')
  assert.equal(bookie.components[0].status, bk.status.bookies)
  assert.deepEqual(bookie.components[0].scalePath, ['bookkeeper', 'replicas'])
  assert.deepEqual(bookie.components[0].configPath, ['bookkeeper', 'config'])
  assert.deepEqual(bookie.storagePath, ['bookkeeper', 'storage'])
  assert.deepEqual(bookie.imagePath, ['bookkeeper', 'image'])
  assert.deepEqual(bookie.readTarget, { kind: 'BookKeeperCluster', ns: 'team-a', name: 'main' })
  assert.equal(bookie.deleteDisabled, true)
  assert.equal(bookie.readOnly, false)
  assert.equal(bookie.serviceURL, undefined)
  assert.deepEqual(relationships, [{
    state: 'linked', namespace: 'team-a', pulsarKey: 'PulsarCluster/team-a/main', pulsarName: 'main',
    bookkeeperKey: 'BookKeeperCluster/team-a/main', bookkeeperName: 'main',
  }])
})

test('managed bookie image overrides remain scoped to bookkeeper', () => {
  const pc = pulsar()
  pc.spec.bookkeeper.image = 'pulsar:bookie'
  const bk = bookkeeper('main', 'team-a', pc)
  const { brokers: [broker], bookies: [bookie] } = inventory({ pulsar: [pc], allBookkeeper: [bk] })
  assert.equal(broker.image, 'pulsar:1')
  assert.equal(bookie.image, 'pulsar:bookie')
  assert.deepEqual(bookie.imagePath, ['bookkeeper', 'image'])
})

test('a broker without its generated bookkeeper has a pending relationship', () => {
  const pc = pulsar()
  const { brokers: [broker], bookies, relationships } = inventory({ pulsar: [pc], allBookkeeper: [] })
  assert.equal(broker.relation.state, 'pending')
  assert.equal(broker.relation.bookkeeperKey, null)
  assert.equal(broker.relation.bookkeeperName, null)
  assert.equal(bookies.length, 0)
  assert.equal(relationships[0].state, 'pending')
})

test('standalone bookkeepers are writable and target themselves', () => {
  const bk = bookkeeper()
  const { bookies: [view], relationships } = inventory({ allBookkeeper: [bk] })
  assert.equal(view.kind, 'BookKeeperCluster')
  assert.equal(view.obj, bk)
  assert.deepEqual(view.components[0].scalePath, ['replicas'])
  assert.equal(view.readOnly, false)
  assert.equal(view.deleteDisabled, false)
  assert.equal(view.relation.state, 'standalone')
  assert.equal(view.relation.pulsarKey, null)
  assert.equal(relationships[0].state, 'standalone')
})

test('ownership requires the correct group, version, controller, name, namespace and nonempty UID', () => {
  const mutations = [
    bk => { bk.metadata.ownerReferences[0].apiVersion = 'other.example/v1alpha1' },
    bk => { bk.metadata.ownerReferences[0].apiVersion = 'pulsar.songof.dev/' },
    bk => { bk.metadata.ownerReferences[0].apiVersion = 'pulsar.songof.dev' },
    bk => { bk.metadata.ownerReferences[0].controller = false },
    bk => { delete bk.metadata.ownerReferences[0].controller },
    bk => { bk.metadata.ownerReferences[0].uid = 'replaced-parent' },
    bk => { delete bk.metadata.ownerReferences[0].uid },
    bk => { bk.metadata.ownerReferences[0].name = 'absent-parent' },
    bk => { bk.metadata.namespace = 'other-namespace' },
  ]
  for (const mutate of mutations) {
    const pc = pulsar(), bk = bookkeeper('main', 'team-a', pc)
    mutate(bk)
    const { brokers: [broker], bookies: [view] } = inventory({ pulsar: [pc], allBookkeeper: [bk] })
    assert.equal(broker.relation.state, 'pending')
    assert.equal(view.relation.state, 'orphan')
    assert.equal(view.relation.pulsarKey, null)
    assert.equal(view.readOnly, true)
    assert.equal(view.deleteDisabled, true)
  }
})

test('missing parent UID never permits a name-only ownership match', () => {
  const pc = pulsar(), bk = bookkeeper('main', 'team-a', pc)
  delete pc.metadata.uid
  delete bk.metadata.ownerReferences[0].uid
  const { brokers: [broker], bookies: [view] } = inventory({ pulsar: [pc], allBookkeeper: [bk] })
  assert.equal(broker.relation.state, 'pending')
  assert.equal(view.relation.state, 'orphan')
  assert.equal(view.readOnly, true)
})

test('duplicate names in distinct namespaces link only to their exact parent UID', () => {
  const a = pulsar('same', 'a'), b = pulsar('same', 'b')
  const ak = bookkeeper('same', 'a', a), bk = bookkeeper('same', 'b', b)
  const { brokers, bookies } = inventory({ pulsar: [b, a], allBookkeeper: [bk, ak] })
  assert.deepEqual(brokers.map(view => view.key), ['PulsarCluster/a/same', 'PulsarCluster/b/same'])
  assert.deepEqual(bookies.map(view => view.relation.pulsarKey), ['PulsarCluster/a/same', 'PulsarCluster/b/same'])
  assert.deepEqual(bookies.map(view => view.readTarget.ns), ['a', 'b'])
})

test('orphan ownership and a different controlling kind remain read-only', () => {
  const missing = bookkeeper('missing-owner', 'team-a', pulsar())
  const foreign = bookkeeper('foreign-owner')
  foreign.metadata.ownerReferences = [{ apiVersion: 'example.org/v1', kind: 'Other', name: 'parent', uid: 'other', controller: true }]
  const { bookies } = inventory({ allBookkeeper: [missing, foreign] })
  assert.ok(bookies.every(view => view.relation.state === 'orphan' && view.readOnly && view.deleteDisabled))
})

test('legacy children and standalone lists are deduplicated but map keys cannot establish ownership', () => {
  const pc = pulsar(), bk = bookkeeper('main', 'team-a', pc), single = bookkeeper('standalone')
  const data = { pulsar: [pc], children: { 'untrusted/map-key': bk }, bookkeeper: [bk, single] }
  const { bookies } = inventory(data)
  assert.equal(bookies.length, 2)
  assert.equal(bookies[0].relation.state, 'linked')
  assert.equal(bookies[1].relation.state, 'standalone')
  delete bk.metadata.ownerReferences
  const unowned = inventory({ pulsar: [pc], children: { 'team-a/main': bk }, bookkeeper: [] })
  assert.equal(unowned.brokers[0].relation.state, 'pending')
  assert.equal(unowned.bookies[0].relation.state, 'standalone')
})

test('the complete inventory is authoritative even when empty or legacy lists disagree', () => {
  const pc = pulsar(), stale = bookkeeper('main', 'team-a', pc), current = bookkeeper('current')
  const data = { pulsar: [pc], children: { 'team-a/main': stale }, bookkeeper: [stale], allBookkeeper: [current] }
  const result = inventory(data)
  assert.deepEqual(result.bookies.map(view => view.displayName), ['current'])
  assert.equal(result.brokers[0].relation.state, 'pending')
  data.allBookkeeper = []
  assert.equal(inventory(data).bookies.length, 0)
})

test('a valid owner on a noncanonical child remains visible but read-only', () => {
  const pc = pulsar(), child = bookkeeper('different-child', 'team-a', pc)
  const result = inventory({ pulsar: [pc], allBookkeeper: [child] })
  assert.equal(result.brokers[0].relation.state, 'pending')
  assert.equal(result.bookies[0].relation.state, 'orphan')
  assert.equal(result.bookies[0].relation.pulsarKey, null)
  assert.equal(result.bookies[0].relation.pulsarName, 'main')
  assert.equal(result.bookies[0].readOnly, true)
  assert.equal(result.bookies[0].deleteDisabled, true)
  assert.equal(result.bookies[0].obj, child)
  assert.equal(result.relationships.length, 2)
})

test('only the canonical child links when additional owned objects are returned', () => {
  const pc = pulsar(), child = bookkeeper('main', 'team-a', pc), other = bookkeeper('a-child', 'team-a', pc)
  const result = inventory({ pulsar: [pc], allBookkeeper: [child, other, child] })
  assert.equal(result.bookies.length, 2)
  assert.equal(result.relationships.length, 2)
  assert.equal(result.brokers[0].relation.bookkeeperName, 'main')
  assert.equal(result.bookies[0].relation.state, 'orphan')
  assert.equal(result.bookies[0].readOnly, true)
  assert.equal(result.bookies[1].relation.state, 'linked')
  assert.equal(result.bookies[1].obj, pc)
})

test('multiple controlling owner references fail closed even with a valid canonical parent', () => {
  for (const kind of ['PulsarCluster', 'OtherController']) {
    const pc = pulsar(), child = bookkeeper('main', 'team-a', pc)
    child.metadata.ownerReferences.push({
      apiVersion: 'pulsar.songof.dev/v1alpha1', kind, name: 'other', uid: 'other-uid', controller: true,
    })
    const result = inventory({ pulsar: [pc], allBookkeeper: [child] })
    assert.equal(result.brokers[0].relation.state, 'pending')
    assert.equal(result.bookies[0].relation.state, 'orphan')
    assert.equal(result.bookies[0].readOnly, true)
    assert.equal(result.bookies[0].deleteDisabled, true)
  }
})

test('noncontrolling references do not invalidate the one canonical controlling owner', () => {
  const pc = pulsar(), child = bookkeeper('main', 'team-a', pc)
  child.metadata.ownerReferences.push({ apiVersion: 'example.org/v1', kind: 'Observer', name: 'observer', uid: 'observer', controller: false })
  const result = inventory({ pulsar: [pc], allBookkeeper: [child] })
  assert.equal(result.brokers[0].relation.state, 'linked')
  assert.equal(result.bookies[0].relation.state, 'linked')
  assert.equal(result.bookies[0].obj, pc)
})

test('empty and not-yet-loaded inventories are valid', () => {
  assert.deepEqual(inventory(), { brokers: [], bookies: [], relationships: [] })
  assert.deepEqual(inventory(null), { brokers: [], bookies: [], relationships: [] })
})
