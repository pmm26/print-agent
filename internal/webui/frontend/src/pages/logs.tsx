import { useEffect, useMemo, useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ChevronDown, RefreshCw, Search, X } from 'lucide-react'
import { useSearchParams } from 'react-router'
import { toast } from 'sonner'
import { api, queryKeys } from '@/lib/api'
import { formatDate, localDateTimeValue, urlTimeValue } from '@/lib/format'
import type { PrinterLogEvent, SystemLogRecord } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Checkbox } from '@/components/ui/checkbox'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { ErrorState, LoadingState, PageHeader, StatusBadge, formatField } from '@/components/shared'

const inputClass = 'h-8 w-full rounded-lg border border-input bg-background px-2.5 text-sm outline-none focus:ring-2 focus:ring-ring/50'
const levels = ['debug', 'info', 'warn', 'error'] as const
const events = ['printer.connected', 'printer.disconnected', 'printer.reconnect_scheduled', 'printer.error', 'printer.configuration_changed', 'print_run.queued', 'print_run.claimed', 'print_run.transmission_started', 'print_run.transmitted', 'print_run.failed', 'print_run.uncertain', 'print_run.cancelled', 'print_run.confirmed_printed']

function useSystemFeed(params: URLSearchParams) {
  const key = params.toString(); const [older, setOlder] = useState<SystemLogRecord[]>([]); const [cursor, setCursor] = useState('')
  const latest = useQuery({ queryKey: queryKeys.systemLogs(key), queryFn: ({ signal }) => { const request = new URLSearchParams(params); request.set('limit', '100'); return api.systemLogs(request, signal) }, refetchInterval: 5000 })
  useEffect(() => { setOlder([]); setCursor('') }, [key])
  useEffect(() => { if (latest.data && !older.length) setCursor(latest.data.nextCursor || '') }, [latest.data, older.length])
  const rows = useMemo(() => { const cutoff = Date.now() - (latest.data?.retentionSeconds || 7200) * 1000; return [...new Map([...(latest.data?.logs || []), ...older].filter(row => new Date(row.createdAt).getTime() >= cutoff).map(row => [row.id, row])).values()] }, [latest.data, older])
  const loadOlder = async () => { if (!cursor) return; try { const request = new URLSearchParams(params); request.set('limit', '100'); request.set('cursor', cursor); const page = await api.systemLogs(request); setOlder(previous => [...previous, ...page.logs]); setCursor(page.nextCursor || '') } catch (error) { toast.error(error instanceof Error ? error.message : 'Could not load older logs') } }
  const refresh = () => { setOlder([]); setCursor(''); latest.refetch() }
  return { ...latest, rows, cursor, loadOlder, refresh }
}

function usePrinterFeed(params: URLSearchParams) {
  const key = params.toString(); const [older, setOlder] = useState<PrinterLogEvent[]>([]); const [cursor, setCursor] = useState('')
  const latest = useQuery({ queryKey: queryKeys.printerLogs(key), queryFn: ({ signal }) => { const request = new URLSearchParams(params); request.set('limit', '100'); return api.printerLogs(request, signal) }, refetchInterval: 5000 })
  useEffect(() => { setOlder([]); setCursor('') }, [key])
  useEffect(() => { if (latest.data && !older.length) setCursor(latest.data.nextCursor || '') }, [latest.data, older.length])
  const rows = useMemo(() => { const cutoff = Date.now() - (latest.data?.retentionSeconds || 172800) * 1000; return [...new Map([...(latest.data?.events || []), ...older].filter(row => new Date(row.createdAt).getTime() >= cutoff).map(row => [row.id, row])).values()] }, [latest.data, older])
  const loadOlder = async () => { if (!cursor) return; try { const request = new URLSearchParams(params); request.set('limit', '100'); request.set('cursor', cursor); const page = await api.printerLogs(request); setOlder(previous => [...previous, ...page.events]); setCursor(page.nextCursor || '') } catch (error) { toast.error(error instanceof Error ? error.message : 'Could not load older events') } }
  const refresh = () => { setOlder([]); setCursor(''); latest.refetch() }
  return { ...latest, rows, cursor, loadOlder, refresh }
}

type SystemDraft = { selected: Set<string>; q: string; printerId: string; runUid: string; from: string; to: string }
function systemDraft(params: URLSearchParams): SystemDraft { return { selected: new Set((params.get('levels') || 'debug,info,warn,error').split(',')), q: params.get('q') || '', printerId: params.get('printerId') || '', runUid: params.get('runUid') || '', from: localDateTimeValue(params.get('from') || ''), to: localDateTimeValue(params.get('to') || '') } }

export function SystemLogsPage() {
  const [params, setParams] = useSearchParams(); const [draft, setDraft] = useState(() => systemDraft(params)); const feed = useSystemFeed(params)
  useEffect(() => setDraft(systemDraft(params)), [params])
  const apply = (event: FormEvent) => { event.preventDefault(); if (!draft.selected.size) return toast.error('Select at least one log level'); const next = new URLSearchParams(); if (draft.selected.size !== 4) next.set('levels', levels.filter(level => draft.selected.has(level)).join(',')); for (const [name, value] of [['q', draft.q], ['printerId', draft.printerId], ['runUid', draft.runUid]] as const) if (value.trim()) next.set(name, value.trim()); if (draft.from) next.set('from', urlTimeValue(draft.from)); if (draft.to) next.set('to', urlTimeValue(draft.to)); setParams(next) }
  return <><PageHeader title="System Logs" description="Every structured application record emitted by the configured logger, retained for two hours." actions={<Button variant="outline" onClick={feed.refresh}><RefreshCw className={feed.isFetching ? 'animate-spin' : ''} />Refresh</Button>} />
    <form onSubmit={apply} className="mb-4 grid gap-3 rounded-xl border bg-card p-4 md:grid-cols-3 xl:grid-cols-6"><fieldset className="flex flex-wrap gap-3 md:col-span-3 xl:col-span-2"><legend className="mb-2 text-xs text-muted-foreground">Levels</legend>{levels.map(level => <Label key={level} className="flex items-center gap-1.5"><Checkbox checked={draft.selected.has(level)} onCheckedChange={checked => setDraft(previous => { const selected = new Set(previous.selected); if (checked === true) selected.add(level); else selected.delete(level); return { ...previous, selected } })} />{level}</Label>)}</fieldset>
      <FilterInput label="Search" value={draft.q} onChange={q => setDraft({ ...draft, q })} placeholder="timeout" /><FilterInput label="Printer ID" value={draft.printerId} onChange={printerId => setDraft({ ...draft, printerId })} placeholder="kitchen" /><FilterInput label="Print Run UID" value={draft.runUid} onChange={runUid => setDraft({ ...draft, runUid })} placeholder="run_…" /><FilterInput label="From" type="datetime-local" value={draft.from} onChange={from => setDraft({ ...draft, from })} /><FilterInput label="To" type="datetime-local" value={draft.to} onChange={to => setDraft({ ...draft, to })} /><div className="flex items-end gap-2"><Button type="submit"><Search />Apply</Button><Button type="button" variant="outline" onClick={() => setParams({})}><X />Clear</Button></div></form>
    {feed.isLoading && <LoadingState rows={6} />}{feed.isError && <ErrorState error={feed.error} retry={() => feed.refetch()} />}
    {feed.data && <div className="overflow-x-auto rounded-xl border"><Table><TableHeader><TableRow><TableHead>Time</TableHead><TableHead>Level</TableHead><TableHead>Message</TableHead><TableHead>Fields</TableHead><TableHead>Attributes</TableHead></TableRow></TableHeader><TableBody>{feed.rows.map(record => <TableRow key={record.id}><TableCell className="whitespace-nowrap">{formatDate(record.createdAt)}</TableCell><TableCell><StatusBadge value={record.level} /></TableCell><TableCell>{record.message}</TableCell><TableCell className="max-w-lg">{Object.entries(record.attributes || {}).slice(0, 6).map(([key, value]) => <Badge key={key} variant="secondary" className="mr-1 mb-1 max-w-72 truncate"><b>{key}</b>={formatField(value)}</Badge>)}</TableCell><TableCell><Collapsible><CollapsibleTrigger render={<Button size="sm" variant="ghost" />}><ChevronDown />Raw</CollapsibleTrigger><CollapsibleContent><pre className="mt-2 max-h-80 min-w-72 overflow-auto rounded-lg bg-muted p-3 text-xs whitespace-pre-wrap">{JSON.stringify(record.attributes || {}, null, 2)}</pre></CollapsibleContent></Collapsible></TableCell></TableRow>)}{!feed.rows.length && <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No matching system logs</TableCell></TableRow>}</TableBody></Table></div>}
    {feed.cursor && <div className="mt-4 text-center"><Button variant="outline" onClick={feed.loadOlder}>Load older</Button></div>}
  </>
}

type PrinterDraft = { printerId: string; event: string; runUid: string; q: string; from: string; to: string }
function printerDraft(params: URLSearchParams): PrinterDraft { return { printerId: params.get('printerId') || '', event: params.get('event') || '', runUid: params.get('runUid') || '', q: params.get('q') || '', from: localDateTimeValue(params.get('from') || ''), to: localDateTimeValue(params.get('to') || '') } }

export function PrinterLogsPage() {
  const [params, setParams] = useSearchParams(); const [draft, setDraft] = useState(() => printerDraft(params)); const feed = usePrinterFeed(params)
  const status = useQuery({ queryKey: queryKeys.status, queryFn: ({ signal }) => api.status(signal), refetchInterval: 2000 })
  useEffect(() => setDraft(printerDraft(params)), [params])
  const configured = new Set(status.data?.printers.map(item => item.printer.id) || [])
  const apply = (event: FormEvent) => { event.preventDefault(); const next = new URLSearchParams(); for (const [name, value] of Object.entries(draft)) if (value.trim()) next.set(name, name === 'from' || name === 'to' ? urlTimeValue(value) : value.trim()); setParams(next) }
  return <><PageHeader title="Printer Logs" description="Printer connections, configuration changes, and Print Run events retained with Job history." actions={<Button variant="outline" onClick={feed.refresh}><RefreshCw className={feed.isFetching ? 'animate-spin' : ''} />Refresh</Button>} />
    <form onSubmit={apply} className="mb-4 grid gap-3 rounded-xl border bg-card p-4 md:grid-cols-3 xl:grid-cols-6"><label className="space-y-1 text-xs text-muted-foreground">Printer ID<select className={inputClass} value={draft.printerId} onChange={event => setDraft({ ...draft, printerId: event.target.value })}><option value="">All printers</option>{status.data?.printers.slice().sort((a, b) => a.printer.displayName.localeCompare(b.printer.displayName)).map(item => <option key={item.printer.id} value={item.printer.id}>{item.printer.displayName === item.printer.id ? item.printer.id : `${item.printer.displayName} (${item.printer.id})`}</option>)}{draft.printerId && !configured.has(draft.printerId) && <option value={draft.printerId}>Archived: {draft.printerId}</option>}</select></label>
      <label className="space-y-1 text-xs text-muted-foreground">Event<select className={inputClass} value={draft.event} onChange={event => setDraft({ ...draft, event: event.target.value })}><option value="">All events</option>{events.map(value => <option key={value} value={value}>{value.replaceAll('_', '.')}</option>)}</select></label><FilterInput label="Print Run UID" value={draft.runUid} onChange={runUid => setDraft({ ...draft, runUid })} placeholder="run_…" /><FilterInput label="Search" value={draft.q} onChange={q => setDraft({ ...draft, q })} placeholder="timeout" /><FilterInput label="From" type="datetime-local" value={draft.from} onChange={from => setDraft({ ...draft, from })} /><FilterInput label="To" type="datetime-local" value={draft.to} onChange={to => setDraft({ ...draft, to })} /><div className="flex items-end gap-2"><Button type="submit"><Search />Apply</Button><Button type="button" variant="outline" onClick={() => setParams({})}><X />Clear</Button></div></form>
    {feed.isLoading && <LoadingState rows={6} />}{feed.isError && <ErrorState error={feed.error} retry={() => feed.refetch()} />}
    {feed.data && <div className="overflow-x-auto rounded-xl border"><Table><TableHeader><TableRow><TableHead>Time</TableHead><TableHead>Event</TableHead><TableHead>Printer</TableHead><TableHead>Print Run</TableHead><TableHead>Message</TableHead></TableRow></TableHeader><TableBody>{feed.rows.map(entry => <TableRow key={entry.id}><TableCell className="whitespace-nowrap">{formatDate(entry.createdAt)}</TableCell><TableCell>{entry.type}</TableCell><TableCell>{entry.printerId || '—'}</TableCell><TableCell className="font-mono text-xs">{entry.runUid || '—'}</TableCell><TableCell>{entry.message || '—'}</TableCell></TableRow>)}{!feed.rows.length && <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No matching printer events</TableCell></TableRow>}</TableBody></Table></div>}
    {feed.cursor && <div className="mt-4 text-center"><Button variant="outline" onClick={feed.loadOlder}>Load older</Button></div>}
  </>
}

function FilterInput({ label, value, onChange, placeholder, type = 'text' }: { label: string; value: string; onChange: (value: string) => void; placeholder?: string; type?: string }) {
  return <Label className="space-y-1 text-xs text-muted-foreground">{label}<Input type={type} step={type === 'datetime-local' ? 1 : undefined} value={value} placeholder={placeholder} onChange={event => onChange(event.target.value)} /></Label>
}
