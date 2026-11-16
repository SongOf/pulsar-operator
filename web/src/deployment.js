// The serving process declares its own API. A hash never changes credential routing.
export function deployment({ pathname = '/', service = '', storageAPI = '' } = {}) {
  const storageOnly = service === 'storage'
  const underStorage = /^\/storage(?:\/|$)/.test(pathname)
  return {
    storageOnly,
    storageAPI: storageOnly && storageAPI === 'api' ? (underStorage ? '/storage/api' : '/api') : '/storage/api',
    defaultModule: storageOnly || underStorage ? 'storage' : 'pulsar',
  }
}

export function browserDeployment() {
  return deployment({ pathname: window.location.pathname,
    service: document.querySelector('meta[name="console-service"]')?.content,
    storageAPI: document.querySelector('meta[name="storage-api-base"]')?.content })
}
