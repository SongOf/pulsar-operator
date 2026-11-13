import test from 'node:test'
import assert from 'node:assert/strict'
import { MODULES, parseRoute, routePath, pulsarRoute, storageRoute, entityRoute } from './routes.js'

test('each module page has a canonical route that round trips', () => {
  assert.deepEqual(MODULES.map(module => module.key), ['pulsar', 'storage'])
  for (const module of MODULES) {
    for (const page of module.pages) {
      const expected = { module: module.key, page: page.key }
      const path = `${module.key}/${page.key}`
      assert.deepEqual(parseRoute('#/' + path), expected)
      assert.equal(routePath(expected), path)
    }
    assert.deepEqual(parseRoute('#/' + module.key), { module: module.key, page: 'overview' })
  }
})

test('legacy Pulsar bookmarks retain their list or resource identity', () => {
  for (const page of ['overview', 'brokers', 'bookies']) {
    assert.equal(routePath(parseRoute('#/' + page)), 'pulsar/' + page)
  }
  for (const page of ['brokers', 'bookies']) {
    const expected = { module: 'pulsar', page, ns: 'messaging', name: 'production' }
    assert.deepEqual(parseRoute(`#/${page}/messaging/production`), expected)
    assert.deepEqual(parseRoute(`#/pulsar/${page}/messaging/production`), expected)
    assert.equal(routePath(expected), `pulsar/${page}/messaging/production`)
  }
})

test('invalid routes cannot select unknown modules, pages or partial identities', () => {
  for (const hash of ['', '#/', '#/agent', '#/pulsar/unknown', '#/brokers/ns',
    '#/pulsar/bookies/ns/', '#/pulsar/bookies//name', '#/pulsar/brokers/ns/name/extra',
    '#/pulsar/overview/ns/name', '#/pulsar/brokers/ns/name%2Fextra', '#/%E0%A4%A']) {
    assert.deepEqual(parseRoute(hash), { module: 'pulsar', page: 'overview' }, hash)
  }
  for (const hash of ['#/storage/unknown', '#/storage/disks/extra', '#/storage/https:%2F%2Fother.test']) {
    assert.deepEqual(parseRoute(hash), { module: 'storage', page: 'overview' }, hash)
  }
})

test('resource navigation consistently points into the Pulsar module', () => {
  assert.equal(entityRoute('PulsarCluster/ns/name'), 'pulsar/brokers/ns/name')
  assert.equal(entityRoute('BookKeeperCluster/ns/name'), 'pulsar/bookies/ns/name')
  for (const key of ['Unknown/ns/name', 'BookKeeperCluster/ns', 'PulsarCluster//name', 'PulsarCluster/ns/name/extra']) {
    assert.equal(entityRoute(key), 'pulsar/overview')
  }
  const encoded = pulsarRoute('bookies', 'test space', 'name#fragment')
  assert.equal(encoded, 'pulsar/bookies/test%20space/name%23fragment')
  assert.deepEqual(parseRoute('#/' + encoded), { module: 'pulsar', page: 'bookies', ns: 'test space', name: 'name#fragment' })
})

test('navigation builders validate page names and resource boundaries', () => {
  assert.equal(pulsarRoute(), 'pulsar/overview')
  assert.equal(storageRoute(), 'storage/overview')
  assert.equal(pulsarRoute('unknown', 'ns', 'name'), 'pulsar/overview')
  assert.equal(pulsarRoute('overview', 'ns', 'name'), 'pulsar/overview')
  assert.equal(pulsarRoute('brokers', 'ns'), 'pulsar/brokers')
  assert.equal(pulsarRoute('bookies', 'ns', 'name/extra'), 'pulsar/bookies')
  assert.equal(storageRoute('https://other.test'), 'storage/overview')
})
