import React, { useEffect, useState } from 'react'
import { PulsarWorkspace } from './PulsarWorkspace.jsx'
import { StorageWorkspace } from './StorageWorkspace.jsx'
import { MODULES, parseRoute, routePath, storageRoute } from './routes.js'
import { browserDeployment } from './deployment.js'

export function ConsoleShell() {
  const [environment] = useState(browserDeployment)
  const resolve = () => {
    let hash = window.location.hash
    if ((!hash || hash === '#' || hash === '#/') && environment.defaultModule === 'storage') hash = '#/storage/overview'
    // Retain direct Storage bookmarks from before the shared React shell.
    if (environment.defaultModule === 'storage' && /^#(?:overview|nodes|disks|pools|volumes|events|guide)$/.test(hash)) hash = '#/storage/' + hash.slice(1)
    const route = parseRoute(hash)
    return environment.storageOnly && route.module === 'pulsar' ? { module: 'storage', page: 'overview' } : route
  }
  const [route, setRoute] = useState(resolve)
  useEffect(() => {
    const changed = () => {
      const next = resolve()
      const canonical = '#/' + routePath(next)
      if (window.location.hash !== canonical) {
        window.history.replaceState(null, '', window.location.pathname + window.location.search + canonical)
      }
      setRoute(next)
    }
    changed()
    window.addEventListener('hashchange', changed)
    return () => window.removeEventListener('hashchange', changed)
  }, [])
  const navigate = path => { window.location.hash = '/' + path }
  const module = MODULES.find(item => item.key === route.module)
  const page = module.pages.find(item => item.key === route.page)

  return <div className="console-layout">
    <aside className="console-sidebar">
      <a className="console-brand" href={'#/' + environment.defaultModule + '/overview'}><span className="brand-mark">C</span><span>集群控制台<span className="brand-caption">PULSAR · STORAGE</span></span></a>
      <div className="nav-caption">模块</div>
      <nav className="module-nav" aria-label="模块导航">
        {MODULES.map(item => <a key={item.key} href={environment.storageOnly && item.key === 'pulsar' ? undefined : '#/' + item.key + '/overview'} aria-disabled={environment.storageOnly && item.key === 'pulsar' ? 'true' : undefined} title={environment.storageOnly && item.key === 'pulsar' ? '当前独立 Storage 服务未提供 Pulsar API；请通过统一 Gateway 访问。' : undefined} className={'workspace-nav module-entry' + (route.module === item.key ? ' active' : '')} aria-current={route.module === item.key ? 'true' : undefined}>
          <span className="module-mark" aria-hidden="true">{item.mark}</span><span><strong>{item.label}</strong><small>{item.description}</small></span>
        </a>)}
      </nav>
      <div className="nav-caption section-caption">{module.label} 管理</div>
      <nav className="module-subnav" aria-label={module.label + ' 页面导航'}>
        {module.pages.map(item => <a key={item.key} href={'#/' + routePath({ module: route.module, page: item.key })} className={route.page === item.key ? 'active' : ''} aria-current={route.page === item.key ? 'page' : undefined}>{item.label}</a>)}
      </nav>
      <div className="sidebar-note"><span className="live-dot" /> Kubernetes 控制面<p>消息集群与物理存储<br />统一入口 · 独立管理</p></div>
    </aside>
    <div className="console-body">
      <header className="console-topbar">
        <div className="console-breadcrumb"><a href={'#/' + module.key + '/overview'}>{module.label}</a><span>/</span><b>{page.label}</b>{route.name && <><span>/</span><span className="breadcrumb-identity">{route.ns && route.ns + ' / '}{route.name}</span></>}</div>
        <span className="module-description">{module.description}</span>
      </header>
      <main className={'console-main' + (route.module === 'storage' ? ' storage-main' : '')}>
        {route.module === 'pulsar'
          ? <PulsarWorkspace route={route} onNavigate={navigate} />
          : <StorageWorkspace route={route} apiBase={environment.storageAPI} storageOnly={environment.storageOnly} onNavigate={(page, name) => navigate(storageRoute(page, name))} />}
      </main>
    </div>
  </div>
}
