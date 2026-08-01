import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { JobDialogProvider, useJobDialog } from './job-dialog'
import { renderApp } from '@/test/render'
import { server, http, HttpResponse } from '@/test/server'

function Opener() { const { openJob } = useJobDialog(); return <button onClick={() => openJob('job_1')}>Open Job</button> }

describe('Job reprint safety', () => {
  it('keeps one reprint request ID across an explicit retry', async () => {
    const bodies: Array<{ reprintRequestId: string }> = []; let calls = 0
    server.use(
      http.get('/api/v1/jobs/job_1', () => HttpResponse.json({ uid: 'job_1', jobId: 'order-1', template: 'kitchen-ticket', data: { orderNumber: '1' }, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(), state: 'completed', fulfilledPrinterCount: 1, originalPrinterCount: 1, requiresAttention: false, hasUncertainResult: false, hasManualReprints: false, originalPrinters: [{ printerId: 'kitchen', fulfilled: true, runs: [] }] })),
      http.post('/api/v1/jobs/job_1/reprint', async ({ request }) => { bodies.push(await request.json() as { reprintRequestId: string }); calls++; return calls === 1 ? HttpResponse.json({ error: 'temporary failure' }, { status: 503 }) : HttpResponse.json({ duplicate: false }) }),
    )
    renderApp(<JobDialogProvider><Opener /></JobDialogProvider>)
    await userEvent.click(screen.getByRole('button', { name: 'Open Job' }))
    await userEvent.click(await screen.findByLabelText('Select kitchen'))
    await userEvent.click(screen.getByRole('button', { name: 'Reprint selected' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create reprints' }))
    expect(await screen.findByText('temporary failure')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Create reprints' }))
    expect(bodies).toHaveLength(2)
    expect(bodies[0].reprintRequestId).toBe(bodies[1].reprintRequestId)
  })
})
