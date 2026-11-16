import React, { useRef, useState } from 'react'
import { Section } from './ui.jsx'
import { StorageModal } from './StorageDialogs.jsx'
import { StorageBadge, DetailRows, Reasons } from './storage-views.jsx'
import { bytes, date } from './storage-model.js'
import { metric, workloadLabels, poolAllocationReasons, poolReviewReasons, reviewIdentity } from './pool-model.js'

export function BenchmarkReport({ report, mode }) {
  if (!report) return <div className="empty compact">等待 Agent 自动进行单盘压测并上报结果。已有业务数据或其他安全条件不满足时不会自动压测。</div>
  return <>
    {mode === 'demo' && <div className="banner warn">模拟压测报告：下列数值为演示数据，没有对真实磁盘执行 I/O，不能用于性能判断。</div>}
    <div className="inline"><span>压测阶段</span><StorageBadge value={report.phase} /><span className={'badge ' + (report.cleanupVerified === true ? 'ok' : 'warn')}>{report.cleanupVerified === true ? '临时测试卷已清理' : '临时测试卷清理未确认'}</span></div>
    <DetailRows rows={[["测试 profile", report.profile], ["运行 ID", report.runID], ["开始", date(report.startedAt)], ["结束", date(report.completedAt)], ["测试卷大小", typeof report.testSizeBytes === 'number' ? bytes(report.testSizeBytes) : '—'], ["结果摘要", report.resultDigest], ["池 UID", report.poolUID], ["物理节点 UID", report.nodeUID], ["VG", report.vgName], ["设备路径", report.device?.path], ["设备序列号", report.device?.serial], ["物理盘容量", typeof report.device?.sizeBytes === 'number' ? bytes(report.device.sizeBytes) : '—']]} />
    {(report.message || report.errorCode) && <div className={'banner ' + (report.phase === 'Succeeded' ? 'info' : 'warn')} role="status">{report.errorCode && <strong>{report.errorCode} · </strong>}{report.message}</div>}
    {report.workloads?.length ? <div className="table-scroll storage-benchmark-table"><table><thead><tr><th>工作负载</th><th>块大小</th><th>队列深度</th><th>时长（秒）</th><th>I/O 总量</th><th>带宽（MiB/s）</th><th>IOPS（次/秒）</th><th>平均延迟（ms）</th><th>P95 延迟（ms）</th><th>P99 延迟（ms）</th><th>错误数</th></tr></thead><tbody>{report.workloads.map((workload, index) => <tr key={workload.name + '/' + index}><td>{workloadLabels[workload.name] || workload.name}</td><td>{typeof workload.blockSizeBytes === 'number' ? bytes(workload.blockSizeBytes) : '—'}</td><td>{metric(workload.queueDepth, 1, 0)}</td><td>{metric(workload.durationSeconds)}</td><td>{typeof workload.bytes === 'number' ? bytes(workload.bytes) : '—'}</td><td>{metric(workload.bandwidthBytesPerSecond, 1024 ** 2)}</td><td>{metric(workload.iops)}</td><td>{metric(workload.latencyMeanNs, 1e6, 6)}</td><td>{metric(workload.latencyP95Ns, 1e6, 6)}</td><td>{metric(workload.latencyP99Ns, 1e6, 6)}</td><td className={workload.errors > 0 ? 'error-text' : ''}>{metric(workload.errors, 1, 0)}</td></tr>)}</tbody></table></div> : <div className="empty compact">尚无工作负载指标。未上报的指标不会显示为零。</div>}
    <p className="muted small">按实际报告展示每项工作负载的配置与结果；临时测试卷仅用于压测，成功且清理完成后仍需人工审核。</p>
  </>
}

export function PoolDetails({ pool, mode, onReview, onVolume, onNode }) {
  const allocationReasons = poolAllocationReasons(pool)
  const reviewReasons = poolReviewReasons(pool)
  const approval = pool.allocationApproval
  const approved = approval?.decision === 'Approved'
  return <>
    <Section title={pool.name || pool.id} actions={<button className="primary" disabled={allocationReasons.length > 0} onClick={() => onVolume(pool)}>申请 PVC</button>}>
      <div className="inline"><span>存储池</span><StorageBadge value={pool.state} /><span>卷分配</span><StorageBadge value={pool.allocationState || 'Unavailable'} /></div>
      <DetailRows rows={[["物理节点", <button className="link" onClick={() => onNode(pool.node)}>{pool.node}</button>], ["UID", pool.uid], ["资源版本", pool.resourceVersion], ["VG", pool.vgName], ["StorageClass", pool.storageClass || '等待审核通过后提供'], ["总容量 / 可用", `${bytes(pool.capacityBytes)} / ${bytes(pool.availableBytes)}`]]} />
      <Reasons items={allocationReasons} />{pool.message && <p className="muted small">{pool.message}</p>}
    </Section>
    <Section title="单盘压测结果"><BenchmarkReport report={pool.benchmark} mode={mode} /></Section>
    <Section title="人工审核" actions={!approved && <button className="primary" disabled={reviewReasons.length > 0} onClick={() => onReview(pool)}>审核压测结果</button>}>
      <div className="inline"><StorageBadge value={approval?.decision || 'AwaitingReview'} />{approved && <span className="muted small">审核通过后不提供撤回或重复审核。</span>}</div>
      {approval && <DetailRows rows={[["审核时间", date(approval.reviewedAt)], ["审核备注", approval.note], ["审核绑定池 UID", approval.poolUID], ["审核绑定运行", approval.runID], ["审核绑定结果摘要", approval.resultDigest]]} />}
      {!approved && <Reasons items={reviewReasons} />}
      <p className="muted small">审核决定绑定具体存储池和本次压测结果。只有当前结果通过人工审核且服务端确认可分配，才能创建业务卷。</p>
    </Section>
  </>
}

export function PoolReviewDialog({ pool, currentPool, api, mode, onClose, onDone, onRefresh }) {
  const [reviewed] = useState(() => structuredClone(pool))
  const [expected] = useState(() => reviewIdentity(pool))
  const [decision, setDecision] = useState('Approved')
  const [note, setNote] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const lock = useRef(false)
  const reasons = poolReviewReasons(currentPool, expected)
  const disabled = reasons.length > 0 || !confirmed || decision === 'Rejected' && !note.trim()
  const submit = async event => {
    event.preventDefault()
    if (lock.current || disabled) return
    lock.current = true; setBusy(true); setError('')
    try {
      // Always compare to the frozen review, even if the user disabled polling.
      const latest = await api.pool(reviewed.id)
      const blockers = poolReviewReasons(latest, expected)
      if (blockers.length) throw new Error(blockers.join('；'))
      const result = await api.reviewPool(reviewed.id, expected, decision, note)
      onDone(result)
    } catch (error) { setError(error.message); onRefresh?.() }
    finally { lock.current = false; setBusy(false) }
  }
  return <StorageModal title="审核单盘压测结果" busy={busy} onClose={onClose}><form className="storage-form" onSubmit={submit}>
    <div className="banner info">本次审核固定为 {reviewed.name || reviewed.id} 的当前报告。后台状态或结果变化后必须关闭并重新审核；不会自动替换正在确认的报告。</div>
    <DetailRows rows={[["池 UID", expected.uid], ["资源版本", expected.resourceVersion], ["运行 ID", expected.runID], ["结果摘要", expected.resultDigest]]} />
    <BenchmarkReport report={reviewed.benchmark} mode={mode} />
    <Reasons items={reasons} />{error && <div className="banner error" role="alert">{error}</div>}
    <fieldset disabled={busy || reasons.length > 0}><label>审核决定<select value={decision} onChange={event => setDecision(event.target.value)}><option value="Approved">通过：允许此结果进入业务卷分配</option><option value="Rejected">拒绝：保留结果，继续禁止分配</option></select></label><label>{decision === 'Rejected' ? '拒绝理由（必填）' : '审核备注（可选）'}<textarea rows={3} maxLength={1024} value={note} required={decision === 'Rejected'} onChange={event => setNote(event.target.value)} /></label><label className="check"><input type="checkbox" required checked={confirmed} onChange={event => setConfirmed(event.target.checked)} />我已核对设备身份、各项指标和临时测试卷清理结果。</label></fieldset>
    <div className="row end"><button type="button" disabled={busy} onClick={onClose}>取消</button><button type="submit" className={decision === 'Rejected' ? 'danger' : 'primary'} disabled={busy || disabled}>{busy ? '正在提交…' : decision === 'Rejected' ? '提交拒绝理由' : '确认审核通过'}</button></div>
  </form></StorageModal>
}
