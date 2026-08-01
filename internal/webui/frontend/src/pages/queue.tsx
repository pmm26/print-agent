import { useMemo, useState } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, RefreshCw, XCircle } from 'lucide-react'
import { useSearchParams } from 'react-router'
import { toast } from 'sonner'
import { api, queryKeys } from '@/lib/api'
import { formatDate } from '@/lib/format'
import type { PrintRun } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from '@/components/ui/alert-dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge } from '@/components/shared'
import { useJobDialog } from '@/components/job-dialog'

const selectClass = 'h-8 rounded-lg border border-input bg-background px-2.5 text-sm outline-none focus:ring-2 focus:ring-ring/50'

export default function QueuePage() {
  const [params, setParams] = useSearchParams(); const [statusFilter, setStatusFilter] = useState(''); const [confirm, setConfirm] = useState<{ run: PrintRun; action: 'confirm' | 'cancel' } | null>(null)
  const queryClient = useQueryClient(); const { openJob } = useJobDialog()
  const status = useQuery({ queryKey: queryKeys.status, queryFn: ({ signal }) => api.status(signal), refetchInterval: 2000 })
  const selectedPrinter = params.get('printerId') || ''
  const ids = selectedPrinter ? [selectedPrinter] : (status.data?.printers.map(item => item.printer.id) || [])
  const queues = useQueries({ queries: ids.map(id => ({ queryKey: queryKeys.queue(id), queryFn: ({ signal }: { signal: AbortSignal }) => api.queue(id, 100, signal), refetchInterval: 5000 })) })
  const rows = useMemo(() => {
    const result: PrintRun[] = []
    for (const query of queues) if (query.data) result.push(...[query.data.processingRun, ...query.data.queuedRuns, ...query.data.retryPendingRuns, ...query.data.attentionRuns, ...query.data.recentTransmittedRuns].filter((run): run is PrintRun => !!run))
    const unique = [...new Map(result.map(run => [run.uid, run])).values()]
    return statusFilter ? unique.filter(run => run.status === statusFilter) : unique
  }, [queues, statusFilter])
  const mutation = useMutation({ mutationFn: ({ run, action }: { run: PrintRun; action: 'confirm' | 'cancel' }) => api.mutate(action === 'confirm' ? `/print-runs/${encodeURIComponent(run.uid)}/confirm-printed` : `/print-runs/${encodeURIComponent(run.uid)}/cancel`), onSuccess: async (_, variables) => { toast.success(variables.action === 'confirm' ? 'Print confirmed' : 'Print Run cancelled'); setConfirm(null); await Promise.all([queryClient.invalidateQueries({ queryKey: ['queue'] }), queryClient.invalidateQueries({ queryKey: ['job'] }), queryClient.invalidateQueries({ queryKey: ['jobs'] })]) }, onError: error => toast.error(error.message) })
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['queue'] })
  return <><PageHeader title="Queue" description="Current, pending, attention-required, and recently transmitted Print Runs grouped across printers." actions={<Button variant="outline" onClick={refresh}><RefreshCw />Refresh</Button>} />
    <div className="mb-4 flex flex-wrap gap-2"><select aria-label="Printer" className={selectClass} value={selectedPrinter} onChange={event => { const next = new URLSearchParams(); if (event.target.value) next.set('printerId', event.target.value); setParams(next) }}><option value="">All printers</option>{status.data?.printers.map(item => <option key={item.printer.id} value={item.printer.id}>{item.printer.displayName}</option>)}</select><select aria-label="Status" className={selectClass} value={statusFilter} onChange={event => setStatusFilter(event.target.value)}><option value="">All statuses</option>{['queued', 'processing', 'transmitted', 'failed', 'uncertain', 'cancelled'].map(value => <option key={value}>{value}</option>)}</select></div>
    {status.isLoading && <LoadingState />}{status.isError && <ErrorState error={status.error} retry={() => status.refetch()} />}{queues.some(query => query.isLoading) && !rows.length && <LoadingState rows={4} />}
    {!status.isLoading && !queues.some(query => query.isLoading) && !rows.length && <EmptyState title="No Print Runs" detail="Nothing matches the selected printer and status." />}
    {!!rows.length && <div className="overflow-x-auto rounded-xl border"><Table><TableHeader><TableRow><TableHead>Created</TableHead><TableHead>Print Run</TableHead><TableHead>Job</TableHead><TableHead>Printer</TableHead><TableHead>#</TableHead><TableHead>Trigger</TableHead><TableHead>Status</TableHead><TableHead>Error</TableHead><TableHead>Actions</TableHead></TableRow></TableHeader><TableBody>{rows.map(run => <TableRow key={run.uid}><TableCell>{formatDate(run.createdAt)}</TableCell><TableCell className="font-mono text-xs">{run.uid}{run.trigger === 'manual_reprint' && <span className="ml-2 text-primary">reprint</span>}</TableCell><TableCell className="font-mono text-xs">{run.jobUid}</TableCell><TableCell>{run.printerId}</TableCell><TableCell>{run.runNumber}</TableCell><TableCell>{run.trigger.replaceAll('_', ' ')}</TableCell><TableCell><StatusBadge value={run.status} />{run.retryPending && <span className="ml-2 text-xs text-muted-foreground">retry pending</span>}</TableCell><TableCell className="max-w-60 text-xs text-muted-foreground">{run.errorMessage || '—'}</TableCell><TableCell><div className="flex gap-1">{run.status === 'uncertain' && !run.resolution && <Button size="sm" variant="outline" onClick={() => setConfirm({ run, action: 'confirm' })}><CheckCircle2 />It printed</Button>}{run.status === 'queued' && <Button size="sm" variant="destructive" onClick={() => setConfirm({ run, action: 'cancel' })}><XCircle />Cancel</Button>}<Button size="sm" variant="outline" onClick={() => openJob(run.jobUid)}>View Job</Button></div></TableCell></TableRow>)}</TableBody></Table></div>}
    <AlertDialog open={!!confirm} onOpenChange={open => !open && !mutation.isPending && setConfirm(null)}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>{confirm?.action === 'confirm' ? 'Confirm paper came out?' : 'Cancel this Print Run?'}</AlertDialogTitle><AlertDialogDescription>{confirm?.action === 'confirm' ? 'This resolves an uncertain transmission as physically printed. Only confirm after checking the printer.' : 'Cancellation is only possible before processing begins.'}</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel disabled={mutation.isPending}>Back</AlertDialogCancel><AlertDialogAction variant={confirm?.action === 'cancel' ? 'destructive' : 'default'} disabled={mutation.isPending || !confirm} onClick={() => confirm && mutation.mutate(confirm)}>{mutation.isPending ? 'Working…' : 'Confirm'}</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
  </>
}
