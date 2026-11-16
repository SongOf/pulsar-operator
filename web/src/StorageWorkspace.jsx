import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createStorageAPI, saveStorageToken } from './storage-api.js'
import { count, diskEligibility, volumeReadOnlyReason } from './storage-model.js'
import { STORAGE_PAGES } from './routes.js'
import { Section } from './ui.jsx'
import { StorageOverview, NodesTable, NodeSummary, DisksTable, PoolsTable, VolumesTable, EventsTable, Pagination, Reasons, StorageGuide } from './storage-views.jsx'
import { StorageModal, TokenForm, StorageDetails, PoolPlanDialog, AgentDialog, CreateVolumeDialog, VolumeMutationDialog } from './StorageDialogs.jsx'
import { PoolDetails, PoolReviewDialog } from './PoolDetails.jsx'
import { poolAllocationReasons } from './pool-model.js'
import './storage-workspace.css'

const descriptions = {
  overview: '查看物理节点、磁盘、存储池和持久卷的实际状态。', nodes: '所有加入 Kubernetes 的物理节点均可见。启用 Agent 后，等待真实设备发现。',
  disks: '核对设备身份与上报有效期，预览并确认接管计划。', pools: '查看单盘压测与人工审核；审核通过后才允许分配业务卷。',
  volumes: '申请、扩容和保留持久卷。外部 Operator 的 PVC 只读观察。', events: '查看磁盘纳管、物理节点启用与卷生命周期的审计记录。', guide: '从节点接入、设备发现到业务使用持久卷。',
}
function useClock() {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    const tick = () => setNow(Date.now())
    const timer = setInterval(tick, 500)
    document.addEventListener('visibilitychange', tick)
    return () => { clearInterval(timer); document.removeEventListener('visibilitychange', tick) }
  }, [])
  return now
}

export function StorageWorkspace({ route, apiBase = '/storage/api', storageOnly = false, onNavigate }) {
  const api = useMemo(() => createStorageAPI(apiBase), [apiBase])
  const [config, setConfig] = useState(null)
  const [configError, setConfigError] = useState('')
  const [credentials, setCredentials] = useState(false)
  const [session, setSession] = useState(0)
  const [retry, setRetry] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    setConfig(null); setConfigError('')
    api.config(controller.signal).then(value => { if (!controller.signal.aborted) setConfig(value) }).catch(error => { if (!controller.signal.aborted) setConfigError(error.message) })
    return () => controller.abort()
  }, [api, retry])
  const save = value => { saveStorageToken(value); setCredentials(false); setSession(s => s + 1) }
  return <div className="storage-workspace">
    {storageOnly && <div className="banner info small">当前为独立 Storage 服务。Pulsar 页面请通过统一 Gateway 访问。</div>}
    {configError && <div className="banner error" role="alert">{configError} <button onClick={() => setRetry(n => n + 1)}>重新连接</button></div>}
    {!config && !configError && <div className="empty" role="status">正在连接 Storage API…</div>}
    {config && <StorageContent key={session} api={api} config={config} route={route} onNavigate={onNavigate} onCredentials={() => setCredentials(true)} />}
    {credentials && <StorageModal title="Storage 访问凭证" onClose={() => setCredentials(false)}><TokenForm onSave={save} /></StorageModal>}
  </div>
}

function StorageContent({ api, config, route, onNavigate, onCredentials }) {
  const { page, name: resourceName } = route
  const nodeName = page === 'nodes' ? resourceName : undefined
  const poolID = page === 'pools' ? resourceName : undefined
  const mode = config.mode
  const [filters, setFilters] = useState({ offset: 0, limit: 20, q: '', state: '', node: '' })
  const [draft, setDraft] = useState('')
  const [data, setData] = useState(null)
  const [error, setError] = useState('')
  const [needToken, setNeedToken] = useState(false)
  const [busy, setBusy] = useState(false)
  const [updated, setUpdated] = useState(null)
  const [interval, setIntervalSeconds] = useState(15)
  const [selected, setSelected] = useState(new Map())
  const [dialog, setDialog] = useState(null)
  const [notice, setNotice] = useState('')
  const request = useRef(null)
  const version = useRef(0)
  const now = useClock()
  const pageKey = page + '/' + (resourceName || '')
  const activeKey = useRef(pageKey)
  const navigate = (page, name) => {
    setFilters({ offset: 0, limit: 20, q: '', state: '', node: '' }); setDraft(''); setDialog(null)
    onNavigate(page, name)
  }
  useEffect(() => {
    if (activeKey.current !== pageKey) {
      activeKey.current = pageKey
      setData(null); setFilters({ offset: 0, limit: 20, q: '', state: '', node: '' }); setDraft(''); setDialog(null)
    }
  }, [pageKey])
  const refresh = useCallback(async (force = true) => {
    if (page === 'guide') return
    if (request.current && !force) return
    request.current?.abort()
    const controller = new AbortController()
    request.current = controller
    const current = ++version.current
    setBusy(true)
    try {
      let next
      if (page === 'overview') {
        const [summary, nodes, disks] = await Promise.all([api.overview(controller.signal), api.list('nodes', { limit: 6 }, controller.signal), api.list('disks', { limit: 24 }, controller.signal)])
        next = { summary, nodes, disks }
      } else if (page === 'nodes' && nodeName) next = await api.node(nodeName, filters, controller.signal)
      else if (poolID) next = await api.pool(poolID, controller.signal)
      else next = await api.list(page, filters, controller.signal)
      if (controller.signal.aborted || version.current !== current) return
      setData({ key: pageKey, value: next }); setError(''); setNeedToken(false); setUpdated(new Date())
      const disks = page === 'disks' ? next.items : page === 'nodes' && nodeName ? next.disks.items : []
      setSelected(previous => {
        const result = new Map(previous)
        for (const disk of disks) if (result.has(disk.id)) result.set(disk.id, disk)
        return result
      })
    } catch (error) {
      if (controller.signal.aborted || version.current !== current) return
      setError(error.message)
      if ([401, 403].includes(error.status)) { setNeedToken(true); setData(null); setSelected(new Map()); setDialog(null) }
    } finally {
      if (version.current === current) { request.current = null; setBusy(false) }
    }
  }, [api, page, nodeName, poolID, pageKey, filters])
  useEffect(() => {
    refresh()
    return () => { version.current++; request.current?.abort(); request.current = null }
  }, [refresh])
  useEffect(() => {
    if (!interval || page === 'guide') return
    const timer = setInterval(() => { if (!document.hidden) refresh(false) }, interval * 1000)
    return () => clearInterval(timer)
  }, [refresh, interval, page])
  const changeFilters = change => { setData(null); setFilters(old => ({ ...old, offset: 0, ...change })) }
  const value = data?.key === pageKey ? data.value : null
  const completed = message => { setNotice(message); setDialog(null); refresh() }
  const openVolume = async pool => {
    try {
      const candidates = pool ? [await api.pool(pool.id)] : (await api.list('pools', { limit: 100 })).items
      const pools = candidates.filter(candidate => !poolAllocationReasons(candidate).length)
      setDialog({ kind: 'createVolume', pools })
    } catch (error) { setNotice(error.message) }
  }
  const toggleDisk = (disk, checked) => {
    if (checked && diskEligibility(disk, Date.now(), mode).length) return
    setSelected(previous => {
      const next = new Map(previous)
      if (!checked) next.delete(disk.id)
      else if (next.size < 20) next.set(disk.id, disk)
      return next
    })
  }
  const selectionReasons = [...selected.values()].flatMap(disk => diskEligibility(disk, now, mode).map(reason => `${disk.node} / ${disk.name || disk.id}：${reason}`))
  const showPlan = () => {
    const disks = [...selected.values()]
    if (!disks.length || disks.some(disk => diskEligibility(disk, Date.now(), mode).length)) return
    setDialog({ kind: 'plan', disks })
  }
  const node = name => navigate('nodes', name)
  const showVolume = (kind, volume) => {
    if (kind !== 'volume' && volumeReadOnlyReason(volume, mode)) return
    setDialog({ kind, volume })
  }
  const list = page === 'nodes' && nodeName ? value?.disks : value
  const title = resourceName ? poolID ? value?.name || poolID : nodeName : STORAGE_PAGES.find(item => item.key === page)?.label
  return <>
    <div className="workspace-toolbar"><span className={'badge ' + (mode === 'demo' ? 'warn' : 'info')}>{mode === 'demo' ? 'DEMO · 模拟环境' : 'CLUSTER · 真实集群'}</span><div className="inline"><span className="sync-note">{updated ? '最近同步 ' + updated.toLocaleTimeString() : '等待同步'}</span><label className="storage-refresh">自动刷新<select value={interval} onChange={e => setIntervalSeconds(Number(e.target.value))}><option value={15}>15 秒</option><option value={30}>30 秒</option><option value={0}>关闭</option></select></label><button onClick={onCredentials}>Storage 凭证</button><button onClick={() => refresh()} disabled={busy || page === 'guide'}>{busy ? '同步中…' : '刷新'}</button></div></div>
    <div className="workspace-heading"><div><div className="eyebrow">STORAGE / DISKFLEET</div><h1>{title}</h1><p>{descriptions[page]}</p></div>{page === 'volumes' && <button className="primary" onClick={() => openVolume()} disabled={needToken}>＋ 申请 PVC</button>}{page === 'pools' && <button className="primary" onClick={() => navigate('disks')}>＋ 接管磁盘</button>}</div>
    {mode === 'demo' && <div className="banner info">当前为模拟环境；节点、磁盘、Agent 和挂载状态均为演示数据，操作不触碰真实设备。</div>}
    {error && <div className="banner error" role="alert">{error}{value && ' 下方保留上次同步结果；磁盘超时后会自动禁用接管。'}{needToken && <button onClick={onCredentials}>设置 Storage 凭证</button>}</div>}
    {notice && <div className="banner info" role="status">{notice} <button className="link" onClick={() => setNotice('')}>关闭提示</button></div>}
    {page === 'guide' ? <StorageGuide /> : <>
      {nodeName && page === 'nodes' && <button className="link back-link" onClick={() => navigate('nodes')}>← 返回物理节点列表</button>}
      {poolID && <button className="link back-link" onClick={() => navigate('pools')}>← 返回存储池列表</button>}
      {nodeName && value?.node && <NodeSummary node={value.node} mode={mode} onEnable={node => setDialog({ kind: 'agent', node })} />}
      {page !== 'overview' && !poolID && <div className="storage-filters"><form onSubmit={e => { e.preventDefault(); changeFilters({ q: draft.trim() }) }} role="search"><label className="sr-only" htmlFor="storage-search">搜索当前列表</label><input id="storage-search" type="search" value={draft} onChange={e => setDraft(e.target.value)} placeholder="搜索名称、节点或设备身份…" /><button>搜索</button></form>{(page === 'disks' || page === 'nodes' && nodeName) && <label>磁盘状态<select value={filters.state} onChange={e => changeFilters({ state: e.target.value })}><option value="">全部</option>{['Available', 'Managed', 'Protected', 'Offline'].map(state => <option key={state}>{state}</option>)}</select></label>}</div>}
      {(page === 'disks' || page === 'nodes' && nodeName) && <section className="storage-selection" aria-label="存储池计划"><div className="inline"><span>已选择 {selected.size} 块磁盘（每批最多 20 块）</span><button onClick={() => setSelected(new Map())} disabled={!selected.size}>取消选择</button><button className="primary" onClick={showPlan} disabled={!selected.size || selectionReasons.length > 0 || needToken}>预览存储池计划 →</button></div><Reasons items={selectionReasons} /></section>}
      {!value && !error && <div className="empty" role="status">正在读取存储状态…</div>}
      {value && (page === 'overview' ? <StorageOverview data={value} onNavigate={navigate} onNode={node} onDisk={disk => setDialog({ kind: 'disk', disk })} /> : poolID ? <PoolDetails pool={value} mode={mode} onReview={pool => setDialog({ kind: 'reviewPool', pool })} onVolume={openVolume} onNode={node} /> : <Section title={page === 'nodes' && nodeName ? 'Agent 上报的磁盘' : `${title} · ${count(list.total)}`}>
        {page === 'nodes' && !nodeName ? <NodesTable items={list.items} onNode={node} /> : page === 'disks' || page === 'nodes' ? <DisksTable items={list.items} selected={selected} onToggle={toggleDisk} onDetail={disk => setDialog({ kind: 'disk', disk })} onNode={node} now={now} mode={mode} /> : page === 'pools' ? <PoolsTable items={list.items} onVolume={openVolume} onNode={node} onPool={id => navigate('pools', id)} /> : page === 'volumes' ? <VolumesTable items={list.items} mode={mode} onDetail={v => showVolume('volume', v)} onExpand={v => showVolume('expand', v)} onRelease={v => showVolume('release', v)} /> : <EventsTable items={list.items} />}
        <Pagination data={list} busy={busy} onOffset={offset => changeFilters({ offset })} />
      </Section>)}
      {page === 'volumes' && <div className="banner info">{mode === 'demo' ? 'Bound 仅表示模拟分配成功。' : 'WaitForFirstConsumer：PVC 等待首次使用它的 Pod 参与调度后再绑定；扩容申请与文件系统扩容完成是不同阶段。'} Retain 释放 PVC 后保留后端数据。</div>}
      {page === 'pools' && <div className="banner info">存储池绑定单个节点；Retain 保留后端数据，释放 PVC 不会释放池容量。</div>}
    </>}
    {['disk', 'volume'].includes(dialog?.kind) && <StorageDetails dialog={dialog} mode={mode} now={now} onClose={() => setDialog(null)} />}
    {dialog?.kind === 'plan' && <PoolPlanDialog api={api} disks={dialog.disks.map(disk => selected.get(disk.id) || disk)} mode={mode} now={now} onClose={() => setDialog(null)} onCreated={id => { setSelected(previous => { const next = new Map(previous); next.delete(id); return next }); refresh() }} />}
    {dialog?.kind === 'agent' && <AgentDialog api={api} node={dialog.node} mode={mode} onClose={() => setDialog(null)} onDone={completed} />}
    {dialog?.kind === 'createVolume' && <CreateVolumeDialog api={api} pools={dialog.pools} mode={mode} onClose={() => setDialog(null)} onDone={message => { completed(message); navigate('volumes') }} />}
    {dialog?.kind === 'reviewPool' && <PoolReviewDialog pool={dialog.pool} currentPool={error || poolID !== dialog.pool.id ? null : value} api={api} mode={mode} onClose={() => setDialog(null)} onRefresh={() => refresh()} onDone={result => { setData({ key: pageKey, value: result }); completed(result.allocationApproval?.decision === 'Rejected' ? '审核拒绝已记录，仍禁止此池分配业务卷。' : '审核通过已记录。等待服务端与 Agent 确认可分配状态。') }} />}
    {['expand', 'release'].includes(dialog?.kind) && <VolumeMutationDialog api={api} kind={dialog.kind} volume={dialog.volume} mode={mode} onClose={() => setDialog(null)} onDone={completed} />}
  </>
}
