import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import App from './App'
import { renderApp } from '@/test/render'

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
})
