import React, { useCallback, useEffect, useRef, useState } from 'react'
import { api, ApiError, setToken } from './api.js'
import { inventory } from './workspace.js'
import { ClusterView } from './ClusterView.jsx'
import { CreateDialog } from './CreateDialog.jsx'
import { CreateBookKeeperDialog } from './CreateBookKeeperDialog.jsx'
import { StorageWorkspace, STORAGE_PAGES } from './StorageWorkspace.jsx'
import { PhaseBadge, Ready } from './ui.jsx'

const names = { overview: '运行总览', brokers: 'Broker 组件', bookies: 'Bookie 组件', storage: '物理存储' }
function parseRoute() {
  try {
    const [area = 'overview', ns, name] = location.hash.replace(/^#\/?/, '').split('/').map(decodeURIComponent)
    return names[area] ? { area, ns, name } : { area: 'overview' }
  } catch { return { area: 'overview' } }
}
const routeFor = key => {
  const [kind, ns, name] = key.split('/')
  return [kind === 'PulsarCluster' ? 'brokers' : 'bookies', ns, name].map(encodeURIComponent).join('/')
}

export function ConsoleShell() {
  const [route, setRoute] = useState(parseRoute)
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
    const changed = () => setRoute(parseRoute())
    window.addEventListener('hashchange', changed)
    refresh()
    const timer = setInterval(() => refresh(false), 5000)
    return () => {
      requestVersion.current++; pending.current = false
      clearInterval(timer); window.removeEventListener('hashchange', changed)
    }
  }, [refresh])
  const navigate = path => { location.hash = '/' + path }
  const openEntity = key => navigate(routeFor(key))
  const catalog = inventory(data || {})
  const rows = route.area === 'brokers' ? catalog.brokers : catalog.bookies
  const current = route.name ? rows.find(v => v.readTarget.ns === route.ns && v.displayName === route.name) : null
  const storagePage = STORAGE_PAGES.some(p => p.key === route.ns) ? route.ns : 'overview'
  const created = key => { setCreating(null); openEntity(key); refresh() }

  return <div className="console-layout">
    <aside className="console-sidebar">
      <a className="console-brand" href="#/overview"><span className="brand-mark">P</span><span>Pulsar<span className="brand-caption">OPERATOR CONSOLE</span></span></a>
      <div className="nav-caption">工作空间</div>
      <nav aria-label="控制台主导航">
        {Object.entries(names).map(([area, label]) => <button key={area} className={'workspace-nav' + (route.area === area ? ' active' : '')} aria-current={route.area === area ? 'page' : undefined} onClick={() => navigate(area)}>
          <span>{label}</span>{area === 'brokers' && <small>{catalog.brokers.length}</small>}{area === 'bookies' && <small>{catalog.bookies.length}</small>}
        </button>)}
      </nav>
      {route.area === 'storage' && <nav className="storage-subnav" aria-label="存储管理导航">{STORAGE_PAGES.map(p => <button key={p.key} className={storagePage === p.key ? 'active' : ''} aria-current={storagePage === p.key ? 'page' : undefined} onClick={() => navigate('storage/' + p.key)}>{p.label}</button>)}</nav>}
      <div className="sidebar-note"><span className="live-dot" /> Kubernetes 控制面<p>计算组件与物理存储<br />统一视图 · 独立管理</p></div>
    </aside>
    <div className="console-body">
      <header className="console-topbar">
        <div className="console-breadcrumb">工作空间 <span>/</span> <b>{names[route.area]}</b>{current && <><span>/</span>{current.displayName}</>}</div>
        <div className="inline"><span className="sync-note">{updated ? '更新于 ' + updated.toLocaleTimeString() : '等待连接'}</span><button onClick={() => setCredentials(true)}>Pulsar 凭证</button><button onClick={refresh} disabled={busy}>{busy ? '同步中…' : '刷新'}</button></div>
      </header>
      <main className={'console-main' + (route.area === 'storage' ? ' storage-main' : '')}>
        {route.area !== 'storage' && error && <div className="banner error" role="alert">无法同步集群：{error}{data && '。下方为上次成功同步的数据。'}</div>}
        {route.area !== 'storage' && needToken ? <TokenForm onSave={t => { setToken(t); refresh() }} /> : route.area === 'storage' ? <StorageWorkspace page={storagePage} onPageChange={p => navigate('storage/' + p)} /> : <>
          {!data && !error ? <div className="empty">正在读取 Kubernetes 集群…</div> : route.area === 'overview' ? <Overview catalog={catalog} openEntity={openEntity} navigate={navigate} onCreate={() => setCreating('pulsar')} /> : <>
            <div className="workspace-heading"><div><div className="eyebrow">{route.area === 'brokers' ? 'PULSAR / COMPUTE' : 'BOOKKEEPER / PERSISTENCE'}</div><h1>{names[route.area]}</h1><p>{route.area === 'brokers' ? '管理 Pulsar 的 Broker、接入与元数据组件，查看关联的 BookKeeper 集群。' : '独立查看每组 Bookie 的副本、磁盘与回收任务，以及使用它的 Pulsar 集群。'}</p></div><button className="primary" onClick={() => setCreating(route.area === 'brokers' ? 'pulsar' : 'bookkeeper')}>{route.area === 'brokers' ? '+ 新建 Pulsar 集群' : '+ 新建独立 BookKeeper'}</button></div>
            {route.name && <button className="link back-link" onClick={() => navigate(route.area)}>← 返回组件列表</button>}
            {current ? <>
              <Relationship view={current} openEntity={openEntity} />
              <ClusterView key={current.key + '/' + current.obj.metadata.uid} view={current} onChanged={refresh} onDeleted={() => { navigate(route.area); refresh() }} />
            </> : route.name ? <div className="card empty"><h2>未找到该组件</h2><p>资源可能已删除，或当前凭据无法读取。</p></div> : <ComponentList key={route.area} rows={rows} area={route.area} openEntity={openEntity} />}
          </>}
        </>}
      </main>
    </div>
    {creating === 'pulsar' && <CreateDialog onClose={() => setCreating(null)} onCreated={created} />}
    {creating === 'bookkeeper' && <CreateBookKeeperDialog onClose={() => setCreating(null)} onCreated={created} />}
    {credentials && <div className="overlay"><div className="card dialog token-dialog"><TokenForm onSave={t => { setToken(t); setCredentials(false); refresh() }} /><button onClick={() => setCredentials(false)}>取消</button></div></div>}
  </div>
}

function TokenForm({ onSave }) {
  const [value, setValue] = useState('')
  return <form className="token-form" onSubmit={e => { e.preventDefault(); onSave(value) }}><h2>连接 Pulsar 控制台</h2><p className="muted">输入管理员提供的 Pulsar 访问凭证。物理存储的凭证在存储页面单独设置。</p><label>访问凭证<input type="password" autoComplete="off" value={value} required onChange={e => setValue(e.target.value)} /></label><button className="primary">连接集群</button></form>
}

function Overview({ catalog, openEntity, navigate, onCreate }) {
  const linked = catalog.brokers.filter(v => v.relation.state === 'linked').length
  return <>
    <div className="workspace-heading"><div><div className="eyebrow">WORKSPACE OVERVIEW</div><h1>运行总览</h1><p>分别管理 Broker 与 Bookie，沿集群关联追踪消息存储。</p></div><button className="primary" onClick={onCreate}>+ 新建 Pulsar 集群</button></div>
    <div className="workspace-stats">
      <Stat title="Broker 集群" value={catalog.brokers.length} note="Pulsar 计算与接入" onClick={() => navigate('brokers')} />
      <Stat title="Bookie 集群" value={catalog.bookies.length} note="BookKeeper 持久化" onClick={() => navigate('bookies')} />
      <Stat title="已确认的集群关联" value={linked} note="依据资源所有权识别" />
      <Stat title="物理存储" value="PV / PVC" note="磁盘 · 存储池 · 容量" onClick={() => navigate('storage/overview')} />
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
