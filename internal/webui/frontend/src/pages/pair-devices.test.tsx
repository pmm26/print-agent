import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { Route, Routes } from 'react-router'
import { AppShell } from '@/components/app-shell'
import { renderApp } from '@/test/render'
import { HttpResponse, http, server } from '@/test/server'
import PairDevicesPage from './pair-devices'
import PrintersPage from './printers'

describe('Bluetooth pairing', () => {
  it('URL-encodes the device address and sends the selected protocol and PIN', async () => {
    let capturedAddress = ''
    let capturedPIN = ''
    let capturedConnectionType = ''
    server.use(
      http.get('/api/v2/bluetooth/devices', () => HttpResponse.json({ supported: true, devices: [{ name: 'Kitchen printer', address: '5A:4A:95:56:6F:B6', paired: false, connected: false, isPrinter: true }] })),
      http.post('/api/v2/bluetooth/devices/:address/pair', async ({ params, request }) => {
        capturedAddress = String(params.address)
        const body = await request.json() as { pin: string; connectionType: string }
        capturedPIN = String(body.pin)
        capturedConnectionType = String(body.connectionType)
        return HttpResponse.json({ ready: true })
      }),
    )

    renderApp(<PairDevicesPage />)
    await userEvent.selectOptions(await screen.findByLabelText('Connection type for 5A:4A:95:56:6F:B6'), 'auto')
    await userEvent.type(screen.getByPlaceholderText('PIN if required'), '1234')
    await userEvent.click(screen.getByRole('button', { name: 'Pair device' }))

    await waitFor(() => expect(capturedAddress).toBe('5A:4A:95:56:6F:B6'))
    expect(capturedPIN).toBe('1234')
    expect(capturedConnectionType).toBe('auto')
  })

  it('passes the selected connection type to discovery', async () => {
    let capturedConnectionType = ''
    server.use(http.post('/api/v2/bluetooth/discovery/start', ({ request }) => {
      capturedConnectionType = new URL(request.url).searchParams.get('connectionType') || ''
      return HttpResponse.json({ scanning: true })
    }))
    renderApp(<PairDevicesPage />)

    await userEvent.selectOptions(await screen.findByLabelText('Scan type'), 'ble')
    await userEvent.click(screen.getByRole('button', { name: 'Scan for devices' }))

    await waitFor(() => expect(capturedConnectionType).toBe('ble'))
  })

  it('opens the printer editor when in-app discovery is unavailable', async () => {
    server.use(http.get('/api/v2/bluetooth/devices', () => HttpResponse.json({ supported: false, devices: [] })))
    renderApp(<Routes><Route element={<AppShell />}><Route path="setup/pair" element={<PairDevicesPage />} /><Route path="setup/printers" element={<PrintersPage />} /></Route></Routes>, '/setup/pair')

    await userEvent.click(await screen.findByRole('button', { name: 'Add printer' }))

    expect(await screen.findByRole('dialog')).toHaveTextContent('Add printer')
    await userEvent.type(screen.getByLabelText('Endpoint'), 'COM7')
    expect(screen.getByLabelText('Endpoint')).toHaveValue('COM7')
  })
})
