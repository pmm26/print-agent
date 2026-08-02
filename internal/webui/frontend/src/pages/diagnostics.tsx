import { useQuery } from '@tanstack/react-query'
import { Download, RefreshCw } from 'lucide-react'
import { api, queryKeys } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { ErrorState, JsonViewer, LoadingState, PageHeader } from '@/components/shared'

export default function DiagnosticsPage() {
  const diagnostics = useQuery({ queryKey: queryKeys.diagnostics, queryFn: ({ signal }) => api.diagnostics(signal) })
  return <><PageHeader title="Diagnostics" description="Agent, database, platform, printer, and configuration troubleshooting information." actions={<><Button variant="outline" onClick={() => diagnostics.refetch()} disabled={diagnostics.isFetching}><RefreshCw className={diagnostics.isFetching ? 'animate-spin' : ''} />Refresh</Button><Button render={<a href="/api/v2/diagnostics/export" />}><Download />Export diagnostics</Button></>} />
    {diagnostics.isLoading && <LoadingState rows={5} />}{diagnostics.isError && <ErrorState error={diagnostics.error} retry={() => diagnostics.refetch()} />}{diagnostics.data && <JsonViewer value={diagnostics.data} className="max-h-[70vh]" />}
  </>
}
