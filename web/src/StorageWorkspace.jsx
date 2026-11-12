import React, { useEffect, useRef, useState } from 'react'
import './storage-workspace.css'

export const STORAGE_PAGES = [
  { key: 'overview', label: '存储总览' },
  { key: 'nodes', label: '物理节点' },
  { key: 'disks', label: '物理磁盘' },
  { key: 'pools', label: '存储池' },
  { key: 'volumes', label: 'PV / PVC' },
  { key: 'events', label: '操作审计' },
  { key: 'guide', label: '接盘指南' },
]

export function isStoragePage(value) {
  return STORAGE_PAGES.some(item => item.key === value)
}

// Return only the small, non-sensitive protocol fields understood by the shell.
export function readStorageMessage(event, frameWindow, origin) {
  if (!frameWindow || event.source !== frameWindow || event.origin !== origin) return null
  const data = event.data
  if (!data || data.channel !== 'pulsar.storage' || !isStoragePage(data.page)) return null
  if (data.kind === 'route') return { kind: 'route', page: data.page }
  if (data.kind === 'ready' && typeof data.apiReady === 'boolean') {
    return { kind: 'ready', page: data.page, apiReady: data.apiReady }
  }
  return null
}

export async function checkStorageEndpoint(fetcher, signal) {
  const response = await fetcher('/storage/api/config', {
    headers: { Accept: 'application/json' }, credentials: 'same-origin', cache: 'no-store', signal,
  })
  if (!response.ok) throw new Error(`存储服务返回 HTTP ${response.status}，请检查服务状态。`)
  const contentType = (response.headers.get('content-type') || '').split(';')[0].trim().toLowerCase()
  if (contentType !== 'application/json') throw new Error('存储入口未正确配置，请检查 /storage/ 的服务路由。')
  const config = await response.json()
  if (!config || !['demo', 'cluster'].includes(config.mode) || typeof config.tokenRequired !== 'boolean') {
    throw new Error('存储服务返回了无法识别的配置。')
  }
}

export function StorageWorkspace({ page = 'overview', onPageChange }) {
  const selected = isStoragePage(page) ? page : 'overview'
  const frame = useRef(null)
  const selectedRef = useRef(selected)
  const onPageChangeRef = useRef(onPageChange)
  const handshakeTimer = useRef(null)
  const ready = useRef(false)
  const [attempt, setAttempt] = useState(0)
  const [src, setSrc] = useState(null)
  const [phase, setPhase] = useState('checking')
  const [error, setError] = useState('')
  selectedRef.current = selected
  onPageChangeRef.current = onPageChange

  const syncPage = () => {
    const child = frame.current?.contentWindow
    if (!child) return
    try {
      // Restrict navigation to the verified same-origin document and hash keys.
      if (child.location.origin === window.location.origin && child.location.pathname === '/storage/') {
        const hash = `#${selectedRef.current}`
        if (child.location.hash !== hash) child.location.hash = hash
      }
    } catch { /* A failed or redirected document is handled by the handshake timeout. */ }
  }

  useEffect(() => {
    const receive = event => {
      const message = readStorageMessage(event, frame.current?.contentWindow, window.location.origin)
      if (!message) return
      if (message.kind === 'ready') {
        clearTimeout(handshakeTimer.current)
        if (!message.apiReady) {
          ready.current = false
          setError('存储页面已加载，但管理 API 暂时不可用。')
          setPhase('error')
          return
        }
        setError('')
        ready.current = true
        setPhase('ready')
        syncPage()
      } else if (ready.current && message.page !== selectedRef.current) {
        onPageChangeRef.current?.(message.page)
      }
    }
    window.addEventListener('message', receive)
    return () => {
      window.removeEventListener('message', receive)
      clearTimeout(handshakeTimer.current)
    }
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    let active = true
    ready.current = false
    setSrc(null)
    setPhase('checking')
    setError('')
    clearTimeout(handshakeTimer.current)
    const timeout = setTimeout(() => controller.abort(), 12000)
    checkStorageEndpoint(window.fetch.bind(window), controller.signal).then(() => {
      if (!active) return
      setSrc(`/storage/?embedded=1#${selectedRef.current}`)
      setPhase('loading')
      handshakeTimer.current = setTimeout(() => {
        if (active) {
          setError('存储页面未能完成加载，请检查服务及浏览器的同源嵌入策略后重试。')
          setPhase('error')
        }
      }, 15000)
    }).catch(cause => {
      if (!active) return
      setError(cause.name === 'AbortError' ? '连接存储服务超时，请重试。' : cause.message || '无法连接存储服务。')
      setPhase('error')
    }).finally(() => clearTimeout(timeout))
    return () => {
      active = false
      controller.abort()
      clearTimeout(timeout)
      clearTimeout(handshakeTimer.current)
    }
  }, [attempt])

  useEffect(syncPage, [selected])

  const loaded = () => {
    syncPage()
    // Load alone proves neither a working API nor that this is the storage app.
    frame.current?.contentWindow?.postMessage({ channel: 'pulsar.storage', kind: 'request-state' }, window.location.origin)
  }
  const failed = () => {
    ready.current = false
    clearTimeout(handshakeTimer.current)
    setError('存储页面加载失败，请检查服务状态后重试。')
    setPhase('error')
  }
  const title = STORAGE_PAGES.find(item => item.key === selected).label
  return (
    <section className="storage-workspace" aria-label={`物理存储：${title}`}>
      {phase === 'error' ? (
        <div className="storage-workspace-status storage-workspace-error" role="alert">
          <h2>物理存储暂时不可用</h2>
          <p>{error}</p>
          <button onClick={() => { ready.current = false; setSrc(null); setAttempt(value => value + 1) }}>重新连接</button>
        </div>
      ) : phase !== 'ready' && (
        <div className="storage-workspace-status" role="status">
          <span className="storage-workspace-spinner" aria-hidden="true" />
          <p>{phase === 'checking' ? '正在连接物理存储服务…' : '正在载入存储工作区…'}</p>
        </div>
      )}
      {src && <iframe key={attempt} ref={frame} src={src} title={`物理存储管理 · ${title}`}
        className="storage-workspace-frame" hidden={phase !== 'ready'} onLoad={loaded} onError={failed} />}
    </section>
  )
}
