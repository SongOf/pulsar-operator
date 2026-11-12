import React, { useState } from 'react'
import { ActionStatus, useAction } from './ui.jsx'
import { configurationBaseline, configurationPatch, configurationRows } from './configuration.js'
// ConfigEditor edits the key/value overrides of one configuration file. Saving sends a merge patch: changed
// keys with their value and removed keys as null.
export function ConfigEditor({ file, config, identity, onSave, onChanged }) {
  const [baseline, setBaseline] = useState(() => configurationBaseline(config, identity))
  const [rows, setRows] = useState(() => configurationRows(config))
  const [state, run, reset] = useAction(onChanged)
  const patch = configurationPatch(baseline.config, rows)
  const dirty = Object.keys(patch).length > 0
  const stale = baseline.identity.uid !== identity.uid || baseline.identity.resourceVersion !== identity.resourceVersion
  const set = (i, col, v) => setRows(rows.map((r, j) => (j === i ? (col === 0 ? [v, r[1]] : [r[0], v]) : r)))
  const save = e => {
    e.preventDefault()
    if (!dirty || state.busy) return
    run('Saved; the component restarts to apply it', async () => {
      const saved = await onSave(patch, baseline.identity)
      setBaseline(configurationBaseline(saved.config, saved.identity))
      setRows(configurationRows(saved.config))
    })
  }
  return (
    <form onSubmit={save}>
      <p className="muted small">Entries override <code>{file}</code>. Saving rolls the component's pods one at a time. Keys the operator manages, such as cluster name and metadata addresses, cannot be overridden.</p>
      {stale && <p className="warn-text" role="status">资源版本已变化，当前草稿已保留。保存会校验原版本；发生冲突时请重新载入最新配置后编辑。</p>}
      <table className="kv">
        <thead><tr><th>Key</th><th>Value</th><th></th></tr></thead>
        <tbody>
          {rows.map(([k, v], i) => (
            <tr key={i}>
              <td><input className="mono" disabled={state.busy} value={k} placeholder="e.g. maxMessageSize" onChange={e => set(i, 0, e.target.value)} /></td>
              <td><input className="mono" disabled={state.busy} value={v} onChange={e => set(i, 1, e.target.value)} /></td>
              <td><button type="button" className="ghost" disabled={state.busy} onClick={() => setRows(rows.filter((_, j) => j !== i))}>Remove</button></td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="row">
        <button type="button" disabled={state.busy} onClick={() => setRows([...rows, ['', '']])}>Add entry</button>
        <button type="button" disabled={state.busy} onClick={() => {
          if (dirty && !confirm('重新载入将放弃未保存的草稿，继续吗？')) return
          setBaseline(configurationBaseline(config, identity)); setRows(configurationRows(config)); reset()
        }}>重新载入最新配置</button>
        <button className="primary" disabled={state.busy || !dirty}>Save configuration</button>
        <ActionStatus state={state} />
      </div>
    </form>
  )
}
