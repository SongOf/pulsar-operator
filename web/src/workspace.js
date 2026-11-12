import { bookkeeperView, pulsarView } from './model.js'

const GROUP = 'pulsar.songof.dev'
const BROKER_COMPONENTS = new Set(['broker', 'zookeeper', 'proxy'])
const BOOKIE_COMPONENTS = new Set(['bookie', 'autorecovery'])

function key(kind, object) {
  return `${kind}/${object.metadata.namespace}/${object.metadata.name}`
}

function target(kind, object) {
  return { kind, ns: object.metadata.namespace, name: object.metadata.name }
}

function pulsarReference(ref) {
  const version = typeof ref.apiVersion === 'string' ? ref.apiVersion.split('/') : []
  return ref.kind === 'PulsarCluster' && version.length === 2 && version[0] === GROUP && version[1] !== '' &&
    ref.controller === true && typeof ref.uid === 'string' && ref.uid !== ''
}

function parentOf(bookkeeper, parents) {
  const owners = bookkeeper.metadata.ownerReferences || []
  if (owners.filter(ref => ref.controller === true).length > 1) return null
  for (const ref of owners) {
    if (!pulsarReference(ref)) continue
    const parent = parents.get(`PulsarCluster/${bookkeeper.metadata.namespace}/${ref.name}`)
    // ChildBookKeeper in internal/resources/pulsar.go reconciles only the child
    // sharing the parent's name. A differently named owned object cannot be
    // edited through that parent because those writes would affect another BK.
    if (parent && bookkeeper.metadata.name === parent.metadata.name &&
      parent.metadata.uid && parent.metadata.uid === ref.uid) return parent
  }
  return null
}

function hasOwner(bookkeeper) {
  // An invalid or unresolved Pulsar owner is still ownership evidence. Showing
  // that object as a writable standalone cluster could bypass its controller.
  return (bookkeeper.metadata.ownerReferences || []).some(ref =>
    ref.kind === 'PulsarCluster' || ref.controller === true)
}

function bookkeepers(data) {
  // Older servers expose only the standalone list and a parent-keyed map.
  // Treat map keys as transport details: ownership is verified on each object.
  const objects = Array.isArray(data.allBookkeeper)
    ? data.allBookkeeper
    : [...Object.values(data.children || {}), ...(data.bookkeeper || [])]
  const unique = new Map()
  for (const object of objects) {
    const objectKey = key('BookKeeperCluster', object)
    if (!unique.has(objectKey)) unique.set(objectKey, object)
  }
  return [...unique.values()].sort((a, b) => key('BookKeeperCluster', a).localeCompare(key('BookKeeperCluster', b)))
}

function relation(parent, bookkeeper, state) {
  const claimed = bookkeeper?.metadata.ownerReferences?.find(ref => ref.kind === 'PulsarCluster')
  return {
    state,
    pulsarKey: parent ? key('PulsarCluster', parent) : null,
    pulsarName: parent?.metadata.name || (state === 'orphan' ? claimed?.name || null : null),
    bookkeeperKey: bookkeeper ? key('BookKeeperCluster', bookkeeper) : null,
    bookkeeperName: bookkeeper?.metadata.name || null,
  }
}

function brokerView(parent, child, link) {
  const view = pulsarView(parent, child)
  return {
    ...view,
    key: key('PulsarCluster', parent),
    scope: 'broker', displayName: parent.metadata.name,
    relation: link, readTarget: target('PulsarCluster', parent),
    readOnly: false, deleteDisabled: false, hideBookieStorage: true,
    components: view.components.filter(component => BROKER_COMPONENTS.has(component.key)),
    conditions: view.conditions.filter(condition => condition.source === 'Pulsar'),
    bookies: [], decommission: null,
  }
}

function managedBookieView(parent, child, link) {
  const view = pulsarView(parent, child)
  return {
    ...view,
    key: key('BookKeeperCluster', child),
    scope: 'bookie', displayName: child.metadata.name,
    relation: link, readTarget: target('BookKeeperCluster', child),
    readOnly: false, managed: true, deleteDisabled: true, hideBookieStorage: false,
    phase: child.status?.phase,
    // Writes still target the parent resource, including component image changes.
    image: parent.spec.bookkeeper.image || view.image,
    imagePath: ['bookkeeper', 'image'],
    components: view.components.filter(component => BOOKIE_COMPONENTS.has(component.key)).map(component =>
      component.key === 'bookie' ? { ...component, status: child.status?.bookies || component.status } : component),
    conditions: view.conditions.filter(condition => condition.source === 'BookKeeper'),
    serviceURL: undefined, webServiceURL: undefined, proxy: undefined,
    zookeeper: child.spec.zookeeper,
  }
}

function independentBookieView(child, link) {
  const orphan = link.state === 'orphan'
  return {
    ...bookkeeperView(child),
    key: key('BookKeeperCluster', child),
    scope: 'bookie', displayName: child.metadata.name,
    relation: link, readTarget: target('BookKeeperCluster', child),
    readOnly: orphan, managed: orphan, deleteDisabled: orphan, hideBookieStorage: false,
  }
}

// Separate the two management scopes without changing the original view models.
// Entity keys identify what a page displays; kind/ns/name/obj identify what it
// writes. Those are deliberately different for a managed BookKeeperCluster.
export function inventory(data = {}) {
  data = data || {}
  const parents = new Map((data.pulsar || []).map(parent => [key('PulsarCluster', parent), parent]))
  const children = new Map()
  const relationships = []
  const bookies = bookkeepers(data).map(child => {
    const parent = parentOf(child, parents)
    const state = parent ? 'linked' : hasOwner(child) ? 'orphan' : 'standalone'
    const link = relation(parent, child, state)
    relationships.push({ ...link, namespace: child.metadata.namespace })
    if (parent) {
      children.set(link.pulsarKey, child)
      return managedBookieView(parent, child, link)
    }
    return independentBookieView(child, link)
  })
  const brokers = [...parents.values()].map(parent => {
    const child = children.get(key('PulsarCluster', parent))
    const link = relation(parent, child, child ? 'linked' : 'pending')
    if (!child) relationships.push({ ...link, namespace: parent.metadata.namespace })
    return brokerView(parent, child, link)
  }).sort((a, b) => a.key.localeCompare(b.key))
  relationships.sort((a, b) =>
    `${a.namespace}/${a.pulsarName || ''}/${a.bookkeeperName || ''}`.localeCompare(`${b.namespace}/${b.pulsarName || ''}/${b.bookkeeperName || ''}`))
  return { brokers, bookies, relationships }
}
