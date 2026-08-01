import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { renderApp } from '@/test/render'
import { HttpResponse, http, server } from '@/test/server'
import PairDevicesPage from './pair-devices'

describe('Bluetooth pairing', () => {
  it('URL-encodes the device address and sends the entered PIN', async () => {
    let capturedAddress = ''
    let capturedPIN = ''
    server.use(
      http.get('/api/v1/bluetooth/devices', () => HttpResponse.json({ supported: true, devices: [{ name: 'Kitchen printer', address: '5A:4A:95:56:6F:B6', paired: false, connected: false, isPrinter: true }] })),
      http.post('/api/v1/bluetooth/devices/:address/pair', async ({ params, request }) => {
        capturedAddress = String(params.address)
        capturedPIN = String((await request.json() as { pin: string }).pin)
        return HttpResponse.json({ ready: true })
      }),
    )

    renderApp(<PairDevicesPage />)
    await userEvent.type(await screen.findByPlaceholderText('PIN (default 0000)'), '1234')
    await userEvent.click(screen.getByRole('button', { name: 'Pair device' }))

    await waitFor(() => expect(capturedAddress).toBe('5A:4A:95:56:6F:B6'))
    expect(capturedPIN).toBe('1234')
  })
})
