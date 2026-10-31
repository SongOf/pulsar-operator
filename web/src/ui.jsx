import React from 'react'

const phaseTone = {
  Running: 'ok', Initializing: 'info', Updating: 'info', Scaling: 'info', Decommissioning: 'warn', Degraded: 'warn', Failed: 'bad',
}

export function PhaseBadge({ phase }) {
  return <span className={'badge ' + (phaseTone[phase] || 'muted')}>{phase || 'Pending'}</span>
}

export function Section({ title, actions, children }) {
  return (
    <section className="card">
      <header className="card-head"><h3>{title}</h3>{actions}</header>
      {children}
    </section>
  )
}

export function Ready({ ready, total }) {
  const tone = total === 0 ? 'muted' : ready === total ? 'ok' : ready === 0 ? 'bad' : 'warn'
  return <span className={'ready ' + tone}>{ready ?? 0}/{total ?? 0}</span>
}

// useAction runs a change and reports its outcome next to the control.
export function useAction(onChanged) {
  const [state, setState] = React.useState({ busy: false, error: null, done: null })
  const run = async (label, fn) => {
    setState({ busy: true, error: null, done: null })
    try {
      await fn()
      setState({ busy: false, error: null, done: label })
      onChanged()
    } catch (e) {
      setState({ busy: false, error: e.message, done: null })
    }
  }
  return [state, run]
}

export function ActionStatus({ state }) {
  if (state.busy) return <span className="muted small">Applying…</span>
  if (state.error) return <span className="error-text small">{state.error}</span>
  if (state.done) return <span className="ok-text small">{state.done}</span>
  return null
}
