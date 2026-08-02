import { createContext, useContext, useMemo, useRef, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, Ban, Printer } from 'lucide-react'
import { toast } from 'sonner'
import { api, queryKeys } from '@/lib/api'
import { formatDate, uid } from '@/lib/format'
import type { JobDetail, PrinterFulfillment } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from '@/components/ui/alert-dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { ErrorState, JsonViewer, LoadingState, StatusBadge } from '@/components/shared'

type JobDialogContextValue = { openJob: (uid: string) => void }
const JobDialogContext = createContext<JobDialogContextValue | null>(null)
export const useJobDialog = () => {
  const value = useContext(JobDialogContext)
  if (!value) throw new Error('useJobDialog must be inside JobDialogProvider')
  return value
}

function TargetCard({ target, selected, onSelect }: { target: PrinterFulfillment; selected: boolean; onSelect: (checked: boolean) => void }) {
  const state = target.fulfilled ? 'fulfilled' : target.cancelled ? 'cancelled' : 'unfulfilled'
  return <Card><CardHeader className="flex-row items-center justify-between gap-3 pb-2"><div className="flex items-center gap-2">
    <Checkbox checked={selected} onCheckedChange={checked => onSelect(checked === true)} aria-label={`Select ${target.printerId}`} />
    <CardTitle className="text-sm">{target.printerId}</CardTitle></div><StatusBadge value={state} />
  </CardHeader><CardContent className="overflow-x-auto px-0 pb-0"><Table><TableHeader><TableRow><TableHead>#</TableHead><TableHead>Run UID</TableHead><TableHead>Trigger</TableHead><TableHead>Status</TableHead><TableHead>Error</TableHead></TableRow></TableHeader><TableBody>
    {(target.runs || []).map(run => <TableRow key={run.uid}><TableCell>{run.runNumber}</TableCell><TableCell className="font-mono text-xs">{run.uid}</TableCell><TableCell>{run.trigger.replaceAll('_', ' ')}</TableCell><TableCell><StatusBadge value={run.status} />{run.retryPending && <span className="ml-2 text-xs text-muted-foreground">retry pending</span>}</TableCell><TableCell className="max-w-64 text-xs text-muted-foreground">{run.errorMessage || '—'}</TableCell></TableRow>)}
    {!target.runs?.length && <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No Print Runs</TableCell></TableRow>}
  </TableBody></Table></CardContent></Card>
}

export function JobDialogProvider({ children }: { children: ReactNode }) {
  const [jobUid, setJobUid] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [action, setAction] = useState<'reprint' | 'cancel' | null>(null)
  const [reason, setReason] = useState('')
  const reprintId = useRef('')
  const queryClient = useQueryClient()
  const job = useQuery({ queryKey: queryKeys.job(jobUid), queryFn: ({ signal }) => api.job(jobUid, signal), enabled: !!jobUid, refetchInterval: jobUid ? 2000 : false })
  const mutation = useMutation({
    mutationFn: async ({ kind, current }: { kind: 'reprint' | 'cancel'; current: JobDetail }) => {
      const printerIds = [...selected]
      if (kind === 'reprint') {
        if (!reprintId.current) reprintId.current = uid('dashboard:reprint:')
        return api.mutate(`/jobs/${encodeURIComponent(current.uid)}/reprint`, 'POST', { reprintRequestId: reprintId.current, printerIds, reason })
      }
      return api.mutate(`/jobs/${encodeURIComponent(current.uid)}/cancel`, 'POST', { printerIds, reason })
    },
    onSuccess: async (_, variables) => {
      toast.success(variables.kind === 'reprint' ? 'Reprint queued' : 'Targets cancelled')
      setAction(null); setReason(''); setSelected(new Set())
      if (variables.kind === 'reprint') reprintId.current = ''
      await Promise.all([queryClient.invalidateQueries({ queryKey: ['job'] }), queryClient.invalidateQueries({ queryKey: ['jobs'] }), queryClient.invalidateQueries({ queryKey: ['queue'] })])
    },
    onError: error => toast.error(error.message),
  })
  const value = useMemo(() => ({ openJob: (next: string) => { setJobUid(next); setSelected(new Set()) } }), [])
  const toggle = (printerId: string, checked: boolean) => setSelected(previous => {
    const next = new Set(previous)
    if (checked) next.add(printerId)
    else next.delete(printerId)
    return next
  })
  const allTargets = job.data?.originalPrinters || []
  const uncertain = allTargets.some(target => selected.has(target.printerId) && target.runs?.some(run => run.status === 'uncertain' && !run.resolution))

  return <JobDialogContext.Provider value={value}>{children}
    <Dialog open={!!jobUid} onOpenChange={open => { if (!open) { setJobUid(''); setSelected(new Set()); setAction(null) } }}><DialogContent className="max-h-[92vh] overflow-y-auto sm:max-w-5xl">
      <DialogHeader><DialogTitle>{job.data ? `${job.data.jobId} / ${job.data.template}` : 'Job details'}</DialogTitle><DialogDescription>Immutable accepted content and complete Print Run history.</DialogDescription></DialogHeader>
      {job.isLoading && <LoadingState rows={4} />}{job.isError && <ErrorState error={job.error} retry={() => job.refetch()} />}
      {job.data && <div className="space-y-4"><div className="grid gap-3 rounded-xl border bg-muted/30 p-4 sm:grid-cols-4">
        <div><p className="text-xs text-muted-foreground">Job UID</p><p className="truncate font-mono text-xs" title={job.data.uid}>{job.data.uid}</p></div>
        <div><p className="text-xs text-muted-foreground">State</p><StatusBadge value={job.data.state} /></div>
        <div><p className="text-xs text-muted-foreground">Original printers</p><p className="font-medium">{job.data.fulfilledPrinterCount}/{job.data.originalPrinterCount} fulfilled</p></div>
        <div><p className="text-xs text-muted-foreground">Created</p><p className="text-sm">{formatDate(job.data.createdAt)}</p></div>
      </div><JsonViewer value={job.data.data} />
      {!!allTargets.length && <div className="flex items-center gap-2"><Checkbox checked={selected.size === allTargets.length} onCheckedChange={checked => setSelected(checked === true ? new Set(allTargets.map(target => target.printerId)) : new Set())} id="job-select-all" /><Label htmlFor="job-select-all">Select all targets</Label></div>}
      <div className="space-y-3">{allTargets.map(target => <TargetCard key={target.printerId} target={target} selected={selected.has(target.printerId)} onSelect={checked => toggle(target.printerId, checked)} />)}</div></div>}
      <DialogFooter><Button variant="destructive" disabled={!selected.size || mutation.isPending} onClick={() => { setReason(''); setAction('cancel') }}><Ban />Cancel selected</Button><Button disabled={!selected.size || mutation.isPending} onClick={() => { reprintId.current ||= uid('dashboard:reprint:'); setReason(''); setAction('reprint') }}><Printer />Reprint selected</Button></DialogFooter>
    </DialogContent></Dialog>
    <AlertDialog open={action !== null} onOpenChange={open => { if (!open && !mutation.isPending) setAction(null) }}><AlertDialogContent>
      <AlertDialogHeader><AlertDialogTitle>{action === 'reprint' ? 'Create physical reprints?' : 'Cancel pending output?'}</AlertDialogTitle><AlertDialogDescription>{action === 'reprint' ? `${uncertain ? 'An earlier result is uncertain and may already have printed. ' : ''}This will create ${selected.size} new physical Print Run${selected.size === 1 ? '' : 's'}.` : `Pending output for ${selected.size} original target${selected.size === 1 ? '' : 's'} will be cancelled.`}</AlertDialogDescription></AlertDialogHeader>
      {uncertain && action === 'reprint' && <div className="flex gap-2 rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm text-amber-800 dark:text-amber-200"><AlertTriangle className="size-5 shrink-0" />Paper may already have come out.</div>}
      <div className="space-y-2"><Label htmlFor="job-action-reason">Reason (optional)</Label><Input id="job-action-reason" value={reason} onChange={event => setReason(event.target.value)} maxLength={500} /></div>
      <AlertDialogFooter><AlertDialogCancel disabled={mutation.isPending}>Back</AlertDialogCancel><AlertDialogAction variant={action === 'cancel' ? 'destructive' : 'default'} disabled={mutation.isPending || !job.data} onClick={() => job.data && action && mutation.mutate({ kind: action, current: job.data })}>{mutation.isPending ? 'Working…' : action === 'reprint' ? 'Create reprints' : 'Cancel output'}</AlertDialogAction></AlertDialogFooter>
    </AlertDialogContent></AlertDialog>
  </JobDialogContext.Provider>
}
