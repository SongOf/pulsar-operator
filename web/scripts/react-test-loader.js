import { createServer } from 'vite'
import { fileURLToPath } from 'node:url'
import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'

// Compile the actual JSX modules without a listening socket or browser globals.
export async function reactViews() {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)), configFile: false,
    server: { middlewareMode: true, hmr: false, ws: false, watch: null }, appType: 'custom',
    optimizeDeps: { noDiscovery: true, include: [] },
  })
  try {
    const views = await server.ssrLoadModule('/src/storage-views.jsx')
    const dialogs = await server.ssrLoadModule('/src/StorageDialogs.jsx')
    const pools = await server.ssrLoadModule('/src/PoolDetails.jsx')
    const components = { ...views, ...dialogs, ...pools }
    return { views: components, render: (name, props) => renderToStaticMarkup(React.createElement(components[name], props)), close: () => server.close() }
  } catch (error) { await server.close(); throw error }
}
