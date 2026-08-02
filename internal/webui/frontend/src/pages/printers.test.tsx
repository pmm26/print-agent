import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { Route, Routes } from 'react-router'
import { AppShell } from '@/components/app-shell'
import { renderApp } from '@/test/render'
import { HttpResponse, http, server } from '@/test/server'
import { PrinterLogsPage } from './logs'
import PrintersPage from './printers'

describe('Printer log navigation', () => {
  it('opens URL-filtered Printer Logs from a printer card', async () => {
    renderApp(<Routes><Route element={<AppShell />}><Route path="setup/printers" element={<PrintersPage />} /><Route path="operations/printer-logs" element={<PrinterLogsPage />} /></Route></Routes>, '/setup/printers')
    await userEvent.click(await screen.findByRole('button', { name: 'Logs' }))
    expect(await screen.findByRole('heading', { name: 'Printer Logs' })).toBeInTheDocument()
    expect(screen.getByLabelText('Printer ID')).toHaveValue('kitchen')
  })

  it('saves a manually entered endpoint without a stale discovered address', async () => {
    let payload: { endpoint?: string; deviceAddress?: string; connectionPreference?: string } = {}
    server.use(
      http.get('/api/v2/bluetooth/candidates', () => HttpResponse.json([{ endpoint: 'COM8', deviceName: 'Kitchen printer', deviceAddress: 'AA:BB:CC:DD:EE:FF', connected: true, isPrinter: true }])),
      http.post('/api/v2/printers', async ({ request }) => {
        payload = await request.json() as typeof payload
        return HttpResponse.json(payload)
      }),
    )
    renderApp(<Routes><Route element={<AppShell />}><Route path="setup/printers" element={<PrintersPage />} /></Route></Routes>, '/setup/printers?newEndpoint=')

    await userEvent.type(await screen.findByLabelText('Printer ID'), 'prep')
    const endpoint = screen.getByLabelText('Endpoint')
    await waitFor(() => expect(document.querySelector('option[value="COM8"]')).toBeInTheDocument())
    await userEvent.type(endpoint, 'COM8')
    await userEvent.clear(endpoint)
    await userEvent.type(endpoint, 'COM7')
    await userEvent.click(screen.getByRole('button', { name: 'Save printer' }))

    await waitFor(() => expect(payload.endpoint).toBe('COM7'))
    expect(payload.deviceAddress).toBe('')
    expect(payload.connectionPreference).toBe('auto')
  })

  it('preserves a protocol-specific connection preference from device setup', async () => {
    renderApp(<Routes><Route element={<AppShell />}><Route path="setup/printers" element={<PrintersPage />} /></Route></Routes>, '/setup/printers?newEndpoint=rfcomm%3A%2F%2FAA%3ABB%3ACC%3ADD%3AEE%3AFF&deviceAddress=AA%3ABB%3ACC%3ADD%3AEE%3AFF&connectionPreference=rfcomm')

    expect(await screen.findByLabelText('Endpoint')).toHaveValue('rfcomm://AA:BB:CC:DD:EE:FF')
    expect(screen.getByLabelText('Default connection type')).toHaveValue('rfcomm')
  })
})
