import test from 'node:test'
import assert from 'node:assert/strict'
import { auditResult } from './audit-model.js'

test('audit distinguishes API acceptance from resource readiness and uncertain results', () => {
  assert.equal(auditResult({ action: 'request.create', result: 'Succeeded' }).label, '请求已接受')
  assert.equal(auditResult({ action: 'resource.patch', result: 'Succeeded' }).label, '写入已确认')
  for (const result of ['Unknown', undefined, 'unexpected']) {
    assert.equal(auditResult({ action: 'resource.delete', result }).tone, 'warn')
  }
  assert.equal(auditResult({ result: 'Failed' }).tone, 'bad')
})
