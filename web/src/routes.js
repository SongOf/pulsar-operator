// Shared navigation only. Backend requests and credentials belong to each workspace.
export const PULSAR_PAGES = [
  { key: 'overview', label: '集群总览' },
  { key: 'brokers', label: 'Broker 组件' },
  { key: 'bookies', label: 'Bookie 组件' },
  { key: 'audit', label: '操作审计' },
]

export const STORAGE_PAGES = [
  { key: 'overview', label: '存储总览' },
  { key: 'nodes', label: '物理节点' },
  { key: 'disks', label: '物理磁盘' },
  { key: 'pools', label: '存储池' },
  { key: 'volumes', label: 'PV / PVC' },
  { key: 'events', label: '操作审计' },
  { key: 'guide', label: '接盘指南' },
]

export const MODULES = [
  { key: 'pulsar', label: 'Pulsar', description: '消息集群与 BookKeeper', mark: 'P', pages: PULSAR_PAGES },
  { key: 'storage', label: 'Storage', description: '物理磁盘与存储池', mark: 'S', pages: STORAGE_PAGES },
]

export const isStoragePage = value => STORAGE_PAGES.some(page => page.key === value)
const isPulsarPage = value => PULSAR_PAGES.some(page => page.key === value)
const isIdentity = value => typeof value === 'string' && value.length > 0 && !value.includes('/')

export function parseRoute(hash = '') {
  let parts
  try { parts = hash.replace(/^#\/?/, '').split('/').map(decodeURIComponent) }
  catch { return { module: 'pulsar', page: 'overview' } }
  // Existing bookmarks retain their component and namespace when opening the
  // new module entry. The shell replaces their URL without adding history.
  if (isPulsarPage(parts[0])) parts.unshift('pulsar')
  const [module, page = 'overview', ns, name] = parts
  if (module === 'storage') {
    return { module, page: parts.length <= 2 && isStoragePage(page) ? page : 'overview' }
  }
  if (module === 'pulsar' && isPulsarPage(page)) {
    if (parts.length === 2) return { module, page }
    if (parts.length === 4 && ['brokers', 'bookies'].includes(page) && isIdentity(ns) && isIdentity(name)) return { module, page, ns, name }
  }
  return { module: 'pulsar', page: 'overview' }
}

export function pulsarRoute(page = 'overview', ns, name) {
  const selected = isPulsarPage(page) ? page : 'overview'
  const parts = ['pulsar', selected]
  if (['brokers', 'bookies'].includes(selected) && isIdentity(ns) && isIdentity(name)) parts.push(ns, name)
  return parts.map(encodeURIComponent).join('/')
}

export const storageRoute = (page = 'overview') => 'storage/' + (isStoragePage(page) ? page : 'overview')
export const routePath = route => route.module === 'storage' ? storageRoute(route.page) : pulsarRoute(route.page, route.ns, route.name)

export function entityRoute(key) {
  const [kind, ns, name, extra] = key.split('/')
  if (extra !== undefined || !isIdentity(ns) || !isIdentity(name)) return pulsarRoute()
  if (kind === 'PulsarCluster') return pulsarRoute('brokers', ns, name)
  if (kind === 'BookKeeperCluster') return pulsarRoute('bookies', ns, name)
  return pulsarRoute()
}
