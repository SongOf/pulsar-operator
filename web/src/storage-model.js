export const statusLabels = { Available: '可接管', Managed: '已纳管', Protected: '受保护', Offline: '离线', Ready: '就绪', NotReady: '未就绪', Pending: '等待中', Bound: '已绑定', Released: '已保留', ReleaseRequested: '已请求释放', Releasing: '等待卸载', Deleted: '已删除', Error: '异常', Failed: '失败', Conflict: '认领冲突', Online: '在线', Retained: '已保留', Active: '在线', NotInstalled: '未部署', NotEnrolled: '未启用', Disabled: '未启用', Running: '运行中', Stale: '上报过期', Unknown: '未知', Unavailable: '不可用', Unsupported: '不支持', Starting: '启动中', AwaitingBenchmark: '等待压测', Benchmarking: '压测中', AwaitingReview: '等待人工审核', Succeeded: '成功', Interrupted: '已中断', Blocked: '受阻', Approved: '审核通过', Rejected: '审核拒绝', PendingBenchmark: '等待压测', LegacyUnreviewed: '历史池未审核', BenchmarkFailed: '压测失败', AwaitingStorageClass: '等待存储类' }
export const statusTone = value => ['Available', 'Managed', 'Ready', 'Bound', 'Online', 'Active', 'Running', 'Succeeded', 'Approved'].includes(value) ? 'ok' : ['Error', 'Offline', 'Failed', 'Conflict', 'NotReady', 'Stale', 'Rejected', 'Blocked', 'Interrupted', 'BenchmarkFailed'].includes(value) ? 'bad' : ['Pending', 'ReleaseRequested', 'Releasing', 'Starting', 'AwaitingReview', 'AwaitingBenchmark', 'Benchmarking', 'PendingBenchmark', 'LegacyUnreviewed', 'AwaitingStorageClass'].includes(value) ? 'warn' : 'muted'
export function bytes(value) {
  const n = Number(value)
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  const power = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1)
  return `${(n / 1024 ** power).toLocaleString('zh-CN', { maximumFractionDigits: 1 })} ${units[power]}`
}
export const count = value => Number(value || 0).toLocaleString('zh-CN')
export const percent = (used, total) => total > 0 ? Math.min(100, Math.max(0, Math.round(Number(used) / Number(total) * 100))) : 0
export function date(value) {
  const d = new Date(value)
  return value && Number.isFinite(d.getTime()) ? d.toLocaleString('zh-CN', { hour12: false }) : '—'
}
export const volumeMessages = volume => [...new Set([volume.bindingMessage, volume.message].filter(value => typeof value === 'string' && value))]
export function volumeReadOnlyReason(volume, mode = 'cluster') {
  if (volume.management === 'external' || (volume.owners || []).some(owner => owner.controller !== false)) return '此 PVC 由外部 Operator 管理，请通过所属管理资源操作。'
  const messages = volumeMessages(volume)
  if (messages.length) return messages.join('；')
  if (['Released', 'Retained', 'ReleaseRequested', 'Releasing', 'Deleted'].includes(volume.status)) return '卷正在释放或已经保留，不再接受扩容或重复释放。'
  if (volume.canManage === false || (mode === 'cluster' && volume.canManage !== true)) return '此卷的身份或回收状态尚未确认，暂时只读。'
  return ''
}
export function diskEligibility(disk, now = Date.now(), mode = 'cluster') {
  const reasons = []
  if (disk.state !== 'Available') reasons.push(...(disk.reasons || []), `磁盘状态为${statusLabels[disk.state] || disk.state || '未知'}。`)
  if (disk.canCreatePool === false || mode === 'cluster' && disk.canCreatePool !== true) reasons.push(...(disk.reasons || []), '物理节点、Agent 或设备尚未满足接管条件。')
  if (mode === 'cluster') {
    if (disk.fresh !== true) reasons.push('Agent 尚未提供有效的本次磁盘扫描。')
    const deadline = Date.parse(disk.validUntil)
    if (!Number.isFinite(deadline) || now >= deadline) reasons.push('磁盘上报已过期或缺少有效期，请等待 Agent 重新发现。')
  }
  return [...new Set(reasons)]
}
export function observedDisk(original, latest = original, mode = 'cluster') {
  if (mode === 'cluster' && ['uid', 'nodeUID', 'node', 'path', 'serial', 'sizeBytes'].some(key => latest[key] !== original[key])) {
    return { ...latest, state: 'Protected', canCreatePool: false, reasons: ['磁盘或物理节点身份已变化，请关闭并重新选择设备、生成计划。'] }
  }
  return latest
}
export function planEligibility(entry, now = Date.now(), mode = 'cluster', latest = entry.disk) {
  const reasons = diskEligibility(observedDisk(entry.disk, latest, mode), now, mode)
  const deadline = Date.parse(entry.plan.expiresAt)
  if (!Number.isFinite(deadline) || now >= deadline) reasons.push('计划已过期，请关闭后重新生成。')
  return reasons
}
export function poolPlanBody(disk, prefix, index) {
  const suffix = String(disk.id).toLowerCase().replace(/[^a-z0-9]/g, '').slice(-8) || String(index + 1)
  return { name: `${prefix}-${index + 1}-${suffix}`, node: disk.node, diskIds: [disk.id] }
}
