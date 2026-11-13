import React, { useCallback, useEffect, useRef, useState } from 'react'
import { api, ApiError, setToken } from './api.js'
import { inventory } from './workspace.js'
import { ClusterView } from './ClusterView.jsx'
import { CreateDialog } from './CreateDialog.jsx'
import { CreateBookKeeperDialog } from './CreateBookKeeperDialog.jsx'
import { PULSAR_PAGES, entityRoute, pulsarRoute, storageRoute } from './routes.js'
import { PhaseBadge, Ready } from './ui.jsx'
import { TokenForm } from './PulsarTokenForm.jsx'
import { PulsarAudit } from './PulsarAudit.jsx'

const names = Object.fromEntries(PULSAR_PAGES.map(page => [page.key, page.label]))

// Mounted only inside the Pulsar module; switching to Storage stops polling and
// discards late responses without affecting the storage connection or token.
export function PulsarWorkspace(props) {
  return props.route.page === 'audit' ? <PulsarAudit /> : <ClusterWorkspace {...props} />
}

function ClusterWorkspace({ route, onNavigate }) {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [needToken, setNeedToken] = useState(false)
  const [credentials, setCredentials] = useState(false)
  const [creating, setCreating] = useState(null)
  const [updated, setUpdated] = useState(null)
  const [busy, setBusy] = useState(false)
  const requestVersion = useRef(0)
  const pending = useRef(false)
  const refresh = useCallback(async (force = true) => {
    if (pending.current && force === false) return
    const version = ++requestVersion.current
    pending.current = true
    setBusy(true)
    try {
      const next = await api.list()
      if (version !== requestVersion.current) return
      setData(next); setError(null); setNeedToken(false); setUpdated(new Date())
    } catch (e) {
      if (version !== requestVersion.current) return
      if (e instanceof ApiError && e.status === 401) { setNeedToken(true); setData(null); setError(null) }
      else setError(e.message)
    } finally {
      if (version === requestVersion.current) { pending.current = false; setBusy(false) }
    }
  }, [])
  useEffect(() => {
    refresh()
    const timer = setInterval(() => refresh(false), 5000)
    return () => {
      requestVersion.current++; pending.current = false
      clearInterval(timer)
    }
  }, [refresh])
  const navigate = page => onNavigate(pulsarRoute(page))
  const openEntity = key => onNavigate(entityRoute(key))
  const catalog = inventory(data || {})
  const rows = route.page === 'brokers' ? catalog.brokers : catalog.bookies
  const current = route.name ? rows.find(v => v.readTarget.ns === route.ns && v.displayName === route.name) : null
  const created = key => { setCreating(null); openEntity(key); refresh() }

  return <>
    <div className="workspace-toolbar" aria-label="Pulsar 操作">
      <span className="workspace-service">Pulsar 集群管理</span>
      <div className="inline"><span className="sync-note">{updated ? '更新于 ' + updated.toLocaleTimeString() : '等待连接'}</span><button onClick={() => setCredentials(true)}>Pulsar 凭证</button><button onClick={() => refresh()} disabled={busy}>{busy ? '同步中…' : '刷新'}</button></div>
    </div>
    {error && <div className="banner error" role="alert">无法同步集群：{error}{data && '。下方为上次成功同步的数据。'}</div>}
    {needToken ? <TokenForm onSave={t => { setToken(t); refresh() }} /> : <>
      {!data && !error ? <div className="empty">正在读取 Kubernetes 集群…</div> : route.page === 'overview' ? <Overview catalog={catalog} openEntity={openEntity} navigate={navigate} onCreate={() => setCreating('pulsar')} onStorage={() => onNavigate(storageRoute())} /> : <>
        <div className="workspace-heading"><div><div className="eyebrow">{route.page === 'brokers' ? 'PULSAR / COMPUTE' : 'BOOKKEEPER / PERSISTENCE'}</div><h1>{names[route.page]}</h1><p>{route.page === 'brokers' ? '管理 Pulsar 的 Broker、接入与元数据组件，查看关联的 BookKeeper 集群。' : '独立查看每组 Bookie 的副本、磁盘与回收任务，以及使用它的 Pulsar 集群。'}</p></div><button className="primary" onClick={() => setCreating(route.page === 'brokers' ? 'pulsar' : 'bookkeeper')}>{route.page === 'brokers' ? '+ 新建 Pulsar 集群' : '+ 新建独立 BookKeeper'}</button></div>
        {route.name && <button className="link back-link" onClick={() => navigate(route.page)}>← 返回组件列表</button>}
        {current ? <>
          <Relationship view={current} openEntity={openEntity} />
          <ClusterView key={current.key + '/' + current.obj.metadata.uid} view={current} onChanged={refresh} onDeleted={() => { navigate(route.page); refresh() }} />
        </> : route.name ? <div className="card empty"><h2>未找到该组件</h2><p>资源可能已删除，或当前凭据无法读取。</p></div> : <ComponentList key={route.page} rows={rows} area={route.page} openEntity={openEntity} />}
      </>}
    </>}
    {creating === 'pulsar' && <CreateDialog onClose={() => setCreating(null)} onCreated={created} />}
    {creating === 'bookkeeper' && <CreateBookKeeperDialog onClose={() => setCreating(null)} onCreated={created} />}
    {credentials && <div className="overlay"><div className="card dialog token-dialog"><TokenForm onSave={t => { setToken(t); setCredentials(false); refresh() }} /><button onClick={() => setCredentials(false)}>取消</button></div></div>}
  </>
}


function Overview({ catalog, openEntity, navigate, onCreate, onStorage }) {
  const linked = catalog.brokers.filter(v => v.relation.state === 'linked').length
  return <>
    <div className="workspace-heading"><div><div className="eyebrow">PULSAR OVERVIEW</div><h1>Pulsar 总览</h1><p>分别管理 Broker 与 Bookie，沿集群关联追踪消息存储。</p></div><button className="primary" onClick={onCreate}>+ 新建 Pulsar 集群</button></div>
    <div className="workspace-stats">
      <Stat title="Broker 集群" value={catalog.brokers.length} note="Pulsar 计算与接入" onClick={() => navigate('brokers')} />
      <Stat title="Bookie 集群" value={catalog.bookies.length} note="BookKeeper 持久化" onClick={() => navigate('bookies')} />
      <Stat title="已确认的集群关联" value={linked} note="依据资源所有权识别" />
      <Stat title="物理存储" value="PV / PVC" note="磁盘 · 存储池 · 容量" onClick={onStorage} />
    </div>
    <section className="card relationship-panel"><header className="card-head"><div><h2>Pulsar 与 BookKeeper 关联</h2><p className="muted small">每行展示一个 Pulsar 集群正在使用的 Bookie 集群。</p></div><span className="badge muted">{catalog.brokers.length} 个 Pulsar 集群</span></header>
      {catalog.brokers.length ? <div className="table-scroll"><table><thead><tr><th>Pulsar / Broker</th><th>Namespace</th><th>BookKeeper / Bookie</th><th>关联状态</th><th>运行状态</th></tr></thead><tbody>{catalog.brokers.map(v => <tr key={v.key}><td><button className="link entity-link" onClick={() => openEntity(v.key)}>{v.displayName}</button></td><td>{v.ns}</td><td>{v.relation.bookkeeperKey ? <button className="link entity-link" onClick={() => openEntity(v.relation.bookkeeperKey)}>{v.relation.bookkeeperName}</button> : <span className="muted">等待 BookKeeper 创建或确认</span>}</td><td><RelationBadge state={v.relation.state} /></td><td><PhaseBadge phase={v.phase} /></td></tr>)}</tbody></table></div> : <div className="empty compact"><h3>尚无 Pulsar 集群</h3><p>创建集群后，在这里查看 Broker 与 Bookie 的关联。</p></div>}
    </section>
    <div className="workspace-columns"><section className="card"><header className="card-head"><h3>Broker 组件</h3><button className="link" onClick={() => navigate('brokers')}>查看全部 →</button></header><p className="muted">副本、配置、滚动更新与客户端接入。</p></section><section className="card"><header className="card-head"><h3>Bookie 组件</h3><button className="link" onClick={() => navigate('bookies')}>查看全部 →</button></header><p className="muted">Journal / Ledger、容量、扩容和安全退役。</p></section></div>
  </>
}
function Stat({ title, value, note, onClick }) {
  return <div className="workspace-stat"><div className="muted small">{title}</div><div className="stat-number">{value}</div><div className="stat-bottom"><span>{note}</span>{onClick && <button className="link" onClick={onClick} aria-label={'查看' + title}>→</button>}</div></div>
}
function RelationBadge({ state }) {
  const labels = { linked: '已关联', pending: '等待关联', orphan: '关联异常', standalone: '独立部署' }
  return <span className={'badge ' + ({ linked: 'ok', pending: 'info', orphan: 'warn', standalone: 'muted' }[state] || 'warn')}>{labels[state] || '待核验'}</span>
}
function Relationship({ view, openEntity }) {
  const relation = view.relation
  const otherKey = view.scope === 'broker' ? relation.bookkeeperKey : relation.pulsarKey
  const otherName = view.scope === 'broker' ? relation.bookkeeperName : relation.pulsarName
  return <div className="relation-strip"><div><span className="muted">{view.scope === 'broker' ? '使用的 BookKeeper 集群' : '使用此 BookKeeper 的 Pulsar 集群'}</span><div className="relation-name">{otherKey && relation.state === 'linked' ? <button className="link" onClick={() => openEntity(otherKey)}>{otherName} →</button> : otherName || (relation.state === 'standalone' ? '当前 Operator 未记录关联' : '尚未确认')}</div></div><RelationBadge state={relation.state} /><p>{relation.state === 'orphan' ? '父资源缺失或身份不匹配，已停用写入操作。' : view.scope === 'bookie' && relation.state === 'linked' ? '在此管理 Bookie，变更由关联 Pulsar 集群统一协调。' : relation.state === 'standalone' ? '独立集群；外部客户端使用情况不在当前控制器关联中。' : '可双向跳转查看组件状态。'}</p></div>
}
function ComponentList({ rows, area, openEntity }) {
  const [query, setQuery] = useState('')
  const [namespace, setNamespace] = useState('')
  const namespaces = [...new Set(rows.map(v => v.readTarget.ns))].sort()
  const filtered = rows.filter(v => (!namespace || namespace === v.readTarget.ns) && (v.displayName + ' ' + v.readTarget.ns + ' ' + (v.relation.pulsarName || '') + ' ' + (v.relation.bookkeeperName || '')).toLowerCase().includes(query.toLowerCase()))
  return <section className="card component-list"><div className="list-toolbar"><label className="search-field"><span className="sr-only">搜索组件或关联集群</span><input type="search" placeholder="搜索名称、命名空间或关联集群…" value={query} onChange={e => setQuery(e.target.value)} /></label><label><span className="sr-only">命名空间</span><select value={namespace} onChange={e => setNamespace(e.target.value)}><option value="">全部命名空间</option>{namespaces.map(ns => <option key={ns}>{ns}</option>)}</select></label><span className="muted small">{filtered.length} 个集群</span></div><div className="table-scroll"><table><thead><tr><th>集群</th><th>Namespace</th><th>组件就绪</th><th>{area === 'brokers' ? '使用的 BookKeeper' : '关联 Pulsar'}</th><th>管理方式</th><th>状态</th><th /></tr></thead><tbody>{filtered.map(v => { const c = v.components.find(c => c.key === (area === 'brokers' ? 'broker' : 'bookie')); const target = area === 'brokers' ? v.relation.bookkeeperKey : v.relation.pulsarKey; const label = area === 'brokers' ? v.relation.bookkeeperName : v.relation.pulsarName; return <tr key={v.key}><td><button className="link entity-link" onClick={() => openEntity(v.key)}>{v.displayName}</button></td><td>{v.readTarget.ns}</td><td><Ready ready={c?.status?.readyReplicas} total={c?.desired} /></td><td>{target && v.relation.state === 'linked' ? <button className="link" onClick={() => openEntity(target)}>{label} ↗</button> : label || '—'}</td><td><RelationBadge state={v.relation.state} /></td><td><PhaseBadge phase={v.phase} /></td><td><button className="ghost" onClick={() => openEntity(v.key)}>管理</button></td></tr> })}</tbody></table></div>{!filtered.length && <div className="empty compact"><h3>{rows.length ? '没有匹配的组件' : '尚无组件'}</h3><p>{rows.length ? '调整搜索条件或命名空间筛选。' : '创建集群后，组件将在这里显示。'}</p></div>}</section>
}
