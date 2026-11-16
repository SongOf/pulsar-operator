const tokenKey = 'diskfleet-token'

export class StorageError extends Error {
  constructor(status, message) { super(message); this.status = status }
}

export function saveStorageToken(value, storage = sessionStorage) {
  try {
    if (value) storage.setItem(tokenKey, value)
    else storage.removeItem(tokenKey)
  } catch { throw new Error('浏览器不允许会话存储，无法保存 Storage 凭证。') }
}

export function createStorageAPI(base = '/storage/api', fetcher = (...args) => fetch(...args), storage = () => sessionStorage) {
  if (!['/api', '/storage/api'].includes(base)) throw new Error('Storage API 地址无效。')
  const call = async (path, { method = 'GET', body, signal, anonymous = false } = {}) => {
    const headers = { Accept: 'application/json' }
    let token = ''
    if (!anonymous) { try { token = storage().getItem(tokenKey) || '' } catch { /* session unavailable */ } }
    if (token) headers.Authorization = 'Bearer ' + token
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    let response
    try { response = await fetcher(base + path, { method, headers, signal, body: body === undefined ? undefined : JSON.stringify(body) }) }
    catch (error) {
      if (error.name === 'AbortError') throw error
      throw new StorageError(0, '无法连接 Storage API，请检查服务后重试。')
    }
    const result = await response.json().catch(() => null)
    if (!response.ok) {
      if (response.status === 401 || response.status === 403) throw new StorageError(response.status, 'Storage 凭证缺失、失效或权限不足，请设置独立的 Storage Token。')
      throw new StorageError(response.status, typeof result?.detail === 'string' ? result.detail : `Storage 请求失败（HTTP ${response.status}）。`)
    }
    if (!result || typeof result !== 'object') throw new StorageError(response.status, 'Storage API 返回无效响应，请检查服务路由。')
    return result
  }
  const list = (kind, options = {}, signal) => {
    if (!['nodes', 'disks', 'pools', 'volumes', 'events'].includes(kind)) throw new Error('未知存储列表。')
    return call('/' + kind + '?' + new URLSearchParams({ limit: 20, offset: 0, ...options }), { signal })
  }
  return {
    config: async signal => {
      const config = await call('/config', { signal, anonymous: true })
      if (!['demo', 'cluster'].includes(config.mode) || typeof config.tokenRequired !== 'boolean') throw new StorageError(0, 'Storage API 配置无效，请检查服务路由。')
      return config
    },
    overview: signal => call('/overview', { signal }), list,
    node: (name, options = {}, signal) => call('/nodes/' + encodeURIComponent(name) + '?' + new URLSearchParams({ limit: 20, offset: 0, ...options }), { signal }),
    enableAgent: node => {
      if (!node?.uid || !node?.resourceVersion) throw new StorageError(428, '节点身份或版本缺失，请刷新节点后重试。')
      return call('/nodes/' + encodeURIComponent(node.name) + '/agent', { method: 'POST', body: { uid: node.uid, resourceVersion: node.resourceVersion } })
    },
    plan: body => call('/pools/plan', { method: 'POST', body }),
    createPool: plan => call('/pools', { method: 'POST', body: { planId: plan.planId, confirmation: plan.confirmation } }),
    pool: (id, signal) => call('/pools/' + encodeURIComponent(id), { signal }),
    reviewPool: (id, expected, decision, note = '') => {
      if (!['uid', 'resourceVersion', 'runID', 'resultDigest'].every(key => typeof expected?.[key] === 'string' && expected[key])) throw new StorageError(428, '存储池或压测身份、资源版本缺失，请刷新后重试。')
      if (!['Approved', 'Rejected'].includes(decision)) throw new StorageError(400, '无效审核决定。')
      if (decision === 'Rejected' && !note.trim()) throw new StorageError(400, '拒绝审核时必须填写理由。')
      const { uid, resourceVersion, runID, resultDigest } = expected
      return call('/pools/' + encodeURIComponent(id) + '/review', { method: 'POST', body: { uid, resourceVersion, runID, resultDigest, decision, note: note.trim() } })
    },
    createVolume: body => call('/volumes', { method: 'POST', body }),
    expand: (volume, sizeGiB) => call('/volumes/' + encodeURIComponent(volume.id) + '/expand', { method: 'POST', body: { sizeGiB } }),
    release: (volume, mode) => {
      if (mode === 'cluster' && !volume.pvcUid) throw new StorageError(428, 'PVC 身份缺失，请刷新并核对后再释放。')
      return call('/volumes/' + encodeURIComponent(volume.id) + (mode === 'cluster' ? '?uid=' + encodeURIComponent(volume.pvcUid) : ''), { method: 'DELETE' })
    },
  }
}
