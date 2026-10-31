// The console API. A token, when the operator requires one, comes from ?token= once and is kept for the tab.
const tokenKey = 'pulsar-operator-token'

export function initToken() {
  const url = new URL(window.location.href)
  const t = url.searchParams.get('token')
  if (t) {
    try { sessionStorage.setItem(tokenKey, t) } catch { /* storage unavailable */ }
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

async function call(method, path, body) {
  const headers = { 'Content-Type': 'application/json' }
  if (token()) headers.Authorization = 'Bearer ' + token()
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  if (res.status === 204) return null
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new ApiError(res.status, data.error || res.statusText)
  return data
}

export const kindPath = { PulsarCluster: 'pulsarclusters', BookKeeperCluster: 'bookkeeperclusters' }
const base = (kind, ns, name) => `/api/${kindPath[kind]}/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`

export const api = {
  list: () => call('GET', '/api/clusters'),
  create: (kind, obj) => call('POST', `/api/${kindPath[kind]}`, obj),
  patch: (kind, ns, name, spec) => call('PATCH', base(kind, ns, name), { spec }),
  remove: (kind, ns, name) => call('DELETE', base(kind, ns, name)),
  restart: (kind, ns, name, component) => call('POST', `${base(kind, ns, name)}/restart?component=${component}`),
  pods: (kind, ns, name) => call('GET', `${base(kind, ns, name)}/pods`),
  events: (kind, ns, name) => call('GET', `${base(kind, ns, name)}/events`),
  metrics: (kind, ns, name, range) => call('GET', `${base(kind, ns, name)}/metrics?range=${range}`),
}
