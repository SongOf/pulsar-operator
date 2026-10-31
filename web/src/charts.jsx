import React, { useEffect, useMemo, useRef, useState } from 'react'

// Units format values for axes, tooltips and tiles.
export const units = {
  rate: v => `${compact(v)}/s`,
  bytesRate: v => `${bytes(v)}/s`,
  bytes: v => bytes(v),
  percent: v => `${round(v)}%`,
  ms: v => `${round(v)} ms`,
  count: v => compact(v),
  cores: v => `${round(v, 2)} cores`,
}

function round(v, digits = 1) {
  if (!isFinite(v)) return '—'
  return Math.abs(v) >= 100 ? Math.round(v).toLocaleString() : (+v.toFixed(digits)).toString()
}

export function compact(v) {
  if (!isFinite(v)) return '—'
  const a = Math.abs(v)
  if (a >= 1e9) return `${round(v / 1e9)}G`
  if (a >= 1e6) return `${round(v / 1e6)}M`
  if (a >= 1e3) return `${round(v / 1e3)}k`
  return round(v)
}

export function bytes(v) {
  if (!isFinite(v)) return '—'
  const steps = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (Math.abs(v) >= 1024 && i < steps.length - 1) { v /= 1024; i++ }
  return `${round(v)} ${steps[i]}`
}

// slotOf keeps a color on its entity: bookie-3 is always slot 4, whatever else is drawn.
export function slotOf(name, index) {
  const m = /-(\d+)$/.exec(name)
  return ((m ? +m[1] : index) % 8) + 1
}

function niceMax(v) {
  if (!(v > 0)) return 1
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  for (const f of [1, 2, 2.5, 5, 10]) if (f * p >= v) return f * p
  return 10 * p
}

function timeLabel(t, spanSeconds) {
  const d = new Date(t * 1000)
  const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
  return spanSeconds > 86400 ? `${d.getMonth() + 1}/${d.getDate()} ${hm}` : hm
}

function useWidth() {
  const ref = useRef(null)
  const [w, setW] = useState(600)
  useEffect(() => {
    if (!ref.current || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(([e]) => setW(Math.max(240, Math.floor(e.contentRect.width))))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  return [ref, w]
}

// LineChart draws one measure on one axis: one line per series, crosshair and tooltip on hover, a legend for two
// or more series, end labels for up to four, and a table view.
export function LineChart({ title, series, unit, step = 15, yMax, empty = 'No data in this range' }) {
  const [ref, width] = useWidth()
  const [hover, setHover] = useState(null)
  const [table, setTable] = useState(false)
  const fmt = units[unit] || units.count
  const height = 200, pad = { l: 56, r: series.length > 1 && series.length <= 4 ? 84 : 12, t: 10, b: 24 }
  const iw = width - pad.l - pad.r, ih = height - pad.t - pad.b

  const { times, x0, x1, max } = useMemo(() => {
    const ts = new Set()
    let lo = Infinity, hi = -Infinity, top = 0
    for (const s of series) for (const [t, v] of s.points) { ts.add(t); lo = Math.min(lo, t); hi = Math.max(hi, t); top = Math.max(top, v) }
    return { times: [...ts].sort((a, b) => a - b), x0: lo, x1: hi, max: yMax ?? niceMax(top) }
  }, [series, yMax])

  const hasData = times.length > 0
  const sx = t => pad.l + (x1 === x0 ? iw / 2 : ((t - x0) / (x1 - x0)) * iw)
  const sy = v => pad.t + ih - (v / max) * ih
  // A gap longer than three steps breaks the line instead of drawing across missing scrapes.
  const path = pts => pts.map(([t, v], i) => `${i === 0 || t - pts[i - 1][0] > step * 3 ? 'M' : 'L'}${sx(t).toFixed(1)},${sy(v).toFixed(1)}`).join('')

  const onMove = e => {
    if (!hasData) return
    const box = e.currentTarget.getBoundingClientRect()
    const px = ((e.clientX - box.left) / box.width) * width
    let best = times[0]
    for (const t of times) if (Math.abs(sx(t) - px) < Math.abs(sx(best) - px)) best = t
    setHover(best)
  }
  const valueAt = (s, t) => s.points.find(p => p[0] === t)?.[1]

  // End labels, pushed apart vertically so they never overlap.
  const ends = series.length > 1 && series.length <= 4 ? series.map(s => {
    const last = s.points[s.points.length - 1]
    return last ? { name: s.name, slot: s.slot, y: sy(last[1]) } : null
  }).filter(Boolean).sort((a, b) => a.y - b.y) : []
  for (let i = 1; i < ends.length; i++) if (ends[i].y - ends[i - 1].y < 13) ends[i].y = ends[i - 1].y + 13

  return (
    <figure className="chart" ref={ref}>
      <figcaption>
        <span className="chart-title">{title}</span>
        <button type="button" className="link small" onClick={() => setTable(!table)}>{table ? 'Chart' : 'Table'}</button>
      </figcaption>
      {series.length > 1 && (
        <div className="legend">{series.map(s => <span key={s.name}><i style={{ background: `var(--series-${s.slot})` }} />{s.name}</span>)}</div>
      )}
      {!hasData ? <div className="chart-empty muted small" style={{ height }}>{empty}</div> : table ? (
        <div className="chart-table" style={{ maxHeight: height }}>
          <table>
            <thead><tr><th>Time</th>{series.map(s => <th key={s.name}>{s.name}</th>)}</tr></thead>
            <tbody>{times.slice(-30).reverse().map(t => (
              <tr key={t}><td>{timeLabel(t, 2 * 86400)}</td>{series.map(s => { const v = valueAt(s, t); return <td key={s.name}>{v === undefined ? '—' : fmt(v)}</td> })}</tr>
            ))}</tbody>
          </table>
        </div>
      ) : (
        <div className="chart-plot">
          <svg width="100%" viewBox={`0 0 ${width} ${height}`} role="img" aria-label={title}
            onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
            {[0, 0.25, 0.5, 0.75, 1].map(f => (
              <g key={f}>
                <line className="grid" x1={pad.l} x2={pad.l + iw} y1={sy(max * f)} y2={sy(max * f)} />
                <text className="axis" x={pad.l - 6} y={sy(max * f) + 4} textAnchor="end">{fmt(max * f)}</text>
              </g>
            ))}
            {[x0, (x0 + x1) / 2, x1].map((t, i) => (
              <text key={i} className="axis" x={sx(t)} y={height - 6} textAnchor={i === 0 ? 'start' : i === 2 ? 'end' : 'middle'}>{timeLabel(t, x1 - x0)}</text>
            ))}
            {series.map(s => <path key={s.name} d={path(s.points)} fill="none" stroke={`var(--series-${s.slot})`} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />)}
            {ends.map(e => <text key={e.name} className="end-label" x={pad.l + iw + 6} y={e.y + 4}>{e.name}</text>)}
            {hover !== null && (
              <g>
                <line className="crosshair" x1={sx(hover)} x2={sx(hover)} y1={pad.t} y2={pad.t + ih} />
                {series.map(s => { const v = valueAt(s, hover); return v === undefined ? null : (
                  <circle key={s.name} cx={sx(hover)} cy={sy(v)} r="4" fill={`var(--series-${s.slot})`} stroke="var(--panel)" strokeWidth="2" />
                ) })}
              </g>
            )}
          </svg>
          {hover !== null && (
            <div className="tooltip" style={{ left: `${(sx(hover) / width) * 100}%`, transform: sx(hover) > width * 0.6 ? 'translateX(calc(-100% - 12px))' : 'translateX(12px)' }}>
              <div className="muted small">{timeLabel(hover, 2 * 86400)}</div>
              {series.map(s => { const v = valueAt(s, hover); return v === undefined ? null : (
                <div key={s.name} className="tip-row"><i style={{ background: `var(--series-${s.slot})` }} /><span>{s.name}</span><b>{fmt(v)}</b></div>
              ) })}
            </div>
          )}
        </div>
      )}
    </figure>
  )
}

// StatTile is a headline number with its unit.
export function StatTile({ label, value, unit, tone, hint }) {
  const fmt = units[unit] || units.count
  return (
    <div className={'tile' + (tone ? ' ' + tone : '')}>
      <div className="tile-label">{label}</div>
      <div className="tile-value">{value === undefined ? '—' : fmt(value)}</div>
      {hint && <div className="muted small">{hint}</div>}
    </div>
  )
}
