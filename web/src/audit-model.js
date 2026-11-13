const actions = {
  'request.create': '创建集群请求', 'request.patch': '修改集群请求',
  'request.delete': '删除集群请求', 'request.restart': '重启请求',
  'resource.create': '创建资源', 'resource.update': '更新资源',
  'resource.patch': '变更资源', 'resource.delete': '删除资源',
  'status.update': '关键状态变化', 'status.patch': '关键状态变化',
}
export const auditAction = action => actions[action] || action
export function auditResult(record) {
  switch (record.result) {
    case 'Succeeded': return { tone: 'ok', label: record.action.startsWith('request.') ? '请求已接受' : '写入已确认' }
    case 'Failed': return { tone: 'bad', label: '已拒绝 / 失败' }
    case 'Started': return { tone: 'info', label: '处理中 / 待确认' }
    default: return { tone: 'warn', label: '结果待核实' }
  }
}
