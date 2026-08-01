import type { ReactNode } from 'react'
import { AlertCircle, CheckCircle2, CircleDashed, LoaderCircle, RefreshCw, XCircle } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'

export function PageHeader({ title, description, actions }: { title: string; description: string; actions?: ReactNode }) {
  return <div className="mb-5 flex flex-col justify-between gap-3 sm:flex-row sm:items-start">
    <div><h1 className="text-2xl font-semibold tracking-tight">{title}</h1><p className="mt-1 text-sm text-muted-foreground">{description}</p></div>
    {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
  </div>
}

const good = new Set(['connected', 'running', 'completed', 'transmitted', 'fulfilled'])
const bad = new Set(['error', 'failed', 'uncertain', 'attention_required', 'unreachable'])
const waiting = new Set(['connecting', 'reconnecting', 'printing', 'queued', 'processing'])

export function StatusBadge({ value, className }: { value: string; className?: string }) {
  const Icon = good.has(value) ? CheckCircle2 : bad.has(value) ? AlertCircle : waiting.has(value) ? LoaderCircle : value === 'cancelled' || value === 'disabled' ? XCircle : CircleDashed
  return <Badge variant="outline" className={cn(
    'gap-1 capitalize', good.has(value) && 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
    bad.has(value) && 'border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-300',
    waiting.has(value) && 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300', className,
  )}><Icon className={cn('size-3', waiting.has(value) && 'animate-spin')} />{value.replaceAll('_', ' ')}</Badge>
}

export function LoadingState({ rows = 3 }: { rows?: number }) {
  return <div className="space-y-3" aria-label="Loading">{Array.from({ length: rows }, (_, index) => <Skeleton key={index} className="h-16 w-full" />)}</div>
}

export function EmptyState({ title, detail, action }: { title: string; detail?: string; action?: ReactNode }) {
  return <Card className="border-dashed"><CardContent className="flex min-h-36 flex-col items-center justify-center gap-2 text-center">
    <CircleDashed className="size-7 text-muted-foreground" /><h2 className="font-medium">{title}</h2>
    {detail && <p className="max-w-xl text-sm text-muted-foreground">{detail}</p>}{action}
  </CardContent></Card>
}

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  const message = error instanceof Error ? error.message : 'Something went wrong'
  return <Card className="border-destructive/40"><CardContent className="flex min-h-32 flex-col items-center justify-center gap-3 text-center text-destructive">
    <AlertCircle className="size-7" /><p>{message}</p>{retry && <Button variant="outline" onClick={retry}><RefreshCw />Try again</Button>}
  </CardContent></Card>
}

export function JsonViewer({ value, className }: { value: unknown; className?: string }) {
  return <pre className={cn('max-h-96 overflow-auto rounded-lg border bg-muted/50 p-4 text-xs whitespace-pre-wrap break-words', className)}>{JSON.stringify(value, null, 2)}</pre>
}

export function formatField(value: unknown) {
  return typeof value === 'string' ? value : JSON.stringify(value)
}
