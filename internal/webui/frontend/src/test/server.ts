import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'

const status = {
  agent: { version: 'test', databaseOk: true }, degraded: false, persistence: { paused: false },
  printers: [{ printer: { id: 'kitchen', displayName: 'Kitchen', enabled: true, transport: 'mock', endpoint: 'mock://kitchen', baudRate: 9600, dataBits: 8, stopBits: 1, parity: 'none', charactersPerLine: 32, encoding: 'CP858', autoReconnect: true }, state: 'connected', endpoint: 'mock://kitchen', queueDepth: 0, attentionCount: 0 }],
}

export const handlers = [
  http.get('/api/v1/status', () => HttpResponse.json(status)),
  http.get('/api/v1/jobs', () => HttpResponse.json({ jobs: [], nextCursor: '' })),
  http.get('/api/v1/bluetooth/devices', () => HttpResponse.json({ supported: true, devices: [] })),
  http.get('/api/v1/bluetooth/candidates', () => HttpResponse.json([])),
  http.get('/api/v1/admin/settings', () => HttpResponse.json({ allowedOrigin: 'https://pos.example.test' })),
  http.get('/api/v1/admin/tokens', () => HttpResponse.json([])),
  http.get('/api/v1/printers/:printerId/queue', () => HttpResponse.json({ printer: status.printers[0], processingRun: null, queuedRuns: [], retryPendingRuns: [], attentionRuns: [], recentTransmittedRuns: [] })),
  http.get('/api/v1/system-logs', ({ request }) => {
    const cursor = new URL(request.url).searchParams.get('cursor')
    return HttpResponse.json(cursor ? { logs: [{ id: 1, createdAt: new Date().toISOString(), level: 'warn', message: 'older timeout', attributes: {} }], nextCursor: '', retentionSeconds: 7200 } : { logs: [{ id: 2, createdAt: new Date().toISOString(), level: 'error', message: 'latest timeout', printerId: 'kitchen', attributes: { path: '/print' } }], nextCursor: 'older-cursor', retentionSeconds: 7200 })
  }),
  http.get('/api/v1/printer-logs', () => HttpResponse.json({ events: [], nextCursor: '', retentionSeconds: 172800 })),
  http.get('/api/v1/diagnostics', () => HttpResponse.json({ report: { databaseOk: true } })),
]

export const server = setupServer(...handlers)
export { http, HttpResponse }
