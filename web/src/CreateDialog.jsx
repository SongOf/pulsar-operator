import React, { useState } from 'react'
import { api } from './api.js'
import { newPulsarCluster } from './model.js'

const defaults = {
  name: '', namespace: 'pulsar', image: 'apachepulsar/pulsar:4.0.14',
  zkReplicas: 3, zkSize: '5Gi', bookies: 3, minBookies: 3, journal: '10Gi', ledger: '50Gi', storageClass: '', reclaim: 'Retain',
  brokers: 2, proxy: false, proxyServiceType: 'ClusterIP',
}

export function CreateDialog({ onClose, onCreated }) {
  const [f, setF] = useState(defaults)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const field = (k, label, type = 'text', extra = {}) => (
    <label>{label}<input type={type} value={f[k]} onChange={e => setF({ ...f, [k]: e.target.value })} {...extra} /></label>
  )
  const submit = async e => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.create('PulsarCluster', newPulsarCluster(f))
      onCreated(`PulsarCluster/${f.namespace}/${f.name}`)
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }
  return (
    <div className="overlay" onClick={onClose}>
      <form className="card dialog" onClick={e => e.stopPropagation()} onSubmit={submit}>
        <h2>New Pulsar cluster</h2>
        <div className="grid2">
          {field('name', 'Name', 'text', { required: true, pattern: '[a-z0-9]([-a-z0-9]*[a-z0-9])?', autoFocus: true })}
          {field('namespace', 'Namespace (must exist)', 'text', { required: true })}
          {field('image', 'Image')}
          {field('storageClass', 'StorageClass (empty = default)')}
        </div>
        <h4>ZooKeeper</h4>
        <div className="grid2">{field('zkReplicas', 'Servers (odd, fixed)', 'number', { min: 1 })}{field('zkSize', 'Data volume')}</div>
        <h4>BookKeeper</h4>
        <div className="grid2">
          {field('bookies', 'Bookies', 'number', { min: 1 })}{field('minBookies', 'Minimum bookies', 'number', { min: 0 })}
          {field('journal', 'Journal PVC')}{field('ledger', 'Ledger PVC')}
          <label>PVCs when the cluster is deleted<select value={f.reclaim} onChange={e => setF({ ...f, reclaim: e.target.value })}><option>Retain</option><option>Delete</option></select></label>
        </div>
        <h4>Brokers and proxy</h4>
        <div className="grid2">
          {field('brokers', 'Brokers', 'number', { min: 0 })}
          <label className="check"><input type="checkbox" checked={f.proxy} onChange={e => setF({ ...f, proxy: e.target.checked })} /> Proxy</label>
          {f.proxy && <label>Proxy service<select value={f.proxyServiceType} onChange={e => setF({ ...f, proxyServiceType: e.target.value })}><option>ClusterIP</option><option>NodePort</option><option>LoadBalancer</option></select></label>}
        </div>
        {error && <div className="banner error">{error}</div>}
        <div className="row end">
          <button type="button" onClick={onClose}>Cancel</button>
          <button className="primary" disabled={busy}>{busy ? 'Creating…' : 'Create'}</button>
        </div>
      </form>
    </div>
  )
}
