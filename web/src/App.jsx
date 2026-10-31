import React, { useCallback, useEffect, useState } from 'react'
import { api, ApiError, setToken } from './api.js'
import { bookkeeperView, pulsarView } from './model.js'
import { ClusterView } from './ClusterView.jsx'
import { CreateDialog } from './CreateDialog.jsx'
import { PhaseBadge } from './ui.jsx'

export function App() {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [needToken, setNeedToken] = useState(false)
  const [selected, setSelected] = useState(null)
  const [creating, setCreating] = useState(false)

  const refresh = useCallback(async () => {
    try {
      setData(await api.list())
      setError(null)
      setNeedToken(false)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setNeedToken(true)
      else setError(e.message)
    }
  }, [])

  useEffect(() => {
    refresh()
    const t = setInterval(refresh, 5000)
    return () => clearInterval(t)
  }, [refresh])

  const views = data ? [
    ...data.pulsar.map(pc => pulsarView(pc, data.children[`${pc.metadata.namespace}/${pc.metadata.name}`])),
    ...data.bookkeeper.map(bookkeeperView),
  ] : []
  const keyOf = v => `${v.kind}/${v.ns}/${v.name}`
  const current = views.find(v => keyOf(v) === selected) || views[0]

  if (needToken) return <TokenPrompt onSave={t => { setToken(t); refresh() }} />

  return (
    <div className="layout">
      <aside>
        <div className="brand">Pulsar Operator</div>
        <button className="primary wide" onClick={() => setCreating(true)}>New Pulsar cluster</button>
        <nav>
          {views.length === 0 && data && <p className="muted">No clusters yet.</p>}
          {views.map(v => (
            <button key={keyOf(v)} className={'nav-item' + (current && keyOf(current) === keyOf(v) ? ' active' : '')} onClick={() => setSelected(keyOf(v))}>
              <span className="nav-name">{v.name}</span>
              <span className="nav-meta">{v.kind === 'PulsarCluster' ? 'Pulsar' : 'BookKeeper'} · {v.ns}</span>
              <PhaseBadge phase={v.phase} />
            </button>
          ))}
        </nav>
      </aside>
      <main>
        {error && <div className="banner error">Cannot reach the operator: {error}</div>}
        {!data && !error && <p className="muted">Loading…</p>}
        {current && <ClusterView key={keyOf(current)} view={current} onChanged={refresh} onDeleted={() => { setSelected(null); refresh() }} />}
        {data && views.length === 0 && <div className="empty"><h2>No clusters</h2><p>Create a Pulsar cluster to get started.</p></div>}
      </main>
      {creating && <CreateDialog onClose={() => setCreating(false)} onCreated={key => { setCreating(false); setSelected(key); refresh() }} />}
    </div>
  )
}

function TokenPrompt({ onSave }) {
  const [t, setT] = useState('')
  return (
    <div className="center">
      <form className="card narrow" onSubmit={e => { e.preventDefault(); onSave(t) }}>
        <h2>Console token</h2>
        <p className="muted">This operator requires the token set in its CONSOLE_TOKEN environment variable.</p>
        <input type="password" value={t} onChange={e => setT(e.target.value)} autoFocus />
        <button className="primary">Continue</button>
      </form>
    </div>
  )
}
