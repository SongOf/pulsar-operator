// Normalises a PulsarCluster (with its child BookKeeperCluster) or a standalone BookKeeperCluster into one view
// model, so the console renders and changes both the same way. Paths point into the resource's spec.

export function nest(path, value) {
  return path.reduceRight((acc, key) => ({ [key]: acc }), value)
}

export function get(obj, path) {
  return path.reduce((o, k) => (o == null ? undefined : o[k]), obj)
}

export function pulsarView(pc, child) {
  const s = pc.spec, st = pc.status || {}, cs = child?.status || {}
  const components = [
    { key: 'zookeeper', label: 'ZooKeeper', status: st.zookeeper, desired: s.zookeeper.replicas ?? 3, restart: 'zookeeper', configPath: ['zookeeper', 'config'], file: 'zookeeper.conf', imagePath: ['zookeeper', 'image'] },
    { key: 'bookie', label: 'Bookies', status: st.bookkeeper, desired: s.bookkeeper.replicas, scalePath: ['bookkeeper', 'replicas'], min: s.bookkeeper.minReplicas ?? 3, restart: 'bookie', configPath: ['bookkeeper', 'config'], file: 'bookkeeper.conf', imagePath: ['bookkeeper', 'image'] },
    { key: 'autorecovery', label: 'Auto-recovery', status: cs.autoRecovery, desired: s.bookkeeper.autoRecovery?.enabled === false ? 0 : (s.bookkeeper.autoRecovery?.replicas ?? 1), restart: 'autorecovery' },
    { key: 'broker', label: 'Brokers', status: st.broker, desired: s.broker.replicas, scalePath: ['broker', 'replicas'], restart: 'broker', configPath: ['broker', 'config'], file: 'broker.conf', imagePath: ['broker', 'image'] },
  ]
  if (s.proxy?.enabled) {
    components.push({ key: 'proxy', label: 'Proxy', status: st.proxy, desired: s.proxy.replicas ?? 1, scalePath: ['proxy', 'replicas'], restart: 'proxy', configPath: ['proxy', 'config'], file: 'proxy.conf', imagePath: ['proxy', 'image'] })
  }
  return {
    kind: 'PulsarCluster', obj: pc, ns: pc.metadata.namespace, name: pc.metadata.name,
    phase: st.phase, image: s.image, imagePath: ['image'],
    components,
    bookies: cs.bookieDetails || [], decommission: cs.decommission,
    storage: s.bookkeeper.storage, storagePath: ['bookkeeper', 'storage'], expansionSupported: cs.expansionSupported,
    proxy: s.proxy || {}, serviceURL: st.serviceURL, webServiceURL: st.webServiceURL,
    conditions: [...(st.conditions || []).map(c => ({ ...c, source: 'Pulsar' })), ...(cs.conditions || []).map(c => ({ ...c, source: 'BookKeeper' }))],
    paused: s.paused,
  }
}

export function bookkeeperView(bk) {
  const s = bk.spec, st = bk.status || {}
  return {
    kind: 'BookKeeperCluster', obj: bk, ns: bk.metadata.namespace, name: bk.metadata.name,
    phase: st.phase, image: s.image, imagePath: ['image'],
    components: [
      { key: 'bookie', label: 'Bookies', status: st.bookies, desired: s.replicas, scalePath: ['replicas'], min: s.minReplicas ?? 3, restart: 'bookie', configPath: ['config'], file: 'bookkeeper.conf', imagePath: ['image'] },
      { key: 'autorecovery', label: 'Auto-recovery', status: st.autoRecovery, desired: s.autoRecovery?.enabled === false ? 0 : (s.autoRecovery?.replicas ?? 1), restart: 'autorecovery' },
    ],
    bookies: st.bookieDetails || [], decommission: st.decommission,
    storage: s.storage, storagePath: ['storage'], expansionSupported: st.expansionSupported,
    conditions: (st.conditions || []).map(c => ({ ...c, source: 'BookKeeper' })),
    zookeeper: s.zookeeper,
    // A BookKeeperCluster owned by a PulsarCluster is monitored and managed through it.
    managed: (bk.metadata.ownerReferences || []).some(r => r.kind === 'PulsarCluster' && r.controller),
    paused: s.paused,
  }
}

export const DEFAULT_IMAGE = 'apachepulsar/pulsar:4.0.14'

// upgradePlan lists the components in the order the operator upgrades them, each with the image it should run,
// the image it runs and where it stands: current, rolling, or waiting for the component before it.
export function upgradePlan(view) {
  const clusterImage = view.image || DEFAULT_IMAGE
  const order = ['zookeeper', 'bookie', 'broker', 'proxy']
  const steps = order.map(k => view.components.find(c => c.key === k)).filter(c => c && c.imagePath)
  let blockedBy = null
  return steps.map(c => {
    const override = view.kind === 'PulsarCluster' ? get(view.obj.spec, c.imagePath) : undefined
    const desired = override || clusterImage
    const st = c.status || {}
    let state, detail
    if (!st.image) {
      state = 'pending'; detail = 'not created yet'
    } else if (st.image !== desired) {
      state = 'waiting'; detail = blockedBy ? `waits until ${blockedBy} finished` : 'about to roll'
    } else if (!st.rolledOut) {
      state = 'rolling'; detail = `${st.updatedReplicas ?? 0}/${st.replicas ?? 0} updated, ${st.readyReplicas ?? 0} ready`
    } else {
      state = 'current'; detail = 'up to date'
    }
    if (state !== 'current' && !blockedBy) blockedBy = c.label
    return { ...c, override, desired, running: st.image, state, detail }
  })
}

// bookieRemoval decides whether one more bookie may be decommissioned now: one at a time, from a healthy
// cluster, never below the minimum. It returns the bookie that would go, or why none can.
export function bookieRemoval(view, c) {
  const st = c.status || {}
  const running = st.replicas ?? c.desired
  const ordinal = c.desired - 1
  const target = view.bookies.find(b => b.name.endsWith(`-${ordinal}`))
  let blocked = null
  if (view.decommission || c.desired !== running) blocked = 'a bookie is being removed; wait until it finished'
  else if (c.desired <= c.min) blocked = `already at the minimum of ${c.min} bookies`
  else if ((st.readyReplicas ?? 0) < running || !st.rolledOut) blocked = 'every bookie must be ready before one is removed'
  return { ordinal, target, blocked, busy: !!view.decommission || c.desired !== running }
}

// Conditions worth a person's attention: anything not in its healthy state.
export function problems(conditions) {
  const healthy = { Ready: 'True', MetadataInitialized: 'True', StorageResized: 'True', ScaleInBlocked: 'False', SpecRejected: 'False' }
  return conditions.filter(c => healthy[c.type] !== undefined && c.status !== healthy[c.type] && !(c.type === 'Ready' && c.reason === 'AllComponentsReady'))
}

export function newPulsarCluster(f) {
  const vol = (size, sc) => (sc ? { size, storageClassName: sc } : { size })
  return {
    apiVersion: 'pulsar.songof.dev/v1alpha1', kind: 'PulsarCluster',
    metadata: { name: f.name, namespace: f.namespace },
    spec: {
      image: f.image,
      zookeeper: { replicas: +f.zkReplicas, storage: vol(f.zkSize, f.storageClass) },
      bookkeeper: {
        replicas: +f.bookies, minReplicas: +f.minBookies,
        storage: { journal: vol(f.journal, f.storageClass), ledger: vol(f.ledger, f.storageClass), reclaimPolicy: f.reclaim },
      },
      broker: { replicas: +f.brokers },
      proxy: { enabled: f.proxy, replicas: 1, serviceType: f.proxyServiceType },
    },
  }
}
