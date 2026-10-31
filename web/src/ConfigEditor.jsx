import React, { useState } from 'react'
import { ActionStatus, useAction } from './ui.jsx'

// ConfigEditor edits the key/value overrides of one configuration file. Saving sends a merge patch: changed
// keys with their value and removed keys as null.
export function ConfigEditor({ file, config, onSave, onChanged }) {
  const initial = Object.entries(config).sort(([a], [b]) => a.localeCompare(b))
  const [rows, setRows] = useState(initial.length ? initial : [['', '']])
  const [state, run] = useAction(onChanged)

  const set = (i, col, v) => setRows(rows.map((r, j) => (j === i ? (col === 0 ? [v, r[1]] : [r[0], v]) : r)))
  const save = e => {
    e.preventDefault()
    const next = {}
    for (const [k, v] of rows) if (k.trim()) next[k.trim()] = v
    const patch = { ...next }
    for (const k of Object.keys(config)) if (!(k in next)) patch[k] = null
    run('Saved; the component restarts to apply it', () => onSave(patch))
  }
  return (
    <form onSubmit={save}>
      <p className="muted small">Entries override <code>{file}</code>. Saving rolls the component's pods one at a time. Keys the operator manages, such as cluster name and metadata addresses, cannot be overridden.</p>
      <table className="kv">
        <thead><tr><th>Key</th><th>Value</th><th></th></tr></thead>
        <tbody>
          {rows.map(([k, v], i) => (
            <tr key={i}>
              <td><input className="mono" value={k} placeholder="e.g. maxMessageSize" onChange={e => set(i, 0, e.target.value)} /></td>
              <td><input className="mono" value={v} onChange={e => set(i, 1, e.target.value)} /></td>
              <td><button type="button" className="ghost" onClick={() => setRows(rows.filter((_, j) => j !== i))}>Remove</button></td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="row">
        <button type="button" onClick={() => setRows([...rows, ['', '']])}>Add entry</button>
        <button className="primary" disabled={state.busy}>Save configuration</button>
        <ActionStatus state={state} />
      </div>
    </form>
  )
}
