import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AppShell } from '@/components/app-shell'
import { Route, Routes, useNavigate } from 'react-router'
import { SystemLogsPage } from './logs'
import { renderApp } from '@/test/render'

function Layout() {
  const navigate = useNavigate()
  return <><button onClick={() => navigate(-1)}>Test back</button><AppShell /></>
}

describe('System Logs URL state', () => {
  it('restores filters from the URL and loads older pages', async () => {
    renderApp(<Routes><Route element={<Layout />}><Route path="system/logs" element={<SystemLogsPage />} /></Route></Routes>, '/system/logs?levels=warn,error&printerId=kitchen&q=timeout')
    expect(await screen.findByRole('heading', { name: 'System Logs' })).toBeInTheDocument()
    expect(screen.getByLabelText('Printer ID')).toHaveValue('kitchen')
    expect(screen.getByLabelText('Search')).toHaveValue('timeout')
    expect(screen.getByRole('checkbox', { name: 'debug' })).not.toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'warn' })).toBeChecked()
    expect(await screen.findByText('latest timeout')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Load older' }))
    expect(await screen.findByText('older timeout')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Clear' }))
    expect(await screen.findByLabelText('Printer ID')).toHaveValue('')
    await userEvent.click(screen.getByRole('button', { name: 'Test back' }))
    expect(await screen.findByLabelText('Printer ID')).toHaveValue('kitchen')
    expect(screen.getByLabelText('Search')).toHaveValue('timeout')
  })
})
