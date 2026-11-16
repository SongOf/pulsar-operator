import React from 'react'
import { Section } from './ui.jsx'
import { bytes, count, date, percent, statusLabels, statusTone, diskEligibility, volumeMessages, volumeReadOnlyReason } from './storage-model.js'
import { poolAllocationReasons } from './pool-model.js'

export function StorageBadge({ value }) {
  return <span className={'badge ' + statusTone(value)}>{statusLabels[value] || value || '未知'}</span>
}
export function Empty({ children = '暂无记录，新的状态会在下次同步后显示。' }) {
  return <div className="empty compact">{children}</div>
}
export function Reasons({ items = [] }) {
  return items.length ? <ul className="storage-reasons">{[...new Set(items)].map(reason => <li key={reason}>{reason}</li>)}</ul> : null
}
export function DetailRows({ rows }) {
  return <dl className="storage-details">{rows.map(([label, value]) => <React.Fragment key={label}><dt>{label}</dt><dd>{value === undefined || value === null || value === '' ? '—' : value}</dd></React.Fragment>)}</dl>
}
function Usage({ pool }) {
  const value = percent(pool.allocatedBytes, pool.capacityBytes)
  return <div className="storage-usage"><span>{bytes(pool.allocatedBytes)} · {value}%</span><meter aria-label="已分配比例" min="0" max="100" value={value} /></div>
}
export function Pagination({ data, onOffset, busy }) {
  const { total = 0, limit = 20, offset = 0 } = data || {}
  return <div className="storage-pagination"><span>显示 {total ? count(offset + 1) : 0}–{count(Math.min(offset + limit, total))} 条，共 {count(total)} 条</span><div className="inline"><button disabled={busy || offset === 0} onClick={() => onOffset(Math.max(0, offset - limit))}>← 上一页</button><span>{Math.floor(offset / limit) + 1} / {Math.max(1, Math.ceil(total / limit))}</span><button disabled={busy || offset + limit >= total} onClick={() => onOffset(offset + limit)}>下一页 →</button></div></div>
}
export function StorageOverview({ data, onNavigate, onNode, onDisk }) {
  const { summary, nodes, disks } = data
  const t = summary.totals || {}
  return <>
    {(summary.alerts || []).map((item, i) => <div className="banner warn" key={i}>{typeof item === 'string' ? item : item.message || item.title}</div>)}
    <div className="workspace-stats">{[['纳管总容量', bytes(t.capacityBytes), 'pools'], ['物理节点', count(t.nodes), 'nodes'], ['物理磁盘', count(t.disks), 'disks'], ['持久卷', count(t.volumes), 'volumes']].map(([title, value, page]) => <div className="workspace-stat" key={page}><div className="muted small">{title}</div><div className="stat-number">{value}</div><button className="link" onClick={() => onNavigate(page)}>查看详情 →</button></div>)}</div>
    <div className="workspace-columns"><Section title="存储池容量"><div className="storage-capacity">{bytes(t.availableBytes)}<span>可用 / {bytes(t.capacityBytes)}</span></div><Usage pool={t} /><p className="muted small">厚置备 · Retain 保留数据 · 释放 PVC 不代表回收物理容量</p></Section><Section title="最近操作" actions={<button className="link" onClick={() => onNavigate('events')}>全部记录 →</button>}>{summary.recentEvents?.length ? <ul className="events">{summary.recentEvents.slice(0, 4).map((event, i) => <li key={event.id || i}><strong>{event.action}</strong><p>{event.message}</p><time className="muted small">{date(event.time)}</time></li>)}</ul> : <Empty>暂无操作记录</Empty>}</Section></div>
    <Section title="物理节点概览" actions={<button className="link" onClick={() => onNavigate('nodes')}>查看全部 {count(nodes.total)} 个 →</button>}><NodesTable items={nodes.items} onNode={onNode} /></Section>
    <Section title="磁盘分布" actions={<button className="link" onClick={() => onNavigate('disks')}>查看全部 {count(disks.total)} 块 →</button>}><div className="storage-disk-map">{disks.items.map(disk => <button key={disk.id} className={'storage-disk ' + statusTone(disk.state)} onClick={() => onDisk(disk)} title={`${disk.node} / ${disk.path} · ${statusLabels[disk.state] || disk.state}`}><strong>{disk.name || disk.id}</strong><span>{disk.node} · {bytes(disk.sizeBytes)}</span><StorageBadge value={disk.state} /></button>)}</div>{!disks.items.length && <Empty>Agent 发现设备后会显示磁盘。</Empty>}</Section>
  </>
}
export function NodesTable({ items = [], onNode }) {
  return items.length ? <div className="table-scroll"><table><thead><tr><th>物理节点</th><th>可用区</th><th>物理节点状态</th><th>Agent 状态</th><th>磁盘</th><th>磁盘总容量</th><th /></tr></thead><tbody>{items.map(node => <tr key={node.uid || node.id || node.name}><td><button className="link entity-link" onClick={() => onNode(node.name)}>{node.name}</button>{node.unschedulable && <span className="storage-subline">已禁止调度</span>}</td><td>{node.zone || '未设置'}</td><td><StorageBadge value={node.state} /></td><td><StorageBadge value={node.agentStatus || (node.mode === 'demo' ? '模拟' : 'Unknown')} /></td><td>{count(node.disks)}</td><td>{bytes(node.capacityBytes)}</td><td><button onClick={() => onNode(node.name)}>查看物理节点 →</button></td></tr>)}</tbody></table></div> : <Empty>当前没有可读取的 Kubernetes 物理节点。</Empty>
}
export function NodeSummary({ node, mode, onEnable }) {
  return <Section title="物理节点与 Agent" actions={<button className="primary" disabled={node.canEnableAgent !== true} onClick={() => onEnable(node)}>{mode === 'demo' ? '模拟启用 Agent' : node.enrolled ? '已启用 Agent' : '启用 Agent'}</button>}>
    <div className="inline"><StorageBadge value={node.state} /><span>Agent</span><StorageBadge value={node.agentStatus || (mode === 'demo' ? '模拟' : 'Unknown')} /></div>
    <DetailRows rows={[["物理节点", node.name], ["UID", node.uid], ["可用区", node.zone], ["调度", node.unschedulable ? '已禁止调度' : '允许调度'], ["Agent 选择标签", node.enrolled ? '已启用' : '未启用'], ["已上报磁盘", count(node.disks)], ["磁盘总容量", bytes(node.capacityBytes)]]} />
    <Reasons items={[...(node.enableReasons || []), ...(node.reasons || [])]} />
    <p className="muted small">启用请求只使已部署的 Agent DaemonSet 选择此节点。等待 Pod 就绪并上报磁盘后，才能生成存储池计划。</p>
  </Section>
}
export function DisksTable({ items = [], selected = new Map(), onToggle, onDetail, onNode, now, mode }) {
  const available = items.filter(disk => !diskEligibility(disk, now, mode).length)
  const allSelected = available.length > 0 && available.every(disk => selected.has(disk.id))
  return items.length ? <div className="table-scroll"><table><thead><tr><th><input type="checkbox" aria-label="选择本页可接管磁盘" checked={allSelected} disabled={!available.length} onChange={event => available.forEach(disk => onToggle(disk, event.target.checked))} /></th><th>设备</th><th>物理节点 / 序列号</th><th>容量 / 介质</th><th>状态与接管条件</th><th>上报时间</th><th /></tr></thead><tbody>{items.map(disk => {
    const reasons = diskEligibility(disk, now, mode)
    return <tr key={disk.id}><td><input type="checkbox" aria-label={`选择 ${disk.node} ${disk.name || disk.path}`} checked={selected.has(disk.id)} disabled={reasons.length > 0 && !selected.has(disk.id)} onChange={event => onToggle(disk, event.target.checked)} /></td><td><strong>{disk.name || disk.id}</strong><span className="storage-subline mono">{disk.path}</span></td><td><button className="link" onClick={() => onNode(disk.node)}>{disk.node}</button><span className="storage-subline">{disk.serial || '缺少序列号'}</span></td><td>{bytes(disk.sizeBytes)}<span className="storage-subline">{disk.media || 'Unknown'}</span></td><td><StorageBadge value={disk.state} />{reasons.length > 0 && <Reasons items={reasons} />}</td><td>{mode === 'demo' ? '模拟数据' : <><span>{date(disk.lastObservedTime)}</span><span className="storage-subline">有效至 {date(disk.validUntil)}</span></>}</td><td><button className="link" onClick={() => onDetail(disk)}>详情</button></td></tr>
  })}</tbody></table></div> : <Empty>尚无匹配的磁盘。节点加入 Kubernetes 后仍需启用 Agent 并等待设备上报。</Empty>
}
export function PoolsTable({ items = [], onVolume, onNode, onPool }) {
  return items.length ? <div className="table-scroll"><table><thead><tr><th>存储池 / VG</th><th>物理节点</th><th>总容量 / 可用</th><th>已分配</th><th>池阶段</th><th>压测</th><th>人工审核</th><th>操作</th></tr></thead><tbody>{items.map(pool => {
    const reasons = poolAllocationReasons(pool)
    return <tr key={pool.uid || pool.id}><td><button className="link entity-link" onClick={() => onPool(pool.id)}>{pool.name}</button><span className="storage-subline mono">{pool.vgName}</span><span className="storage-subline">{pool.storageClass}</span>{pool.message && <span className="storage-subline">{pool.message}</span>}</td><td><button className="link" onClick={() => onNode(pool.node)}>{pool.node}</button></td><td>{bytes(pool.capacityBytes)}<span className="storage-subline">可用 {bytes(pool.availableBytes)}</span></td><td><Usage pool={pool} /></td><td><StorageBadge value={pool.state} /><span className="storage-subline">{statusLabels[pool.allocationState] || pool.allocationState}</span></td><td><StorageBadge value={pool.benchmark?.phase || 'AwaitingBenchmark'} /><span className="storage-subline">{pool.benchmark?.cleanupVerified === true ? '清理已确认' : '清理未确认'}</span></td><td><StorageBadge value={pool.allocationApproval?.decision || 'AwaitingReview'} /></td><td><div className="inline"><button className="link" onClick={() => onPool(pool.id)}>详情与审核</button><button disabled={reasons.length > 0} onClick={() => onVolume(pool)}>申请 PVC</button></div><Reasons items={reasons} /></td></tr>
  })}</tbody></table></div> : <Empty>从节点详情或物理磁盘页选择可接管设备，预览并确认计划后创建存储池。</Empty>
}
export function VolumesTable({ items = [], mode, onDetail, onExpand, onRelease }) {
  return items.length ? <div className="table-scroll"><table><thead><tr><th>PVC / 命名空间</th><th>存储池 / StorageClass</th><th>物理节点</th><th>申请容量</th><th>状态</th><th>操作</th></tr></thead><tbody>{items.map(volume => {
    const reason = volumeReadOnlyReason(volume, mode)
    const unverified = volume.status === 'Bound' && volume.bindingVerified === false
    return <tr key={volume.id}><td><strong>{volume.name}</strong><span className="storage-subline">{volume.namespace}{volume.management === 'external' ? ' · 外部 Operator · 只读' : reason ? ' · 只读' : ''}</span></td><td>{unverified ? '归属待核验' : volume.pool || volume.poolId || '待分配'}<span className="storage-subline mono">{volume.storageClass}</span></td><td>{unverified ? '绑定身份待核验' : volume.node || '等待调度'}<Reasons items={volumeMessages(volume)} /></td><td>{bytes(volume.sizeBytes)}</td><td><StorageBadge value={volume.status} /></td><td><div className="inline"><button className="link" onClick={() => onDetail(volume)}>{reason ? '只读详情' : '详情'}</button>{!reason && <><button className="link" onClick={() => onExpand(volume)}>扩容</button><button className="link" onClick={() => onRelease(volume)}>释放</button></>}</div>{reason && <p className="muted small">{reason}</p>}</td></tr>
  })}</tbody></table></div> : <Empty>选择一个就绪的存储池，为业务申请 PVC。</Empty>
}
export function EventsTable({ items = [] }) {
  return items.length ? <div className="table-scroll"><table><thead><tr><th>时间</th><th>级别</th><th>操作</th><th>物理节点</th><th>详情</th></tr></thead><tbody>{items.map((event, index) => <tr key={event.id || index}><td>{date(event.time)}</td><td><span className={'badge ' + (String(event.level).toLowerCase() === 'error' ? 'bad' : 'muted')}>{event.level || 'info'}</span></td><td>{event.action}</td><td>{event.node || '—'}</td><td>{event.message}</td></tr>)}</tbody></table></div> : <Empty>暂无审计记录。</Empty>
}
export function StorageGuide() {
  return <><Section title="从物理磁盘到业务持久卷"><ol className="storage-guide"><li><strong>让节点加入 Kubernetes</strong><p>所有 Node 都显示在节点列表中，加入集群与启用存储 Agent 是两个步骤。</p></li><li><strong>在节点详情启用 Agent</strong><p>服务核验节点身份并启用选择标签。Agent DaemonSet 需先由管理员部署；标签变更不等于 Pod 已就绪。</p></li><li><strong>等待真实磁盘上报</strong><p>Agent 读取宿主设备，系统盘、分区盘、挂载盘和有签名的盘受到保护。上报超过有效期后禁止生成或执行纳管计划。</p><pre>lsblk -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINTS,SERIAL,WWN{'\n'}ls -l /dev/disk/by-id/{'\n'}findmnt</pre></li><li><strong>预览并确认存储池计划</strong><p>逐项核对稳定路径、序列号与容量。每盘创建独立 DiskPool；Agent 再次核验设备身份。真实初始化仍需批准状态及写盘开关，不会自动擦除已有数据。</p></li><li><strong>检查自动压测并完成人工审核</strong><p>Agent 在专用临时 LV 上进行单盘压测，记录实际读写指标并核验清理结果。在存储池详情核对报告，审核通过后才允许分配业务卷。失败、中断或清理未确认时保持禁止分配。</p></li><li><strong>为业务申请 PVC</strong><p>StorageClass 使用 WaitForFirstConsumer，等待使用 PVC 的 Pod 参与调度，再由 CSI 在选定节点的 VG 中创建 LV。</p></li><li><strong>由 CSI 与 kubelet 提供卷</strong><p>CSI 负责逻辑卷、文件系统、挂载和扩容。原始物理盘无需先挂到 /mnt。Retain 释放 PVC 后保留数据和容量占用。</p></li></ol></Section><div className="workspace-columns"><Section title="两个 PV"><p>LVM Physical Volume 是加入 LVM 的物理设备；Kubernetes PersistentVolume 是与 PVC 绑定的持久存储资源。</p></Section><Section title="运行边界"><p>模拟模式不代表真实磁盘或挂载能力。本地卷不提供跨节点复制；应用高可用需要副本和备份。外部 Operator 的 PVC 由所属管理资源维护，Storage 只读观察。</p></Section></div></>
}
