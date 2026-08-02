import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import App from './App'
import { renderApp } from '@/test/render'
import { HttpResponse, http, server } from '@/test/server'

describe('dashboard routes', () => {
  for (const [path, heading] of [
    ['/setup/pair', 'Pair Bluetooth devices'],
    ['/setup/printers', 'Printers'],
    ['/setup/pos', 'POS Pairing'],
    ['/operations/jobs', 'Jobs'],
    ['/operations/queue', 'Queue'],
    ['/operations/printer-logs', 'Printer Logs'],
    ['/system/diagnostics', 'Diagnostics'],
    ['/system/logs', 'System Logs'],
    ['/dev/pos-simulator', 'POS Simulator'],
  ]) {
    it(`renders ${path}`, async () => {
      renderApp(<App />, path)
      expect(await screen.findByRole('heading', { name: heading })).toBeInTheDocument()
    })
  }

  it('redirects and hides Pair devices on non-Linux platforms', async () => {
    server.use(http.get('/api/v2/status', () => HttpResponse.json({ agent: { version: 'test', databaseOk: true }, platform: 'windows', degraded: false, persistence: { paused: false }, printers: [] })))
    renderApp(<App />, '/setup/pair')

    expect(await screen.findByRole('heading', { name: 'Printers' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Setup' }))
    expect(await screen.findByRole('menuitem', { name: 'Printers' })).toBeInTheDocument()
    expect(screen.queryByText('Pair devices')).not.toBeInTheDocument()
  })
})
