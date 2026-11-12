import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

test('gateway logs use sanitized fields and legacy token links never reach upstreams', () => {
  const config = readFileSync(new URL('../../deploy/console/nginx.conf', import.meta.url), 'utf8')
  const format = config.match(/log_format console_safe ([\s\S]*?);/)?.[1]
  assert.ok(format)
  assert.match(format, /\$request_method \$uri \$server_protocol/)
  assert.doesNotMatch(format, /\$(?:request\b|request_uri\b|args\b|query_string\b|http_)/)
  assert.match(config, /access_log \/dev\/stdout console_safe;/)
  assert.match(config, /server\s*\{[\s\S]*?error_log \/dev\/null;/)
  assert.match(config, /if \(\$arg_token != ""\) \{ return 303 \$uri; \}/)
})
