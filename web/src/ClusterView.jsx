import React, { useEffect, useState } from 'react'
import { api } from './api.js'
import { bookieRemoval, get, nest, problems, upgradePlan } from './model.js'
import { ActionStatus, PhaseBadge, Ready, Section, useAction } from './ui.jsx'
import { ConfigEditor } from './ConfigEditor.jsx'
import { Monitoring } from './Monitoring.jsx'
export function ClusterView({ view, onChanged, onDeleted }) {
  const patch = (spec, expected = view.obj.metadata) => api.patch(view.kind, view.ns, view.name, spec, expected)
  const issues = problems(view.conditions)
  const [tab, setTab] = useState('overview')
  const tabs = [
    ['overview', '组件状态'], ['monitoring', '监控'],
    ...(!view.hideBookieStorage ? [['storage', 'Journal / Ledger']] : []),
    ['configuration', '配置'], ['upgrade', '版本更新'], ['activity', 'Pod 与事件'],
    ...(!view.deleteDisabled && !view.readOnly ? [['lifecycle', view.kind === 'PulsarCluster' ? 'Pulsar 集群操作' : '集群操作']] : []),
  ]
  const tabAvailable = tabs.some(([key]) => key === tab)
  useEffect(() => { if (!tabAvailable) setTab('overview') }, [tabAvailable])
  return (
    <div className="cluster">
      <header className="cluster-head">
        <div>
          <h1>{view.displayName || view.name}</h1>
          <div className="muted">{view.scope === 'bookie' ? 'BookKeeperCluster' : view.kind} · namespace {view.readTarget?.ns || view.ns}{view.paused && ' · reconciliation paused'}</div>
        </div>
        <PhaseBadge phase={view.phase} />
      </header>
      {issues.map(c => (
        <div key={c.source + c.type} className={'banner ' + (c.type === 'Ready' ? 'info' : 'warn')}>
          <b>{c.source} {c.type}: {c.reason}</b>{c.message && <> — {c.message}</>}
        </div>
      ))}
      {view.decommission && <Decommission d={view.decommission} />}
      {tab === 'overview' && view.serviceURL && (
        <div className="endpoints">
          <div><span className="muted">Service URL</span><code>{view.serviceURL}</code></div>
          <div><span className="muted">Admin URL</span><code>{view.webServiceURL}</code></div>
        </div>
      )}
      <nav className="component-tabs" aria-label="组件详情">{tabs.map(([key, title]) => <button key={key} className={key === tab ? 'active' : ''} aria-current={key === tab ? 'page' : undefined} onClick={() => setTab(key)}>{title}</button>)}</nav>
      {view.readOnly && <div className="banner warn read-only-message">此资源的所有权尚未确认，当前只读。核实父资源身份后再执行管理操作。</div>}
      <div className="component-readonly"><fieldset disabled={view.readOnly}><div className="component-panels">
        {tab === 'overview' && <><Components view={view} patch={patch} onChanged={onChanged} /><Bookies view={view} /></>}
        {tab === 'monitoring' && <Monitoring view={view} patch={patch} onChanged={onChanged} />}
        {tab === 'storage' && !view.hideBookieStorage && <><Storage view={view} patch={patch} onChanged={onChanged} /><Bookies view={view} /></>}
        {tab === 'configuration' && <Configuration view={view} patch={patch} onChanged={onChanged} />}
        {tab === 'upgrade' && <Upgrade view={view} patch={patch} onChanged={onChanged} />}
        {tab === 'activity' && <><Pods view={view} /><Events view={view} /></>}
        {tab === 'lifecycle' && !view.deleteDisabled && !view.readOnly && <Danger view={view} onDeleted={onDeleted} />}
      </div></fieldset></div>
    </div>
  )
}
function Decommission({ d }) {
  const steps = ['Stopping', 'Replicating', 'Cleaning']
  return (
    <div className={'banner ' + (d.phase === 'Failed' ? 'error' : 'warn')}>
      <b>Decommissioning bookie {d.ordinal}</b> <code>{d.bookieID}</code>
      <div className="steps">
        {steps.map(s => <span key={s} className={'step' + (s === d.phase ? ' current' : '')}>{s}</span>)}
        {d.phase === 'Failed' && <span className="step current">Failed</span>}
      </div>
      <div className="small">{d.message} · started {new Date(d.startedAt).toLocaleString()}</div>
    </div>
  )
}
function Components({ view, patch, onChanged }) {
  return (
    <Section title="Components">
      <table>
        <thead><tr><th>Component</th><th>Ready</th><th>Image</th><th>Scale</th><th></th></tr></thead>
        <tbody>{view.components.map(c => <ComponentRow key={c.key} c={c} view={view} patch={patch} onChanged={onChanged} />)}</tbody>
      </table>
      {view.kind === 'PulsarCluster' && view.scope !== 'bookie' && <ProxyToggle view={view} patch={patch} onChanged={onChanged} />}
    </Section>
  )
}
function ComponentRow({ c, view, patch, onChanged }) {
  const [n, setN] = useState(c.desired)
  const [state, run] = useAction(onChanged)
  useEffect(() => setN(c.desired), [c.desired])
  const st = c.status || {}
  return (
    <tr>
      <td><b>{c.label}</b>{c.key === 'zookeeper' && <div className="muted small">ensemble size is fixed</div>}</td>
      <td><Ready ready={st.readyReplicas} total={st.replicas ?? c.desired} />{st.replicas > 0 && !st.rolledOut && <span className="muted small"> rolling</span>}</td>
      <td className="mono small">{st.image || '—'}</td>
      <td>
        {c.key === 'bookie' ? <BookieScale c={c} view={view} patch={patch} run={run} state={state} /> : c.scalePath ? (
          <form className="inline" onSubmit={e => { e.preventDefault(); run(`Scaled to ${n}`, () => patch(nest(c.scalePath, +n))) }}>
            <input type="number" min={0} value={n} onChange={e => setN(e.target.value)} className="num" />
            <button disabled={state.busy || +n === c.desired}>Apply</button>
            {c.min !== undefined && <span className="muted small">min {c.min}</span>}
          </form>
        ) : <span className="muted">{c.desired}</span>}
        {c.key !== 'bookie' && <ActionStatus state={state} />}
      </td>
      <td>
        {c.restart && (
          <button className="ghost" disabled={state.busy} onClick={() => {
            if (confirm(`Rolling restart of ${c.label}?`)) run('Restart requested', () => api.restart(view.kind, view.ns, view.name, c.restart, view.obj.metadata))
          }}>Restart</button>
        )}
      </td>
    </tr>
  )
}
// BookieScale adds bookies freely but removes them only one per action: each removal is a full decommission
// (stop, re-replicate its ledgers, delete its PVCs), and the next can start only after it finished.
function BookieScale({ c, view, patch, run, state }) {
  const [n, setN] = useState(c.desired)
  useEffect(() => setN(c.desired), [c.desired])
  const { ordinal: next, target, blocked, busy } = bookieRemoval(view, c)
  const removeOne = () => {
    const msg = `Decommission bookie ${next}${target ? ` (${target.bookieID})` : ''}?\n\n` +
      '1. The bookie stops.\n2. Its ledgers are re-replicated to the other bookies.\n3. Its journal and ledger PVCs are deleted.\n\n' +
      'This takes from seconds to hours, depending on how much data it holds.'
    if (confirm(msg)) run(`Decommissioning bookie ${next}`, () => patch(nest(c.scalePath, next)))
  }
  return (
    <div className="bookie-scale">
      <form className="inline" onSubmit={e => { e.preventDefault(); run(`Scaling out to ${n}`, () => patch(nest(c.scalePath, +n))) }}>
        <input type="number" min={c.desired} value={n} onChange={e => setN(e.target.value)} className="num" />
        <button disabled={state.busy || busy || +n <= c.desired}>Scale out</button>
      </form>
      <div className="inline">
        <button className="warn-button" disabled={state.busy || !!blocked} onClick={removeOne}>Decommission one bookie</button>
        <span className="muted small">min {c.min}</span>
      </div>
      {blocked && <div className="muted small">{blocked}</div>}
      <ActionStatus state={state} />
    </div>
  )
}
function ProxyToggle({ view, patch, onChanged }) {
  const [state, run] = useAction(onChanged)
  const [type, setType] = useState(view.proxy.serviceType || 'ClusterIP')
  const enabled = !!view.proxy.enabled
  return (
    <div className="row">
      <span>Proxy is <b>{enabled ? 'enabled' : 'disabled'}</b></span>
      <select value={type} onChange={e => setType(e.target.value)}>
        <option>ClusterIP</option><option>NodePort</option><option>LoadBalancer</option>
      </select>
      <button onClick={() => run(enabled ? 'Proxy disabled' : 'Proxy enabled', () => patch({ proxy: { enabled: !enabled, serviceType: type } }))}>
        {enabled ? 'Disable proxy' : 'Enable proxy'}
      </button>
      {enabled && type !== (view.proxy.serviceType || 'ClusterIP') && (
        <button onClick={() => run('Service type changed', () => patch({ proxy: { serviceType: type } }))}>Change service type</button>
      )}
      <ActionStatus state={state} />
    </div>
  )
}
function Bookies({ view }) {
  if (!view.bookies.length) return null
  const vol = v => v?.capacity ? `${v.capacity}${v.requested && v.requested !== v.capacity ? ` → ${v.requested}` : ''}` : (v?.requested || '—')
  return (
    <Section title="Bookies and disks">
      <table>
        <thead><tr><th>Bookie</th><th>Ready</th><th>Journal PVC</th><th>Ledger PVC</th></tr></thead>
        <tbody>
          {view.bookies.map(b => (
            <tr key={b.name}>
              <td><b>{b.name}</b><div className="mono small muted">{b.bookieID}</div></td>
              <td>{b.ready ? <span className="ok-text">Ready</span> : <span className="warn-text">Not ready</span>}</td>
              <td><div className="mono small">{b.journal?.name}</div>{vol(b.journal)} <span className="muted small">{b.journal?.phase}{b.journal?.resizing && ' · resizing'}</span></td>
              <td><div className="mono small">{b.ledger?.name}</div>{vol(b.ledger)} <span className="muted small">{b.ledger?.phase}{b.ledger?.resizing && ' · resizing'}</span></td>
            </tr>
          ))}
        </tbody>
      </table>
    </Section>
  )
}
function Storage({ view, patch, onChanged }) {
  const s = view.storage || {}
  const [journal, setJournal] = useState(s.journal?.size || '')
  const [ledger, setLedger] = useState(s.ledger?.size || '')
  const [state, run] = useAction(onChanged)
  useEffect(() => { setJournal(s.journal?.size || ''); setLedger(s.ledger?.size || '') }, [s.journal?.size, s.ledger?.size])
  const changed = journal !== s.journal?.size || ledger !== s.ledger?.size
  return (
    <Section title="Bookie storage (PV / PVC)">
      <p className="muted small">
        Each bookie has one journal and one ledger PVC. Sizes can only grow.{' '}
        {view.expansionSupported === false && <b className="warn-text">The StorageClass does not allow expansion: existing PVCs keep their size, new bookies get the new one.</b>}
        {view.expansionSupported === true && <span className="ok-text">The StorageClass allows online expansion.</span>}
      </p>
      <form className="grid3" onSubmit={e => {
        e.preventDefault()
        run('Storage updated', () => patch(nest(view.storagePath, { journal: { size: journal }, ledger: { size: ledger } })))
      }}>
        <label>Journal size<input value={journal} onChange={e => setJournal(e.target.value)} /><span className="muted small">class {s.journal?.storageClassName || 'default'}</span></label>
        <label>Ledger size<input value={ledger} onChange={e => setLedger(e.target.value)} /><span className="muted small">class {s.ledger?.storageClassName || 'default'}</span></label>
        <label>Reclaim on delete<select value={s.reclaimPolicy || 'Retain'} onChange={e => run('Reclaim policy updated', () => patch(nest(view.storagePath, { reclaimPolicy: e.target.value })))}>
          <option>Retain</option><option>Delete</option></select></label>
        <div><button className="primary" disabled={!changed || state.busy}>Resize</button> <ActionStatus state={state} /></div>
      </form>
    </Section>
  )
}
function Configuration({ view, patch, onChanged }) {
  const editable = view.components.filter(c => c.configPath)
  const [tab, setTab] = useState(editable[0]?.key)
  const c = editable.find(x => x.key === tab) || editable[0]
  if (!c) return null
  return (
    <Section title="Configuration" actions={
      <div className="tabs">{editable.map(x => <button key={x.key} className={x.key === c.key ? 'tab active' : 'tab'} onClick={() => setTab(x.key)}>{x.label}</button>)}</div>
    }>
      <ConfigEditor key={c.key} file={c.file} config={get(view.obj.spec, c.configPath) || {}} identity={view.obj.metadata}
        onSave={async (next, expected) => {
          const updated = await patch(nest(c.configPath, next), expected)
          return { config: get(updated.spec, c.configPath) || {}, identity: updated.metadata }
        }} onChanged={onChanged} />
    </Section>
  )
}
// Upgrade changes the cluster image, or one component's image, and shows the rollout in the order the operator
// performs it.
function Upgrade({ view, patch, onChanged }) {
  const [image, setImage] = useState(view.image || '')
  const [state, run] = useAction(onChanged)
  useEffect(() => setImage(view.image || ''), [view.image])
  const plan = upgradePlan(view)
  const pulsar = view.kind === 'PulsarCluster'
  const scopedBookie = view.scope === 'bookie'
  return (
    <Section title="Version and upgrade">
      {view.scope !== 'broker' && <form className="inline" onSubmit={e => {
        e.preventDefault()
        const scope = scopedBookie || !pulsar ? 'the bookies and auto-recovery' : 'every component without its own image (in order: ZooKeeper, bookies, brokers, proxy)'
        if (confirm(`Upgrade ${scope} to ${image}?`)) run('Upgrade started', () => patch(nest(view.imagePath, image)))
      }}>
        <label className="grow">{scopedBookie ? 'Bookie image' : pulsar ? 'Cluster image' : 'Image'}<input className="mono" value={image} onChange={e => setImage(e.target.value)} /></label>
        <button className="primary" disabled={!image || image === view.image || state.busy}>Upgrade {scopedBookie ? 'Bookie' : pulsar ? 'cluster' : ''}</button>
        <ActionStatus state={state} />
      </form>}
      <table className="upgrade">
        <thead><tr><th>#</th><th>Component</th><th>Runs</th><th>Target</th><th>State</th>{pulsar && <th>Upgrade this component only</th>}</tr></thead>
        <tbody>{plan.map((c, i) => <UpgradeRow key={c.key} i={i + 1} c={c} pulsar={pulsar} patch={patch} onChanged={onChanged} />)}</tbody>
      </table>
      {pulsar && <p className="muted small">组件镜像可以分别设置。控制器仍按 ZooKeeper → Bookie → Broker → Proxy 的顺序协调更新；当前页面只显示本组组件，其他组件未就绪时可能需要等待。Auto-recovery 跟随 Bookie 镜像。</p>}
    </Section>
  )
}
const stateLabel = { current: 'Up to date', rolling: 'Rolling', waiting: 'Waiting', pending: 'Pending' }
function UpgradeRow({ i, c, pulsar, patch, onChanged }) {
  const [image, setImage] = useState(c.override || '')
  const [state, run] = useAction(onChanged)
  useEffect(() => setImage(c.override || ''), [c.override])
  const tone = { current: 'ok', rolling: 'info', waiting: 'warn', pending: 'muted' }[c.state]
  return (
    <tr>
      <td className="muted">{i}</td>
      <td><b>{c.label}</b>{c.override && <div className="muted small">own image</div>}</td>
      <td className="mono small">{c.running || '—'}</td>
      <td className="mono small">{c.desired}</td>
      <td><span className={'badge ' + tone}>{stateLabel[c.state]}</span><div className="muted small">{c.detail}</div></td>
      {pulsar && (
        <td>
          <form className="inline" onSubmit={e => {
            e.preventDefault()
            if (confirm(`Upgrade only ${c.label} to ${image}?`)) run(`${c.label} upgrade started`, () => patch(nest(c.imagePath, image)))
          }}>
            <input className="mono" placeholder="image for this component" value={image} onChange={e => setImage(e.target.value)} />
            <button disabled={!image || image === c.override || state.busy}>Upgrade</button>
            {c.override && <button type="button" className="ghost" disabled={state.busy} onClick={() => {
              if (confirm(`${c.label} goes back to the cluster image. Continue?`)) run('Follows the cluster image', () => patch(nest(c.imagePath, null)))
            }}>Use cluster image</button>}
          </form>
          <ActionStatus state={state} />
        </td>
      )}
    </tr>
  )
}
function usePolled(fn, deps) {
  const [rows, setRows] = useState([])
  useEffect(() => {
    let live = true
    const load = () => fn().then(r => live && setRows(r || [])).catch(() => {})
    load()
    const t = setInterval(load, 5000)
    return () => { live = false; clearInterval(t) }
  }, deps)
  return rows
}
function Pods({ view }) {
  const target = view.readTarget || view
  const records = usePolled(() => api.pods(target.kind, target.ns, target.name), [target.kind, target.ns, target.name])
  const pods = view.scope ? records.filter(p => view.components.some(c => c.key === p.component)) : records
  return (
    <Section title={`Pods (${pods.length})`}>
      <table>
        <thead><tr><th>Pod</th><th>Component</th><th>Status</th><th>Restarts</th><th>Node</th></tr></thead>
        <tbody>
          {pods.map(p => (
            <tr key={p.name}>
              <td className="mono small">{p.name}</td><td>{p.component}</td>
              <td>{p.ready ? <span className="ok-text">Ready</span> : <span className="warn-text">{p.phase}</span>}</td>
              <td className={p.restarts > 0 ? 'warn-text' : ''}>{p.restarts}</td>
              <td className="small">{p.node}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Section>
  )
}
function Events({ view }) {
  const target = view.readTarget || view
  const events = usePolled(() => api.events(target.kind, target.ns, target.name), [target.kind, target.ns, target.name])
  if (!events.length) return null
  return (
    <Section title="Events">
      <ul className="events">
        {events.map((e, i) => (
          <li key={i} className={e.type === 'Warning' ? 'warn-text' : ''}>
            <span className="muted small">{new Date(e.time).toLocaleString()}</span> <b>{e.reason}</b> {e.message}{e.count > 1 && <span className="muted"> ×{e.count}</span>}
          </li>
        ))}
      </ul>
    </Section>
  )
}
function Danger({ view, onDeleted }) {
  const [confirmName, setConfirmName] = useState('')
  const [state, run] = useAction(onDeleted)
  const reclaim = (view.kind === 'PulsarCluster' ? view.obj.spec.bookkeeper?.storage?.reclaimPolicy : view.storage?.reclaimPolicy) || 'Retain'
  return (
    <Section title={view.kind === 'PulsarCluster' ? '删除整个 Pulsar 集群' : 'Delete BookKeeper cluster'}>
      {view.kind === 'PulsarCluster' && <p className="warn-text">此操作删除 Pulsar 集群的 ZooKeeper、BookKeeper、Broker 和 Proxy，不是仅删除 Broker 组件。</p>}
      <p className="muted small">Deletes every workload of the cluster. Bookie PVCs are {reclaim === 'Delete' ? <b>deleted with their data</b> : 'kept (reclaim policy Retain)'}.</p>
      <form className="inline" onSubmit={e => { e.preventDefault(); run('Deleted', () => api.remove(view.kind, view.ns, view.name, view.obj.metadata)) }}>
        <input placeholder={`type ${view.name} to confirm`} value={confirmName} onChange={e => setConfirmName(e.target.value)} />
        <button className="danger" disabled={confirmName !== view.name || state.busy}>Delete</button>
        <ActionStatus state={state} />
      </form>
    </Section>
  )
}
