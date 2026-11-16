const workloadNames = ['seq-write', 'seq-read', 'rand-write', 'rand-read']
const positive = value => typeof value === 'number' && Number.isFinite(value) && value > 0
const nonnegative = value => typeof value === 'number' && Number.isFinite(value) && value >= 0

// Server capabilities are authoritative. The client also binds every action
// to the actual report being displayed, never merely to the pool's Ready phase.
export function benchmarkReasons(pool) {
  const report = pool?.benchmark
  if (!report) return ['尚未收到单盘压测结果。']
  const reasons = []
  if (report.phase !== 'Succeeded') reasons.push('压测尚未成功完成。')
  if (report.cleanupVerified !== true) reasons.push('压测临时卷清理尚未确认。')
  if (!pool.uid || report.poolUID !== pool.uid || !report.nodeUID || !report.runID || !report.resultDigest || report.vgName !== pool.vgName) reasons.push('压测结果与当前存储池身份不匹配或缺失。')
  if (pool.nodeUID && report.nodeUID !== pool.nodeUID) reasons.push('压测物理节点与当前物理节点身份不匹配。')
  if (Array.isArray(pool.devices) && (pool.devices.length !== 1 || ['path', 'serial', 'sizeBytes'].some(key => pool.devices[0]?.[key] !== report.device?.[key]))) reasons.push('压测设备与当前存储池认领的设备不匹配。')
  if (report.profile !== 'temporary-lv-v1' || !positive(report.testSizeBytes) || !Number.isFinite(Date.parse(report.startedAt)) || !Number.isFinite(Date.parse(report.completedAt))) reasons.push('压测配置或执行时间不完整。')
  if (!report.device?.path || !report.device?.serial || !positive(report.device?.sizeBytes)) reasons.push('压测设备身份不完整。')
  const workloads = report.workloads
  if (!Array.isArray(workloads) || workloads.length !== workloadNames.length || workloadNames.some(name => workloads.filter(item => item.name === name).length !== 1)) {
    reasons.push('压测缺少完整的顺序读写与随机读写报告。')
  } else if (workloads.some(workload => !positive(workload.blockSizeBytes) || !positive(workload.queueDepth) || !positive(workload.durationSeconds) || !positive(workload.bytes) || !nonnegative(workload.bandwidthBytesPerSecond) || !nonnegative(workload.iops) || !nonnegative(workload.latencyMeanNs) || !nonnegative(workload.latencyP95Ns) || !nonnegative(workload.latencyP99Ns) || workload.errors !== 0)) {
    reasons.push('压测指标缺失、无效或包含 I/O 错误。')
  }
  return reasons
}

export function poolAllocationReasons(pool) {
  if (!pool) return ['尚未确认存储池。']
  const reasons = benchmarkReasons(pool)
  if (pool.canCreateVolume !== true) reasons.push(...(pool.allocationReasons || []), '此池尚未允许分配业务卷。')
  const approval = pool.allocationApproval
  const report = pool.benchmark
  if (approval?.decision !== 'Approved') reasons.push('压测结果尚未通过人工审核。')
  else if (!report || approval.poolUID !== pool.uid || approval.runID !== report.runID || approval.resultDigest !== report.resultDigest) reasons.push('审核结果与当前压测运行不匹配，请核对最新结果。')
  return [...new Set(reasons)]
}

export function reviewIdentity(pool) {
  return Object.freeze({ uid: pool?.uid, resourceVersion: pool?.resourceVersion, runID: pool?.benchmark?.runID, resultDigest: pool?.benchmark?.resultDigest })
}

export function poolReviewReasons(pool, expected) {
  if (!pool) return ['无法确认最新存储池状态，请刷新后重新打开审核。']
  const reasons = benchmarkReasons(pool)
  if (pool.canReview !== true) reasons.push('当前压测结果不可审核。')
  if (pool.allocationApproval?.decision === 'Approved') reasons.push('此结果已经审核通过，不支持撤回或再次审核。')
  const current = reviewIdentity(pool)
  if (Object.values(current).some(value => typeof value !== 'string' || !value)) reasons.push('存储池或压测身份、资源版本缺失。')
  if (expected && Object.keys(current).some(key => current[key] !== expected[key])) reasons.push('存储池版本或压测结果已变化，请关闭并重新打开审核。')
  return [...new Set(reasons)]
}

export const workloadLabels = { 'seq-write': '顺序写', 'seq-read': '顺序读', 'rand-write': '随机写', 'rand-read': '随机读' }
export function metric(value, divisor = 1, digits = 2) {
  if (!nonnegative(value)) return '—'
  return (value / divisor).toLocaleString('zh-CN', { maximumFractionDigits: digits })
}
