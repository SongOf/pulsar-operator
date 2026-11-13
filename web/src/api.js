// Credentials are entered in the login form and kept only for this tab.
const tokenKey = 'pulsar-operator-token'

export function initToken() {
  const url = new URL(window.location.href)
  // Retired query-token links never authenticate. Also clean direct console
  // URLs when no gateway is present; credentials belong in the login form.
  if (url.searchParams.has('token')) {
    url.searchParams.delete('token')
    window.history.replaceState(null, '', url)
  }
}
export function setToken(t) {
  try { sessionStorage.setItem(tokenKey, t) } catch { /* storage unavailable */ }
}

function token() {
  try { return sessionStorage.getItem(tokenKey) || '' } catch { return '' }
}

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status }
}
async function call(method, path, body, expected) {
  const headers = { 'Content-Type': 'application/json' }
  if (token()) headers.Authorization = 'Bearer ' + token()
  if (expected) {
    headers['X-Resource-UID'] = expected.uid
    headers['X-Resource-Version'] = expected.resourceVersion
  }
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  if (res.status === 204) return null
  const data = await res.json().catch(() => null)
  if (data === null) throw new ApiError(res.status, '控制台 API 返回了无效响应，请检查服务路由。')
  if (!res.ok) throw new ApiError(res.status, data.error || res.statusText)
  return data
}
export const kindPath = { PulsarCluster: 'pulsarclusters', BookKeeperCluster: 'bookkeeperclusters' }
const base = (kind, ns, name) => `/api/${kindPath[kind]}/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`
function preconditions(metadata) {
  if (!metadata?.uid || !metadata?.resourceVersion) throw new ApiError(428, '资源身份或版本缺失，请刷新后重试。')
  return { uid: metadata.uid, resourceVersion: metadata.resourceVersion }
}
export const api = {
  list: () => call('GET', '/api/clusters'),
  audit: (filters = {}, cursor = 0) => call('GET', '/api/audit?' + new URLSearchParams({ ...filters, cursor, limit: 50 })),
  create: (kind, obj) => call('POST', `/api/${kindPath[kind]}`, obj),
  patch: (kind, ns, name, spec, expected) => call('PATCH', base(kind, ns, name), { spec }, preconditions(expected)),
  remove: (kind, ns, name, expected) => call('DELETE', base(kind, ns, name), undefined, preconditions(expected)),
  restart: (kind, ns, name, component, expected) => call('POST', `${base(kind, ns, name)}/restart?component=${encodeURIComponent(component)}`, undefined, preconditions(expected)),
  pods: (kind, ns, name) => call('GET', `${base(kind, ns, name)}/pods`),
  events: (kind, ns, name) => call('GET', `${base(kind, ns, name)}/events`),
  metrics: (kind, ns, name, range) => call('GET', `${base(kind, ns, name)}/metrics?range=${range}`),
}
