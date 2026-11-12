import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { transformSync } from 'esbuild'
import * as configuration from './configuration.js'

test('configuration changes are relative to the editing snapshot, including explicit removals', () => {
  const original = { unchanged: 'keep', changed: 'old', removed: 'delete me' }
  const snapshot = configuration.configurationBaseline(original, { uid: 'old', resourceVersion: '10' })
  original.concurrent = 'new'
  assert.deepEqual(configuration.configurationPatch(snapshot.config, [['unchanged', 'keep'], ['changed', 'new'], ['added', 'yes']]), {
    changed: 'new', added: 'yes', removed: null,
  })
  assert.equal(Object.hasOwn(snapshot.config, 'concurrent'), false)
  assert.deepEqual(configuration.configurationPatch(snapshot.config, configuration.configurationRows(snapshot.config)), {})
})

test('configuration keys use own properties when removing or adding entries', () => {
  const baseline = JSON.parse('{"constructor":"old","__proto__":"old"}')
  const patch = configuration.configurationPatch(baseline, [['__proto__', 'new']])
  assert.equal(patch.constructor, null)
  assert.equal(Object.hasOwn(patch, '__proto__'), true)
  assert.equal(patch.__proto__, 'new')
})

// Execute the actual JSX component and action hook with persistent hook cells.
// This checks the rerender/save interaction that a pure diff test would miss.
function editorHarness() {
  const cells = []
  let cursor = 0
  const React = {
    createElement: (type, props, ...children) => ({ type, props: props || {}, children: children.flat(Infinity) }),
    useState(initial) {
      const index = cursor++
      if (!(index in cells)) cells[index] = typeof initial === 'function' ? initial() : initial
      return [cells[index], value => { cells[index] = typeof value === 'function' ? value(cells[index]) : value }]
    },
  }
  const modules = { react: React, './configuration.js': configuration }
  const load = name => {
    const source = readFileSync(new URL(name, import.meta.url), 'utf8')
    const { code } = transformSync(source, { loader: 'jsx', format: 'cjs' })
    const module = { exports: {} }
    new Function('require', 'module', 'exports', code)(name => modules[name], module, module.exports)
    return module.exports
  }
  modules['./ui.jsx'] = load('./ui.jsx')
  const { ConfigEditor } = load('./ConfigEditor.jsx')
  return props => { cursor = 0; return ConfigEditor(props) }
}
function find(tree, predicate) {
  if (!tree || typeof tree !== 'object') return null
  if (predicate(tree)) return tree
  for (const child of tree.children || []) { const result = find(child, predicate); if (result) return result }
  return null
}

test('polled configuration never changes the draft baseline; a conflict retains user edits', async () => {
  const render = editorHarness()
  const attempted = []
  const props = {
    file: 'broker.conf', config: { maxMessageSize: '5242880' }, identity: { uid: 'cluster-a', resourceVersion: '10' },
    onChanged() {},
    async onSave(patch, identity) {
      attempted.push({ patch, identity })
      throw new Error('resource identity or version changed')
    },
  }
  const initial = render(props)
  find(initial, node => node.type === 'input' && node.props.value === '5242880').props.onChange({ target: { value: '6000000' } })
  const polled = { ...props, config: { maxMessageSize: '10485760', subscriptionExpirationTimeMinutes: '60' }, identity: { uid: 'cluster-a', resourceVersion: '20' } }
  const updated = render(polled)
  updated.props.onSubmit({ preventDefault() {} })
  await new Promise(resolve => setImmediate(resolve))
  assert.deepEqual(attempted, [{ patch: { maxMessageSize: '6000000' }, identity: { uid: 'cluster-a', resourceVersion: '10' } }])
  const conflict = render(polled)
  assert.ok(find(conflict, node => node.type === 'input' && node.props.value === '6000000'), 'draft must survive rejection')
  assert.ok(find(conflict, node => node.props.state?.error === 'resource identity or version changed'), 'conflict must be visible')
})

test('successful save establishes the returned version as the next editing baseline', async () => {
  const render = editorHarness()
  const calls = []
  const props = { config: { key: 'original' }, identity: { uid: 'a', resourceVersion: '1' }, onChanged() {},
    async onSave(patch, identity) {
      calls.push({ patch, identity })
      return { config: { key: patch.key }, identity: { uid: 'a', resourceVersion: '2' } }
    },
  }
  let tree = render(props)
  find(tree, node => node.type === 'input' && node.props.value === 'original').props.onChange({ target: { value: 'first edit' } })
  render(props).props.onSubmit({ preventDefault() {} })
  await new Promise(resolve => setImmediate(resolve))
  tree = render(props)
  find(tree, node => node.type === 'input' && node.props.value === 'first edit').props.onChange({ target: { value: 'second edit' } })
  render(props).props.onSubmit({ preventDefault() {} })
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(calls[1].identity.resourceVersion, '2')
  assert.deepEqual(calls[1].patch, { key: 'second edit' })
})
