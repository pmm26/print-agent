import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { Route, Routes } from 'react-router'
import { AppShell } from '@/components/app-shell'
import { renderApp } from '@/test/render'
import { PrinterLogsPage } from './logs'
import PrintersPage from './printers'

describe('Printer log navigation', () => {
  it('opens URL-filtered Printer Logs from a printer card', async () => {
    renderApp(<Routes><Route element={<AppShell />}><Route path="setup/printers" element={<PrintersPage />} /><Route path="operations/printer-logs" element={<PrinterLogsPage />} /></Route></Routes>, '/setup/printers')
    await userEvent.click(await screen.findByRole('button', { name: 'Logs' }))
    expect(await screen.findByRole('heading', { name: 'Printer Logs' })).toBeInTheDocument()
    expect(screen.getByLabelText('Printer ID')).toHaveValue('kitchen')
  })
})
