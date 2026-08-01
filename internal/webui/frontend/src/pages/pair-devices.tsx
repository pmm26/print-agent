import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bluetooth, ExternalLink, Plus, Radio, RefreshCw, Square } from 'lucide-react'
import { useNavigate } from 'react-router'
import { toast } from 'sonner'
import { api, queryKeys } from '@/lib/api'
import type { BluetoothDevice, PrinterStatus } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge } from '@/components/shared'

const normalizeAddress = (value?: string) => (value || '').trim().replaceAll('-', ':').toUpperCase()
const endpointAddress = (endpoint?: string) => { const value = endpoint || ''; const index = value.indexOf('://'); return index >= 0 ? normalizeAddress(value.slice(index + 3)) : '' }
function configuredFor(device: BluetoothDevice, printers: PrinterStatus[]) {
  return printers.filter(status => {
    const printer = status.printer
    if (device.endpoint && printer.endpoint.toLowerCase() === device.endpoint.toLowerCase()) return true
    return normalizeAddress(device.address) !== '' && (normalizeAddress(printer.deviceAddress) || endpointAddress(printer.endpoint)) === normalizeAddress(device.address)
  })
}

export default function PairDevicesPage() {
  const navigate = useNavigate(); const queryClient = useQueryClient(); const stopTimer = useRef<number | undefined>(undefined)
  const [scanning, setScanning] = useState(false); const [pins, setPins] = useState<Record<string, string>>({}); const [pairing, setPairing] = useState<Set<string>>(new Set())
  const status = useQuery({ queryKey: queryKeys.status, queryFn: ({ signal }) => api.status(signal), refetchInterval: 2000 })
  const devices = useQuery({ queryKey: queryKeys.bluetooth, queryFn: ({ signal }) => api.bluetoothDevices(signal), refetchInterval: 2000 })
  const printers = status.data?.printers || []
  const stop = useMutation({ mutationFn: () => api.mutate('/bluetooth/discovery/stop'), onSettled: () => { window.clearTimeout(stopTimer.current); setScanning(false); queryClient.invalidateQueries({ queryKey: queryKeys.bluetooth }) } })
  const start = useMutation({ mutationFn: () => api.mutate('/bluetooth/discovery/start'), onSuccess: () => { setScanning(true); queryClient.invalidateQueries({ queryKey: queryKeys.bluetooth }); stopTimer.current = window.setTimeout(() => stop.mutate(), 20_000) }, onError: error => toast.error(error.message) })
  useEffect(() => () => window.clearTimeout(stopTimer.current), [])
  const pair = async (device: BluetoothDevice) => {
    setPairing(previous => new Set(previous).add(device.address))
    try {
      if (scanning) await stop.mutateAsync()
      const result = await api.mutate<{ ready: boolean }>(`/bluetooth/devices/${encodeURIComponent(device.address)}/pair`, 'POST', { pin: pins[device.address] || '' })
      toast.success(result.ready ? `${device.address} is ready to add.` : `Paired ${device.address}, but no supported endpoint was found.`)
    } catch (error) { toast.error(error instanceof Error ? error.message : 'Pairing failed') }
    finally { setPairing(previous => { const next = new Set(previous); next.delete(device.address); return next }); queryClient.invalidateQueries({ queryKey: queryKeys.bluetooth }) }
  }
  const list = useMemo(() => devices.data?.devices || [], [devices.data])
  return <><PageHeader title="Pair Bluetooth devices" description="Scan for nearby printers, pair when required, then add each ready device." actions={<><Button onClick={() => start.mutate()} disabled={scanning || start.isPending}><Radio />{scanning ? 'Scanning…' : 'Scan for devices'}</Button><Button variant="outline" onClick={() => stop.mutate()} disabled={!scanning || stop.isPending}><Square />Stop scan</Button></>} />
    <p className="mb-4 text-sm text-muted-foreground">{scanning ? 'Scanning… Keep each printer powered on and in pairing mode.' : 'Paired and ready devices remain available after scanning stops.'}</p>
    {devices.isLoading && <LoadingState rows={4} />}{devices.isError && <ErrorState error={devices.error} retry={() => devices.refetch()} />}
    {devices.data && !devices.data.supported && <Card><CardContent className="flex flex-col items-start gap-3 pt-6"><p>Use your operating system Bluetooth settings to pair printers, then add the discovered endpoint under Printers.</p><div className="flex flex-wrap gap-2"><Button variant="outline" onClick={() => api.mutate('/system/open-bluetooth-settings').catch(error => toast.error(error.message))}><ExternalLink />Open Bluetooth settings</Button><Button onClick={() => navigate('/setup/printers?newEndpoint=')}><Plus />Add printer</Button></div></CardContent></Card>}
    {devices.data?.supported && !list.length && <EmptyState title={scanning ? 'Looking for devices…' : 'No devices found'} detail={scanning ? 'Nearby Bluetooth devices will appear automatically.' : 'Select Scan for devices to begin.'} />}
    <div className="grid gap-3">{list.map(device => {
      const configured = configuredFor(device, printers); const busy = pairing.has(device.address)
      return <Card key={device.address}><CardHeader className="flex-row items-start justify-between gap-4"><div><CardTitle className="flex flex-wrap items-center gap-2"><Bluetooth className="size-4" />{device.name || 'Unknown device'} {device.paired ? <StatusBadge value="connected" /> : device.endpoint ? <StatusBadge value="fulfilled" /> : <StatusBadge value="disabled" />}{device.connected && <StatusBadge value="printing" />}</CardTitle><p className="mt-2 font-mono text-xs text-muted-foreground">{device.address}{device.isPrinter ? ' · likely printer' : ''}</p></div></CardHeader><CardContent className="flex flex-wrap items-center justify-end gap-2">
        {configured.length ? <><span className="mr-auto text-sm text-muted-foreground">Added as {configured.map(item => item.printer.displayName || item.printer.id).join(', ')}</span><Button variant="outline" onClick={() => navigate(`/setup/printers?printerId=${encodeURIComponent(configured[0].printer.id)}`)}>View printer</Button></> : device.endpoint ? <Button onClick={() => navigate(`/setup/printers?newEndpoint=${encodeURIComponent(device.endpoint || '')}&deviceAddress=${encodeURIComponent(device.address)}`)}><Plus />Add printer</Button> : device.paired ? <span className="text-sm text-muted-foreground">Paired, but no supported printer endpoint was found.</span> : <><Input className="w-44" inputMode="numeric" maxLength={16} placeholder="PIN (default 0000)" value={pins[device.address] || ''} onChange={event => setPins(previous => ({ ...previous, [device.address]: event.target.value }))} /><Button disabled={busy} onClick={() => pair(device)}><RefreshCw className={busy ? 'animate-spin' : ''} />{busy ? 'Pairing…' : 'Pair device'}</Button></>}
      </CardContent></Card>
    })}</div>
  </>
}
