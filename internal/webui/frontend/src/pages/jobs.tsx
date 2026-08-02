import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { RefreshCw, Search } from 'lucide-react'
import { api, queryKeys } from '@/lib/api'
import { formatDate } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge } from '@/components/shared'
import { useJobDialog } from '@/components/job-dialog'

export default function JobsPage() {
  const [draft, setDraft] = useState(''); const [filter, setFilter] = useState('')
  const jobs = useQuery({ queryKey: queryKeys.jobs(filter), queryFn: ({ signal }) => api.jobs(filter, signal), refetchInterval: 5000 })
  const { openJob } = useJobDialog()
  return <><PageHeader title="Jobs" description="One Job per external job ID and template, with fulfillment across original printers." actions={<Button variant="outline" onClick={() => jobs.refetch()} disabled={jobs.isFetching}><RefreshCw className={jobs.isFetching ? 'animate-spin' : ''} />Refresh</Button>} />
    <form className="mb-4 flex max-w-md gap-2" onSubmit={event => { event.preventDefault(); setFilter(draft.trim()) }}><Input value={draft} onChange={event => setDraft(event.target.value)} placeholder="Filter by external job ID" aria-label="External job ID" /><Button type="submit" variant="secondary"><Search />Filter</Button></form>
    {jobs.isLoading && <LoadingState rows={5} />}{jobs.isError && <ErrorState error={jobs.error} retry={() => jobs.refetch()} />}
    {jobs.data && !jobs.data.jobs.length && <EmptyState title="No Jobs" detail={filter ? 'No Jobs match this external ID.' : 'Jobs submitted by the POS will appear here.'} />}
    {!!jobs.data?.jobs.length && <div className="overflow-x-auto rounded-xl border"><Table><TableHeader><TableRow><TableHead>Job</TableHead><TableHead>Template</TableHead><TableHead>Created</TableHead><TableHead>State</TableHead><TableHead>Fulfilled</TableHead><TableHead>Flags</TableHead></TableRow></TableHeader><TableBody>{jobs.data.jobs.map(job => {
      const flags = [job.requiresAttention ? 'Attention' : '', job.hasUncertainResult ? 'Uncertain' : '', job.hasManualReprints ? 'Reprints' : ''].filter(Boolean)
      return <TableRow key={job.uid} className="cursor-pointer" tabIndex={0} onClick={() => openJob(job.uid)} onKeyDown={event => { if (event.key === 'Enter' || event.key === ' ') openJob(job.uid) }}>
        <TableCell><p className="font-medium">{job.jobId}</p><p className="max-w-64 truncate font-mono text-xs text-muted-foreground">{job.uid}</p></TableCell><TableCell>{job.template}</TableCell><TableCell className="whitespace-nowrap">{formatDate(job.createdAt)}</TableCell><TableCell><StatusBadge value={job.state} /></TableCell><TableCell>{job.fulfilledPrinterCount}/{job.originalPrinterCount}</TableCell><TableCell className="text-xs text-muted-foreground">{flags.join(' · ') || '—'}</TableCell>
      </TableRow>})}</TableBody></Table></div>}
  </>
}
