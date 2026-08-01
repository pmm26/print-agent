import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, RadioTower, Save, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { api, queryKeys } from '@/lib/api'
import { formatDate } from '@/lib/format'
import type { PairingCode, TokenInfo } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from '@/components/ui/alert-dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { ErrorState, LoadingState, PageHeader } from '@/components/shared'

export default function POSPairingPage() {
  const queryClient = useQueryClient(); const [origin, setOrigin] = useState(''); const [code, setCode] = useState<PairingCode | null>(null); const [revoke, setRevoke] = useState<TokenInfo | null>(null)
  const settings = useQuery({ queryKey: [...queryKeys.pairing, 'settings'], queryFn: ({ signal }) => api.settings(signal) })
  const tokens = useQuery({ queryKey: [...queryKeys.pairing, 'tokens'], queryFn: ({ signal }) => api.tokens(signal) })
  useEffect(() => { if (settings.data) setOrigin(settings.data.allowedOrigin || '') }, [settings.data])
  useEffect(() => { if (!code) return; const delay = Math.max(0, new Date(code.expiresAt).getTime() - Date.now()); const timer = window.setTimeout(() => setCode(null), delay); return () => window.clearTimeout(timer) }, [code])
  const save = useMutation({ mutationFn: () => api.mutate('/admin/settings', 'PUT', { allowedOrigin: origin.trim() }), onSuccess: () => { toast.success('Allowed origin saved'); queryClient.invalidateQueries({ queryKey: queryKeys.pairing }) }, onError: error => toast.error(error.message) })
  const generate = useMutation({ mutationFn: api.pairingCode, onSuccess: result => { setCode(result); toast.success('Code valid for five minutes') }, onError: error => toast.error(error.message) })
  const revokeToken = useMutation({ mutationFn: (id: string) => api.mutate(`/admin/tokens/${encodeURIComponent(id)}`, 'DELETE'), onSuccess: () => { toast.success('Token revoked'); setRevoke(null); queryClient.invalidateQueries({ queryKey: queryKeys.pairing }) }, onError: error => toast.error(error.message) })
  return <><PageHeader title="POS Pairing" description="Authorize a hosted POS origin to submit Jobs to this local agent." />
    {(settings.isLoading || tokens.isLoading) && <LoadingState rows={3} />}{settings.isError && <ErrorState error={settings.error} retry={() => settings.refetch()} />}
    <div className="grid gap-4 lg:grid-cols-2"><Card><CardHeader><CardTitle className="flex items-center gap-2"><RadioTower className="size-4" />Allowed POS origin</CardTitle><CardDescription>Exact HTTP(S) origin of the hosted POS page.</CardDescription></CardHeader><CardContent className="space-y-3"><Label htmlFor="pos-origin">Origin</Label><Input id="pos-origin" placeholder="https://pos.example.com" value={origin} onChange={event => setOrigin(event.target.value)} /><Button onClick={() => save.mutate()} disabled={save.isPending}><Save />{save.isPending ? 'Saving…' : 'Save origin'}</Button></CardContent></Card>
      <Card><CardHeader><CardTitle className="flex items-center gap-2"><KeyRound className="size-4" />One-time pairing code</CardTitle><CardDescription>Enter this code in the POS setup screen within five minutes.</CardDescription></CardHeader><CardContent className="space-y-3"><Button onClick={() => generate.mutate()} disabled={generate.isPending}>Generate pairing code</Button>{code && <div className="rounded-xl border bg-muted/40 p-5 text-center"><p className="font-mono text-4xl font-bold tracking-[.3em]">{code.code}</p><p className="mt-2 text-xs text-muted-foreground">Expires {formatDate(code.expiresAt)}</p></div>}</CardContent></Card>
    </div>
    <Card className="mt-4"><CardHeader><CardTitle>Client tokens</CardTitle><CardDescription>Only token metadata is shown. Clear token values are never retained.</CardDescription></CardHeader><CardContent className="overflow-x-auto px-0 pb-0"><Table><TableHeader><TableRow><TableHead>Label</TableHead><TableHead>Origin</TableHead><TableHead>Created</TableHead><TableHead>Last used</TableHead><TableHead /></TableRow></TableHeader><TableBody>{tokens.data?.map(token => <TableRow key={token.id}><TableCell>{token.label}</TableCell><TableCell>{token.allowedOrigin}</TableCell><TableCell>{formatDate(token.createdAt)}</TableCell><TableCell>{formatDate(token.lastUsedAt)}</TableCell><TableCell>{token.revoked ? <span className="text-muted-foreground">Revoked</span> : <Button variant="destructive" size="sm" onClick={() => setRevoke(token)}><Trash2 />Revoke</Button>}</TableCell></TableRow>)}{tokens.data && !tokens.data.length && <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No tokens issued</TableCell></TableRow>}</TableBody></Table></CardContent></Card>
    <AlertDialog open={!!revoke} onOpenChange={open => !open && setRevoke(null)}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>Revoke {revoke?.label}?</AlertDialogTitle><AlertDialogDescription>The POS client will immediately lose access and must be paired again.</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>Back</AlertDialogCancel><AlertDialogAction variant="destructive" disabled={!revoke || revokeToken.isPending} onClick={() => revoke && revokeToken.mutate(revoke.id)}>Revoke token</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
  </>
}
