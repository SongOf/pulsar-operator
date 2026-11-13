import React, { useState } from 'react'

export function TokenForm({ onSave }) {
  const [value, setValue] = useState('')
  return <form className="token-form" onSubmit={e => { e.preventDefault(); onSave(value) }}><h2>连接 Pulsar 控制台</h2><p className="muted">输入管理员提供的 Pulsar 访问凭证。物理存储的凭证在存储页面单独设置。</p><label>访问凭证<input type="password" autoComplete="off" value={value} required onChange={e => setValue(e.target.value)} /></label><button className="primary">连接集群</button></form>
}
