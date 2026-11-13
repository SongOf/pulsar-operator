import React, { useEffect, useRef, useState } from 'react'
import { api, ApiError, setToken } from './api.js'
import { TokenForm } from './PulsarTokenForm.jsx'
import { auditAction, auditResult } from './audit-model.js'

const emptyFilters = { namespace: '', name: '', uid: '', operationId: '', source: '' }
const display = value => value === undefined ? '—' : typeof value === 'string' ? value : JSON.stringify(value)

export function PulsarAudit() {
  const [draft, setDraft] = useState(emptyFilters)
  const [filters, setFilters] = useState(emptyFilters)
  const [items, setItems] = useState([])
  const [cursor, setCursor] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [needToken, setNeedToken] = useState(false)
  const [credentials, setCredentials] = useState(false)
  const version = useRef(0)

  const load = async (query, next = 0) => {
    const request = ++version.current
    setBusy(true); setError('')
    if (!next) { setItems([]); setCursor(0) }
    try {
      const page = await api.audit(query, next)
      if (request !== version.current) return
      if (!Array.isArray(page.items)) throw new Error('审计 API 返回了无法识别的数据。')
      setItems(old => next ? [...old, ...page.items] : page.items)
      setCursor(page.nextCursor || 0); setNeedToken(false)
    } catch (cause) {
      if (request !== version.current) return
      if (cause instanceof ApiError && cause.status === 401) { setNeedToken(true); setItems([]); setCursor(0) }
      else setError(cause.message)
    } finally {
      if (request === version.current) setBusy(false)
    }
  }
  useEffect(() => { load(emptyFilters); return () => { version.current++ } }, [])
  const search = query => { setDraft(query); setFilters(query); load(query) }
  const connect = token => { setToken(token); setCredentials(false); load(filters) }

  return <>
    <div className="workspace-toolbar"><span className="workspace-service">Pulsar 操作追溯</span><div className="inline"><button onClick={() => setCredentials(true)}>Pulsar 凭证</button><button onClick={() => load(filters)} disabled={busy}>刷新</button></div></div>
    <div className="workspace-heading"><div><div className="eyebrow">PULSAR / AUDIT</div><h1>操作审计</h1><p>查看用户请求、资源写入和控制器关键状态变化，按操作 ID 追踪完整执行链路。</p></div></div>
    <p className="muted">请求被接受不代表集群已经就绪。共享凭证以指纹标识；配置及凭证等敏感值已脱敏。结果待核实时，请先检查资源现状。</p>
    {needToken ? <TokenForm onSave={connect} /> : <>
      <form className="card audit-filters" onSubmit={event => { event.preventDefault(); search(draft) }}>
        {[["namespace", "命名空间"], ["name", "资源名称（精确）"], ["uid", "资源 / 集群 UID"], ["operationId", "操作 ID"]].map(([key, label]) => <label key={key}>{label}<input value={draft[key]} maxLength={253} onChange={event => setDraft({ ...draft, [key]: event.target.value })} /></label>)}
        <label>来源<select value={draft.source} onChange={event => setDraft({ ...draft, source: event.target.value })}><option value="">全部</option><option value="api">用户 API</option><option value="controller">控制器</option></select></label>
        <div className="inline"><button className="primary" disabled={busy}>查询</button><button type="button" onClick={() => search(emptyFilters)} disabled={busy}>重置</button></div>
      </form>
      {error && <div className="banner error" role="alert">读取审计失败：{error}</div>}
      <div className="card audit-table"><table><thead><tr><th>时间 / 操作者</th><th>动作 / 来源</th><th>对象</th><th>结果</th><th>追溯</th></tr></thead><tbody>
        {items.map(record => {
          const result = auditResult(record)
          return <tr key={record.id}>
            <td><time dateTime={record.startedAt}>{new Date(record.startedAt).toLocaleString()}</time><div className="muted small audit-identity">{record.actor}</div>{record.clientIP && <div className="muted small">{record.clientIP}</div>}</td>
            <td>{auditAction(record.action)}<div className="muted small">{record.source === 'api' ? '用户 API' : '控制器'}</div></td>
            <td>{record.resource.kind}<div className="audit-identity">{record.resource.namespace} / {record.resource.name || '尚未解析资源'}</div></td>
            <td><span className={'badge ' + result.tone}>{result.label}</span>{record.httpStatus > 0 && <div className="muted small">HTTP {record.httpStatus}</div>}</td>
            <td><details><summary>查看详情</summary><div className="audit-detail">
              <p className="audit-identity">操作 ID：{record.operationId}</p><button onClick={() => search({ ...emptyFilters, operationId: record.operationId })}>查看同次操作</button>
              <p className="audit-identity">记录 ID：{record.id}<br />资源 UID：{record.resource.uid || '—'}<br />集群 UID：{record.clusterUID || '—'}<br />写入前版本：{record.previousVersion || '—'}<br />预期版本：{record.expectedVersion || '—'}<br />响应版本：{record.result === 'Succeeded' ? record.resource.resourceVersion || '—' : '未确认'}{record.observedUID && record.observedUID !== record.resource.uid && <><br />写入前观察到的 UID：{record.observedUID}</>}</p>
              {record.error && <p role="note">{record.error}</p>}
              {record.changes?.length > 0 ? <ul className="audit-changes">{record.changes.map(change => <li key={change.path}><code>{change.path}</code><div><code>{display(change.before)}</code> → <code>{display(change.after)}</code>{change.redacted && <span className="muted">（值已脱敏）</span>}</div></li>)}</ul> : <p className="muted">请求记录的具体变更见同次操作的资源写入记录。</p>}
              {record.truncated && <p className="muted">变更字段较多，此记录保留前 100 项。</p>}
            </div></details></td>
          </tr>
        })}
      </tbody></table>{!items.length && <p className="empty">{busy ? '正在读取审计记录…' : error ? '审计记录暂不可用。' : '没有符合条件的操作记录。'}</p>}</div>
      {cursor > 0 && <button onClick={() => load(filters, cursor)} disabled={busy}>{busy ? '正在加载…' : '加载更早的记录'}</button>}
    </>}
    {credentials && <div className="overlay"><div className="card dialog token-dialog"><TokenForm onSave={connect} /><button onClick={() => setCredentials(false)}>取消</button></div></div>}
  </>
}
