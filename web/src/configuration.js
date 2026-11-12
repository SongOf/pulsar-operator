// Editing is based on the snapshot the user saw, never on newer polled props.
export function configurationBaseline(config, identity) {
  return { config: { ...config }, identity: { uid: identity.uid, resourceVersion: identity.resourceVersion } }
}

export function configurationRows(config) {
  const rows = Object.entries(config).sort(([a], [b]) => a.localeCompare(b))
  return rows.length ? rows : [['', '']]
}

export function configurationPatch(baseline, rows) {
  const next = Object.fromEntries(rows.filter(([key]) => key.trim()).map(([key, value]) => [key.trim(), value]))
  return Object.fromEntries([
    ...Object.entries(next).filter(([key, value]) => !Object.hasOwn(baseline, key) || baseline[key] !== value),
    ...Object.keys(baseline).filter(key => !Object.hasOwn(next, key)).map(key => [key, null]),
  ])
}
