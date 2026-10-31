import React, { useEffect, useState } from 'react'
import { api, ApiError } from './api.js'
import { LineChart, StatTile, bytes, slotOf, units } from './charts.jsx'
import { ActionStatus, Section, useAction } from './ui.jsx'

const ranges = ['15m', '1h', '6h', '24h', '7d']

// perEntity turns {entity: points} into chart series with stable colors.
function perEntity(entities = {}, label = n => n) {
  return Object.keys(entities || {}).filter(n => entities[n]?.length).sort().map((name, i) => ({ name: label(name), slot: slotOf(name, i), points: entities[name] }))
}

function cluster(d, metric, name, slot) {
  const pts = d.series?.[metric]?.cluster
  return pts?.length ? [{ name, slot, points: pts }] : []
}

const short = pod => pod.replace(/^.*-(bookie|zookeeper)-/, '$1-')

export function Monitoring({ view, patch, onChanged }) {
  const spec = view.obj.spec.monitoring || {}
  const enabled = spec.enabled || !!spec.prometheusURL
  const status = view.obj.status?.monitoring || {}
  const [range, setRange] = useState('1h')
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [state, run] = useAction(onChanged)

  useEffect(() => {
    if (!enabled) return
    let live = true
    const load = () => api.metrics(view.kind, view.ns, view.name, range)
      .then(d => { if (live) { setData(d); setError(null) } })
      .catch(e => { if (live) setError(e instanceof ApiError ? e.message : String(e)) })
    load()
    const t = setInterval(load, 15000)
    return () => { live = false; clearInterval(t) }
  }, [enabled, view.kind, view.ns, view.name, range])

  if (view.managed) return null
  if (!enabled) {
    return (
      <Section title="Monitoring">
        <p className="muted small">Monitoring deploys a Prometheus for this cluster that scrapes every component. Its samples live on a
          persistent volume (20Gi, kept 15 days and capped at 80% of the disk), so they survive restarts. You can instead point it at
          an existing Prometheus in <code>spec.monitoring.prometheusURL</code>.</p>
        <button className="primary" disabled={state.busy} onClick={() => run('Monitoring enabled', () => patch({ monitoring: { enabled: true } }))}>Enable monitoring</button>
        <ActionStatus state={state} />
      </Section>
    )
  }

  const c = data?.current || {}
  const step = data?.stepSeconds || 15
  const pods = data?.pods || []
  const down = pods.filter(p => !p.up)
  return (
    <Section title="Monitoring" actions={
      <div className="tabs" role="tablist" aria-label="Time range">
        {ranges.map(r => <button key={r} className={r === range ? 'tab active' : 'tab'} onClick={() => setRange(r)}>{r}</button>)}
      </div>
    }>
      {!status.ready && !spec.prometheusURL && <div className="banner info small">Prometheus is starting; charts fill in as it scrapes.</div>}
      {error && <div className="banner warn small">{error}</div>}
      {data?.errors?.length > 0 && <div className="banner warn small">Some panels failed: {data.errors.join('; ')}</div>}
      {down.length > 0 && <div className="banner warn small">Not scraped: {down.map(p => p.pod).join(', ')}</div>}

      {view.kind === 'PulsarCluster' && (
        <div className="tiles">
          <StatTile label="Messages in" value={c.rateIn} unit="rate" />
          <StatTile label="Messages out" value={c.rateOut} unit="rate" />
          <StatTile label="Throughput in" value={c.throughputIn} unit="bytesRate" />
          <StatTile label="Throughput out" value={c.throughputOut} unit="bytesRate" />
          <StatTile label="Backlog" value={c.backlog} unit="count" hint="messages" />
          <StatTile label="Storage" value={c.storageSize} unit="bytes" />
          <StatTile label="Topics" value={c.topics} unit="count" hint={c.producers !== undefined ? `${c.producers} producers · ${c.consumers ?? 0} consumers` : null} />
          <StatTile label="Writable bookies" value={c.writableBookies} unit="count" hint={c.bookies !== undefined ? `of ${c.bookies}` : null}
            tone={c.bookies !== undefined && c.writableBookies < c.bookies ? 'warn' : null} />
        </div>
      )}
      {view.kind === 'BookKeeperCluster' && (
        <div className="tiles">
          <StatTile label="Writable bookies" value={c.writableBookies} unit="count" hint={c.bookies !== undefined ? `of ${c.bookies}` : null} />
          <StatTile label="Highest disk usage" value={c.maxDiskUsage} unit="percent" tone={c.maxDiskUsage > 80 ? 'warn' : null} />
        </div>
      )}

      <div className="charts">
        {view.kind === 'PulsarCluster' && <>
          <LineChart title="Messages per second" unit="rate" step={step} series={[...cluster(data || {}, 'rateIn', 'in', 1), ...cluster(data || {}, 'rateOut', 'out', 2)]} />
          <LineChart title="Throughput" unit="bytesRate" step={step} series={[...cluster(data || {}, 'throughputIn', 'in', 1), ...cluster(data || {}, 'throughputOut', 'out', 2)]} />
          <LineChart title="Backlog (messages)" unit="count" step={step} series={cluster(data || {}, 'backlog', 'backlog', 1)} />
          <LineChart title="Stored data" unit="bytes" step={step} series={cluster(data || {}, 'storageSize', 'storage', 1)} />
        </>}
        <LineChart title="Bookie ledger disk usage" unit="percent" step={step} yMax={100} series={perEntity(data?.series?.diskUsage, short)} />
        <LineChart title="Bookie write throughput" unit="bytesRate" step={step} series={perEntity(data?.series?.writeBytes, short)} />
        <LineChart title="Bookie add-entry latency, p99" unit="ms" step={step} series={perEntity(data?.series?.addP99, short)} empty="No writes in this range" />
        {view.kind === 'PulsarCluster' && <LineChart title="ZooKeeper average latency" unit="ms" step={step} series={perEntity(data?.series?.zkLatency, short)} />}
      </div>

      {pods.length > 0 && (
        <table className="pods-table">
          <thead><tr><th>Pod</th><th>Component</th><th>Scrape</th><th>Heap</th><th>CPU</th></tr></thead>
          <tbody>{pods.map(p => {
            const pct = p.heapMax > 0 ? (p.heapUsed / p.heapMax) * 100 : null
            return (
              <tr key={p.pod}>
                <td className="mono small">{p.pod}</td><td>{p.component}</td>
                <td>{p.up ? <span className="ok-text">up</span> : <span className="warn-text">down</span>}</td>
                <td>{pct === null ? '—' : <span className="meter-cell"><span className="meter"><span style={{ width: `${Math.min(100, pct)}%` }} /></span>{bytes(p.heapUsed)} / {bytes(p.heapMax)}</span>}</td>
                <td>{units.cores(p.cpu)}</td>
              </tr>
            )
          })}</tbody>
        </table>
      )}
      <p className="muted small">
        Source: <code>{data?.source || status.url}</code>
        {!spec.prometheusURL && <> · retention {spec.prometheus?.retention || '15d'}, at most {spec.prometheus?.retentionSize || '80% of the volume'}</>}
        {data && <> · updated {new Date(data.updatedAt).toLocaleTimeString()}, {step}s resolution</>}
      </p>
    </Section>
  )
}
