import test from 'node:test'
import assert from 'node:assert/strict'
import { createStorageAPI, saveStorageToken } from './storage-api.js'

function fixture(base = '/storage/api') {
  const calls = [], keys = [], tokens = new Map([['diskfleet-token', 'storage-secret'], ['pulsar-operator-token', 'pulsar-secret']])
  const storage = { getItem: key => { keys.push(key); return tokens.get(key) }, setItem: (k, v) => tokens.set(k, v), removeItem: k => tokens.delete(k) }
  const api = createStorageAPI(base, async (url, options) => { calls.push({ url, ...options }); return { ok: true, status: 200, json: async () => url.endsWith('/config') ? { mode: 'cluster', tokenRequired: true } : {} } }, () => storage)
  return { api, calls, keys, storage, tokens }
}
test('Storage root and gateway routes use only Storage credentials; public config is anonymous', async () => {
  for (const base of ['/api', '/storage/api']) {
    const f = fixture(base)
    await f.api.config()
    await f.api.node('worker.example', { limit: 20, offset: 40 })
    assert.equal(f.calls[0].headers.Authorization, undefined)
    assert.equal(f.calls[1].url, base + '/nodes/worker.example?limit=20&offset=40')
    assert.equal(f.calls[1].headers.Authorization, 'Bearer storage-secret')
    assert.deepEqual(f.keys, ['diskfleet-token'])
    saveStorageToken('replacement', f.storage)
    assert.equal(f.tokens.get('pulsar-operator-token'), 'pulsar-secret')
    saveStorageToken('', f.storage)
    assert.equal(f.tokens.has('diskfleet-token'), false)
  }
  assert.throws(() => createStorageAPI('https://untrusted.test/api'))
})
test('enable Agent always carries reviewed node UID and resourceVersion', async () => {
  const f = fixture()
  for (const node of [{ name: 'worker' }, { name: 'worker', uid: 'u' }]) assert.throws(() => f.api.enableAgent(node), /身份/)
  assert.equal(f.calls.length, 0)
  await f.api.enableAgent({ name: 'worker', uid: 'node-uid', resourceVersion: '42' })
  assert.equal(f.calls[0].url, '/storage/api/nodes/worker/agent')
  assert.equal(f.calls[0].method, 'POST')
  assert.deepEqual(JSON.parse(f.calls[0].body), { uid: 'node-uid', resourceVersion: '42' })
})
test('plan submission uses server confirmation and release retries preserve the original PVC UID', async () => {
  const f = fixture()
  await f.api.createPool({ planId: 'p1', confirmation: 'approved', irrelevant: 'ignored' })
  assert.deepEqual(JSON.parse(f.calls[0].body), { planId: 'p1', confirmation: 'approved' })
  assert.throws(() => f.api.release({ id: 'ns~same-name' }, 'cluster'), /身份/)
  await f.api.release({ id: 'ns~same-name', pvcUid: 'old-pvc-uid' }, 'cluster')
  assert.equal(f.calls[1].url, '/storage/api/volumes/ns~same-name?uid=old-pvc-uid')
  assert.equal(f.calls[1].method, 'DELETE')
})
test('backend conflict reason is surfaced and failed configuration never marks the API ready', async () => {
  const api = createStorageAPI('/api', async () => ({ ok: false, status: 409, json: async () => ({ detail: '节点身份已变化，请刷新后重试' }) }), () => ({ getItem: () => '' }))
  await assert.rejects(api.enableAgent({ name: 'worker', uid: 'old', resourceVersion: '1' }), error => error.status === 409 && error.message.includes('身份已变化'))
  for (const value of [null, { mode: 'wrong', tokenRequired: false }, { mode: 'demo' }, '<html>wrong service</html>']) {
    const wrong = createStorageAPI('/api', async () => ({ ok: true, status: 200, json: async () => value }))
    await assert.rejects(wrong.config())
  }
})

test('pool review sends exactly the frozen identity and rejects missing identity or rejection reason locally', async () => {
  const f = fixture()
  const expected = { uid: 'pool-uid', resourceVersion: '17', runID: 'run-one', resultDigest: 'digest-one' }
  await f.api.pool('pool-one')
  assert.equal(f.calls[0].url, '/storage/api/pools/pool-one')
  for (const key of Object.keys(expected)) {
    assert.throws(() => f.api.reviewPool('pool-one', { ...expected, [key]: '' }, 'Approved'), /身份/)
  }
  assert.throws(() => f.api.reviewPool('pool-one', expected, 'Rejected', '   '), /理由/)
  assert.throws(() => f.api.reviewPool('pool-one', expected, 'anything'), /无效/)
  assert.equal(f.calls.length, 1)
  await f.api.reviewPool('pool-one', { ...expected, ignored: 'ignored' }, 'Rejected', '  校验失败  ')
  assert.equal(f.calls[1].url, '/storage/api/pools/pool-one/review')
  assert.deepEqual(JSON.parse(f.calls[1].body), { ...expected, decision: 'Rejected', note: '校验失败' })
  assert.equal(f.calls[1].headers.Authorization, 'Bearer storage-secret')
})
