import React, { useEffect, useId, useRef, useState } from 'react'
import { bytes, date, diskEligibility, observedDisk, planEligibility, poolPlanBody, volumeReadOnlyReason } from './storage-model.js'
import { StorageBadge, DetailRows, Reasons } from './storage-views.jsx'
import { poolAllocationReasons } from './pool-model.js'

export function StorageModal({ title, busy = false, onClose, children }) {
  const ref = useRef(null)
  const titleID = useId()
  useEffect(() => {
    const dialog = ref.current
    dialog?.showModal()
    return () => dialog?.close()
  }, [])
  return <dialog className="storage-dialog" ref={ref} aria-labelledby={titleID} onCancel={event => { event.preventDefault(); if (!busy) onClose() }}><header><h2 id={titleID}>{title}</h2><button aria-label="关闭对话框" disabled={busy} onClick={onClose}>×</button></header>{children}</dialog>
}
export function TokenForm({ onSave }) {
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  return <form className="storage-form" onSubmit={e => { e.preventDefault(); try { onSave(value.trim()) } catch (error) { setError(error.message) } }}><p>Storage 与 Pulsar 分别使用自己的凭证。Token 仅保存于当前标签页的 sessionStorage。</p><label>Storage Bearer Token<input type="password" autoComplete="off" autoFocus value={value} onChange={e => setValue(e.target.value)} /></label><p className="muted small">无需认证时可留空；清空并保存会删除原凭证。</p>{error && <div className="banner error" role="alert">{error}</div>}<button className="primary">保存并连接</button></form>
}
export function StorageDetails({ dialog, mode, now, onClose }) {
  const { disk, volume } = dialog
  return <StorageModal title={disk ? '磁盘详情' : '持久卷详情'} onClose={onClose}>{disk ? <><StorageBadge value={disk.state} /><DetailRows rows={[["设备", disk.name], ["物理节点", disk.node], ["稳定路径", disk.path], ["序列号", disk.serial], ["容量", bytes(disk.sizeBytes)], ["型号 / 介质", `${disk.model || '—'} / ${disk.media || 'Unknown'}`], ["存储池", disk.poolId], ["上报时间", date(disk.lastObservedTime)], ["有效期", date(disk.validUntil)]]} /><Reasons items={diskEligibility(disk, now, mode)} /></> : <><StorageBadge value={volume.status} /><DetailRows rows={[["PVC", `${volume.namespace}/${volume.name}`], ["PVC UID", volume.pvcUid], ["PV", volume.pvName], ["存储池", volume.pool || volume.poolId], ["物理节点", volume.node], ["StorageClass", volume.storageClass], ["申请容量", bytes(volume.sizeBytes)], ["挂载", volume.mountPath], ["创建时间", date(volume.createdAt)], ["归属", volume.management === 'external' ? '外部 Operator' : 'DiskFleet'], ["所有者", (volume.owners || []).map(owner => `${owner.kind}/${owner.name}`).join(', ')]]} /><Reasons items={[volumeReadOnlyReason(volume, mode)].filter(Boolean)} /></>}</StorageModal>
}
function useMutation() {
  const [busy, setBusy] = useState(false)
  const lock = useRef(false)
  const [error, setError] = useState('')
  const run = async fn => {
    if (lock.current) return
    lock.current = true; setBusy(true); setError('')
    try { await fn() } catch (error) { setError(error.message) } finally { lock.current = false; setBusy(false) }
  }
  return { busy, error, run }
}
function FormError({ message }) { return message ? <div className="banner error" role="alert">{message}</div> : null }
function FormButtons({ onClose, busy, disabled = false, label, danger = false }) {
  return <div className="row end"><button type="button" disabled={busy} onClick={onClose}>取消</button><button type="submit" className={danger ? 'danger' : 'primary'} disabled={busy || disabled}>{busy ? '正在提交…' : label}</button></div>
}

export function AgentDialog({ api, node, mode, onClose, onDone }) {
  const mutation = useMutation()
  return <StorageModal title="启用物理节点 Agent" busy={mutation.busy} onClose={onClose}><form className="storage-form" onSubmit={e => { e.preventDefault(); if (node.canEnableAgent !== true) return; mutation.run(async () => { await api.enableAgent(node); onDone(mode === 'demo' ? '已模拟启用 Agent，没有操作真实物理节点。' : '物理节点启用请求已接受，等待 Agent Pod 就绪并上报磁盘。') }) }}><DetailRows rows={[["物理节点", node.name], ["UID", node.uid], ["资源版本", node.resourceVersion]]} /><p>{mode === 'demo' ? '此操作仅更新模拟数据。' : '将为此物理节点启用 Agent DaemonSet 选择标签。不会开启真实写盘开关，也不会自动创建存储池。'}</p><Reasons items={node.enableReasons} /><FormError message={mutation.error} /><FormButtons onClose={onClose} busy={mutation.busy} disabled={node.canEnableAgent !== true} label={mode === 'demo' ? '模拟启用' : '确认启用 Agent'} /></form></StorageModal>
}

export function PoolPlanDialog({ api, disks, mode, now, onClose, onCreated }) {
  const [prefix, setPrefix] = useState('pool')
  const [plans, setPlans] = useState(null)
  const [confirmed, setConfirmed] = useState(false)
  const [results, setResults] = useState({})
  const originals = useRef(disks)
  const observations = useRef(disks)
  observations.current = disks
  const currentDisk = disk => observedDisk(disk, observations.current.find(latest => latest.id === disk.id) || disk, mode)
  const mutation = useMutation()
  const reasons = plans ? plans.filter(entry => !results[entry.disk.id]?.success).flatMap(entry => planEligibility(entry, now, mode, currentDisk(entry.disk))) : originals.current.flatMap(disk => diskEligibility(currentDisk(disk), now, mode))
  const pending = plans?.filter(entry => !results[entry.disk.id]?.success) || []
  const submit = event => {
    event.preventDefault()
    mutation.run(async () => {
      if (!plans) {
        const next = []
        for (const [index, original] of originals.current.entries()) {
          const disk = currentDisk(original)
          const reasons = diskEligibility(disk, Date.now(), mode)
          if (reasons.length) throw new Error(reasons.join('；'))
          const body = poolPlanBody(disk, prefix.trim(), index)
          next.push({ disk, name: body.name, plan: await api.plan(body) })
        }
        setPlans(next)
      } else {
        if (!confirmed) throw new Error('请先核对并确认设备身份。')
        for (const entry of pending) {
          const reasons = planEligibility(entry, Date.now(), mode, currentDisk(entry.disk))
          if (reasons.length) { setResults(previous => ({ ...previous, [entry.disk.id]: { success: false, message: reasons.join('；') } })); continue }
          try {
            const result = await api.createPool(entry.plan)
            setResults(previous => ({ ...previous, [entry.disk.id]: { success: true, message: result.canCreateVolume === true ? '存储池已允许分配业务卷' : '已创建存储池，请到存储池详情查看自动压测并完成人工审核' } }))
            onCreated(entry.disk.id)
          } catch (error) { setResults(previous => ({ ...previous, [entry.disk.id]: { success: false, message: error.message } })) }
        }
      }
    })
  }
  return <StorageModal title={plans ? '确认存储池计划' : '预览存储池计划'} busy={mutation.busy} onClose={onClose}><form className="storage-form" onSubmit={submit}><div className={'banner ' + (mode === 'demo' ? 'info' : 'warn')}>{mode === 'demo' ? '模拟纳管不初始化真实设备。' : plans ? '确认后创建 DiskPool；Agent 仅在批准状态和真实写盘开关均满足时初始化设备。请核对每块盘的身份。' : '此步骤只生成计划，不写入磁盘。每块设备创建独立存储池。'}</div><FormError message={mutation.error} />{!plans && <label>存储池名称前缀<input pattern="[a-z]([a-z0-9-]{0,30}[a-z0-9])?" maxLength={32} required value={prefix} disabled={mutation.busy} onChange={e => setPrefix(e.target.value)} /></label>}
      {(plans || originals.current.map(disk => ({ disk }))).map(({ disk, plan, name }) => <article className="storage-plan-item" key={disk.id}><h3>{name || disk.name || disk.id} · {disk.node}</h3><p className="mono">{disk.path}</p><p>序列号 {disk.serial} · {bytes(disk.sizeBytes)}</p>{mode === 'cluster' && <p>设备上报有效至 {date(currentDisk(disk).validUntil)}</p>}{plan && <><pre>{(plan.commands || []).map(command => Array.isArray(command) ? command.join(' ') : command).join('\n')}</pre><Reasons items={plan.warnings} /><p>计划有效至 {date(plan.expiresAt)}</p></>}{results[disk.id] && <p role="status" className={results[disk.id].success ? 'ok-text' : 'error-text'}>{results[disk.id].message}</p>}</article>)}
      <Reasons items={reasons} />{plans && pending.length > 0 && <label className="check"><input type="checkbox" checked={confirmed} disabled={mutation.busy} onChange={e => setConfirmed(e.target.checked)} required />我已核对设备身份与容量，确认接管这些{mode === 'demo' ? '模拟' : '空'}磁盘。</label>}
      {plans && pending.length === 0 ? <button type="button" className="primary" onClick={onClose}>完成</button> : <FormButtons onClose={onClose} busy={mutation.busy} disabled={reasons.length > 0 || !!plans && !confirmed} label={plans ? '确认提交存储池' : '生成接管计划 →'} />}
    </form></StorageModal>
}

export function CreateVolumeDialog({ api, pools, mode, onClose, onDone }) {
  const mutation = useMutation()
  const eligible = pools.filter(pool => !poolAllocationReasons(pool).length)
  const submit = event => {
    event.preventDefault()
    const data = new FormData(event.target)
    mutation.run(async () => {
      const selected = eligible.find(pool => pool.id === data.get('poolId'))
      if (!selected) throw new Error('请选择压测和人工审核均通过的可分配存储池。')
      const latest = await api.pool(selected.id)
      const reasons = poolAllocationReasons(latest)
      if (reasons.length) throw new Error(reasons.join('；'))
      if (latest.uid !== selected.uid || latest.benchmark.runID !== selected.benchmark.runID || latest.benchmark.resultDigest !== selected.benchmark.resultDigest) throw new Error('存储池或审核结果已变化，请关闭并重新选择存储池。')
      await api.createVolume({ name: data.get('name').trim(), namespace: data.get('namespace').trim(), poolId: selected.id, sizeGiB: Number(data.get('sizeGiB')) })
      onDone(mode === 'demo' ? '已完成模拟 PVC 分配。' : 'PVC 申请已提交，请等待调度与 CSI 置备。')
    })
  }
  return <StorageModal title="申请持久卷" busy={mutation.busy} onClose={onClose}><form className="storage-form" onSubmit={submit}>
    <FormError message={mutation.error} />
    {eligible.length ? <fieldset disabled={mutation.busy}><div className="grid2"><label>PVC 名称<input name="name" required maxLength={63} pattern="[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" /></label><label>命名空间<input name="namespace" defaultValue="default" required maxLength={63} pattern="[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" /></label></div><label>已通过审核的存储池<select name="poolId" required>{eligible.map(pool => <option key={pool.id} value={pool.id}>{pool.name} · {pool.node} · 可用 {bytes(pool.availableBytes)}</option>)}</select></label><p className="muted small">从当前前 100 个池中筛选已审核且可分配的池；也可从池列表对应行申请。</p><label>申请容量（GiB）<input name="sizeGiB" type="number" min={1} step={1} defaultValue={10} required /></label></fieldset> : <p>当前没有通过压测、清理与人工审核的可分配存储池。请先在存储池详情完成审核。</p>}
    <div className="banner info">{mode === 'demo' ? '模拟模式只分配演示目录。' : '命名空间需已存在。WaitForFirstConsumer 等待使用该 PVC 的 Pod 调度后再创建后端卷。'} 默认 Retain 保留数据。</div>
    <FormButtons onClose={onClose} busy={mutation.busy} disabled={!eligible.length} label="创建 PVC" />
  </form></StorageModal>
}

export function VolumeMutationDialog({ api, kind, volume, mode, onClose, onDone }) {
  const mutation = useMutation()
  const current = Math.ceil(Number(volume.sizeBytes) / 1024 ** 3)
  const reason = volumeReadOnlyReason(volume, mode)
  return <StorageModal title={kind === 'expand' ? '扩容持久卷' : '释放 PVC，保留数据'} busy={mutation.busy} onClose={onClose}><form className="storage-form" onSubmit={e => { e.preventDefault(); if (volumeReadOnlyReason(volume, mode)) return; const data = new FormData(e.target); mutation.run(async () => { if (kind === 'expand') { await api.expand(volume, Number(data.get('sizeGiB'))); onDone(mode === 'demo' ? '模拟卷容量已更新。' : '扩容请求已提交，请等待 CSI 完成卷与文件系统扩容。') } else { await api.release(volume, mode); onDone('释放请求已提交，Retain 保留后端数据与容量占用。') } }) }}><DetailRows rows={[["PVC", `${volume.namespace}/${volume.name}`], ["当前容量", bytes(volume.sizeBytes)], ["池", volume.pool || volume.poolId]]} /><FormError message={mutation.error || reason} />{kind === 'expand' ? <><label>目标总容量（GiB）<input name="sizeGiB" type="number" min={current + 1} step={1} defaultValue={current + 10} disabled={mutation.busy} required /></label><p>仅支持增大容量。申请成功与文件系统扩容完成是两个阶段。</p></> : <><div className="banner warn">{mode === 'demo' ? '卷将标记为 Released。' : '将删除 PVC，请先停止使用它的业务 Pod。'} Retain 保留后端卷和数据，不回收存储池容量。</div><label className="check"><input type="checkbox" required disabled={mutation.busy} />确认释放此 PVC，并保留后端数据。</label></>}<FormButtons onClose={onClose} busy={mutation.busy} disabled={!!reason} label={kind === 'expand' ? '提交扩容' : '释放并保留'} danger={kind === 'release'} /></form></StorageModal>
}
