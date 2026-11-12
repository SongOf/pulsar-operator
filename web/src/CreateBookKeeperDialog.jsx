import React, { useState } from 'react'
import { api } from './api.js'
import { DEFAULT_IMAGE } from './model.js'

export function CreateBookKeeperDialog({ onClose, onCreated }) {
  const [form, setForm] = useState({ name: '', namespace: 'bookkeeper', zookeeper: '', image: DEFAULT_IMAGE, replicas: 3, minReplicas: 3, journal: '10Gi', ledger: '50Gi', journalClass: '', ledgerClass: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const field = (name, label, options = {}) => <label>{label}<input value={form[name]} onChange={e => setForm({ ...form, [name]: e.target.value })} {...options} /></label>
  const submit = async event => {
    event.preventDefault(); setBusy(true); setError(null)
    try {
      if (+form.minReplicas > +form.replicas) throw new Error('最小副本数不能超过初始副本数')
      const volume = (size, storageClassName) => ({ size, ...(storageClassName ? { storageClassName } : {}) })
      const obj = { apiVersion: 'pulsar.songof.dev/v1alpha1', kind: 'BookKeeperCluster', metadata: { name: form.name, namespace: form.namespace }, spec: {
        zookeeper: form.zookeeper, image: form.image, replicas: +form.replicas, minReplicas: +form.minReplicas,
        storage: { journal: volume(form.journal, form.journalClass), ledger: volume(form.ledger, form.ledgerClass), reclaimPolicy: 'Retain' },
      } }
      await api.create('BookKeeperCluster', obj)
      onCreated('BookKeeperCluster/' + form.namespace + '/' + form.name)
    } catch (e) { setError(e.message); setBusy(false) }
  }
  return <div className="overlay" onClick={onClose}><form className="card dialog" onClick={e => e.stopPropagation()} onSubmit={submit}>
    <h2>新建独立 BookKeeper 集群</h2><p className="muted">连接已有 ZooKeeper。此集群独立管理，不会自动绑定到已有 Pulsar 集群。</p>
    <div className="grid2">{field('name', '名称', { required: true, pattern: '[a-z0-9]([-a-z0-9]*[a-z0-9])?' })}{field('namespace', '命名空间（须已存在）', { required: true })}{field('zookeeper', 'ZooKeeper 地址（host:port，逗号分隔）', { required: true })}{field('image', '镜像', { required: true })}{field('replicas', '初始 Bookie 数', { type: 'number', min: 1, required: true })}{field('minReplicas', '最小 Bookie 数', { type: 'number', min: 0, required: true })}{field('journal', 'Journal 容量', { required: true })}{field('journalClass', 'Journal StorageClass（空为默认）')}{field('ledger', 'Ledger 容量', { required: true })}{field('ledgerClass', 'Ledger StorageClass（空为默认）')}</div>
    {error && <div className="banner error" role="alert">{error}</div>}<div className="row end"><button type="button" onClick={onClose}>取消</button><button className="primary" disabled={busy}>{busy ? '创建中…' : '创建集群'}</button></div>
  </form></div>
}
