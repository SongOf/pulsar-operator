import React, { useEffect, useState } from 'react'
import { PulsarWorkspace } from './PulsarWorkspace.jsx'
import { StorageWorkspace } from './StorageWorkspace.jsx'
import { MODULES, parseRoute, routePath, storageRoute } from './routes.js'

export function ConsoleShell() {
  const [route, setRoute] = useState(() => parseRoute(window.location.hash))
  useEffect(() => {
    const changed = () => {
      const next = parseRoute(window.location.hash)
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
      <a className="console-brand" href="#/pulsar/overview"><span className="brand-mark">C</span><span>集群控制台<span className="brand-caption">PULSAR · STORAGE</span></span></a>
      <div className="nav-caption">模块</div>
      <nav className="module-nav" aria-label="模块导航">
        {MODULES.map(item => <a key={item.key} href={'#/' + item.key + '/overview'} className={'workspace-nav module-entry' + (route.module === item.key ? ' active' : '')} aria-current={route.module === item.key ? 'true' : undefined}>
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
        <div className="console-breadcrumb"><a href={'#/' + module.key + '/overview'}>{module.label}</a><span>/</span><b>{page.label}</b>{route.name && <><span>/</span><span className="breadcrumb-identity">{route.ns} / {route.name}</span></>}</div>
        <span className="module-description">{module.description}</span>
      </header>
      <main className={'console-main' + (route.module === 'storage' ? ' storage-main' : '')}>
        {route.module === 'pulsar'
          ? <PulsarWorkspace route={route} onNavigate={navigate} />
          : <StorageWorkspace page={route.page} onPageChange={page => navigate(storageRoute(page))} />}
      </main>
    </div>
  </div>
}
