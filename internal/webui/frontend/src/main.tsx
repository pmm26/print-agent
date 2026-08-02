import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from 'react-router'
import { Toaster } from '@/components/ui/sonner'
import { queryClient } from '@/lib/query'
import App from './App'
import './index.css'

createRoot(document.getElementById('root')!).render(<StrictMode>
  <QueryClientProvider client={queryClient}><BrowserRouter basename="/admin"><App /><Toaster richColors position="bottom-center" /></BrowserRouter></QueryClientProvider>
</StrictMode>)
