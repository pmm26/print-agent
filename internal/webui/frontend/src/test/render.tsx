import type { ReactElement } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { Toaster } from '@/components/ui/sonner'

export function renderApp(element: ReactElement, initialEntry = '/operations/jobs') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  return { client, ...render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[initialEntry]}>{element}<Toaster /></MemoryRouter></QueryClientProvider>) }
}
