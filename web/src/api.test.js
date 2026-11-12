import { test } from 'node:test'
import assert from 'node:assert/strict'
import { api, ApiError, initToken, setToken } from './api.js'

function browser(t, href = 'https://console.example/#/overview') {
  const originals = { window: globalThis.window, sessionStorage: globalThis.sessionStorage, fetch: globalThis.fetch }
  t.after(() => Object.assign(globalThis, originals))
  const storage = new Map()
  let replaced
  globalThis.sessionStorage = { getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value) }
  globalThis.window = { location: { href }, history: { replaceState: (_state, _title, url) => { replaced = String(url) } } }
  return { storage, get replaced() { return replaced } }
}

test('retired URL token never authenticates and is removed without changing the route', t => {
  const env = browser(t, 'https://console.example/?token=nonsecret-old-link&view=1#/bookies/team/bk')
  initToken()
  assert.equal(env.storage.size, 0)
  assert.equal(env.replaced, 'https://console.example/?view=1#/bookies/team/bk')
  setToken('nonsecret-form-token')
  assert.equal(env.storage.get('pulsar-operator-token'), 'nonsecret-form-token')
  initToken()
  assert.equal(env.storage.get('pulsar-operator-token'), 'nonsecret-form-token')
})

test('every existing-resource mutation transmits the reviewed UID and resourceVersion', async t => {
  browser(t)
  const calls = []
  globalThis.fetch = async (path, options) => {
    calls.push({ path, ...options })
    return options.method === 'DELETE' ? new Response(null, { status: 204 }) : Response.json({})
  }
  const expected = { uid: 'parent-uid', resourceVersion: '45' }
  await api.patch('PulsarCluster', 'team', 'p', { bookkeeper: { replicas: 4 } }, expected)
  await api.restart('PulsarCluster', 'team', 'p', 'bookie', expected)
  await api.remove('PulsarCluster', 'team', 'p', expected)
  assert.deepEqual(calls.map(call => call.method), ['PATCH', 'POST', 'DELETE'])
  for (const call of calls) {
    assert.equal(call.headers['X-Resource-UID'], 'parent-uid')
    assert.equal(call.headers['X-Resource-Version'], '45')
  }
  assert.deepEqual(JSON.parse(calls[0].body), { spec: { bookkeeper: { replicas: 4 } } })
})

test('missing identity fails locally and server conflicts remain visible to callers', async t => {
  browser(t)
  globalThis.fetch = async () => { throw new Error('must not send without preconditions') }
  assert.throws(() => api.remove('PulsarCluster', 'n', 'p'), error => error instanceof ApiError && error.status === 428)
  globalThis.fetch = async () => Response.json({ error: 'resource identity or version changed' }, { status: 409 })
  await assert.rejects(api.patch('PulsarCluster', 'n', 'p', {}, { uid: 'old', resourceVersion: '1' }), error => error.status === 409 && /version changed/.test(error.message))
})

test('HTML success responses cannot be mistaken for a successful API operation', async t => {
  browser(t)
  globalThis.fetch = async () => new Response('<html>wrong route</html>', { headers: { 'Content-Type': 'text/html' } })
  await assert.rejects(api.list(), /API/)
})
