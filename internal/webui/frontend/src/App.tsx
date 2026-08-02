import { lazy, Suspense, useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Navigate, Route, Routes, useLocation } from 'react-router'
import { AppShell } from '@/components/app-shell'
import { LoadingState } from '@/components/shared'
import { api, queryKeys } from '@/lib/api'

const JobsPage = lazy(() => import('@/pages/jobs'))
const PairDevicesPage = lazy(() => import('@/pages/pair-devices'))
const PrintersPage = lazy(() => import('@/pages/printers'))
const POSPairingPage = lazy(() => import('@/pages/pos-pairing'))
const QueuePage = lazy(() => import('@/pages/queue'))
const DiagnosticsPage = lazy(() => import('@/pages/diagnostics'))
const POSSimulatorPage = lazy(() => import('@/pages/pos-simulator'))
const SystemLogsPage = lazy(() => import('@/pages/logs').then(module => ({ default: module.SystemLogsPage })))
const PrinterLogsPage = lazy(() => import('@/pages/logs').then(module => ({ default: module.PrinterLogsPage })))

const titles: Record<string, string> = {
  '/setup/pair': 'Pair devices', '/setup/printers': 'Printers', '/setup/pos': 'POS Pairing',
  '/operations/jobs': 'Jobs', '/operations/queue': 'Queue', '/operations/printer-logs': 'Printer Logs',
  '/system/diagnostics': 'Diagnostics', '/system/logs': 'System Logs', '/dev/pos-simulator': 'POS Simulator',
}

function TitleSync() {
  const location = useLocation()
  useEffect(() => { document.title = `${titles[location.pathname] || 'Jobs'} · Print Agent` }, [location.pathname])
  return null
}

function LinuxPairRoute() {
  const status = useQuery({ queryKey: queryKeys.status, queryFn: ({ signal }) => api.status(signal) })
  if (status.isPending) return <LoadingState rows={4} />
  if (status.data?.platform !== 'linux') return <Navigate to="/setup/printers" replace />
  return <PairDevicesPage />
}

export default function App() {
  return <><TitleSync /><Suspense fallback={<div className="mx-auto max-w-7xl p-6"><LoadingState rows={6} /></div>}><Routes>
    <Route element={<AppShell />}>
      <Route index element={<Navigate to="/operations/jobs" replace />} />
      <Route path="setup/pair" element={<LinuxPairRoute />} />
      <Route path="setup/printers" element={<PrintersPage />} />
      <Route path="setup/pos" element={<POSPairingPage />} />
      <Route path="operations/jobs" element={<JobsPage />} />
      <Route path="operations/queue" element={<QueuePage />} />
      <Route path="operations/printer-logs" element={<PrinterLogsPage />} />
      <Route path="system/diagnostics" element={<DiagnosticsPage />} />
      <Route path="system/logs" element={<SystemLogsPage />} />
      <Route path="dev/pos-simulator" element={<POSSimulatorPage />} />
      <Route path="*" element={<Navigate to="/operations/jobs" replace />} />
    </Route>
  </Routes></Suspense></>
}
