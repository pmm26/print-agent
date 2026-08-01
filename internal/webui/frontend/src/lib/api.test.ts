import { describe, expect, it, vi } from 'vitest'
import { APIError, request } from './api'
import { queryClient } from './query'

describe('API client', () => {
  it('turns structured failures into APIError without retrying mutations', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(new Response(JSON.stringify({ code: 'conflict', error: 'already exists' }), { status: 409, headers: { 'Content-Type': 'application/json' } }))
    await expect(request('/test')).rejects.toEqual(expect.objectContaining<Partial<APIError>>({ status: 409, code: 'conflict', message: 'already exists' }))
    expect(queryClient.getDefaultOptions().mutations?.retry).toBe(false)
    vi.restoreAllMocks()
  })
})
