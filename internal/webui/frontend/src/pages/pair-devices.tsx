import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bluetooth, Copy, ExternalLink, Plus, Radio, RefreshCw, Square, Unplug, Trash2 } from 'lucide-react'
import { useNavigate } from 'react-router'
import { toast } from 'sonner'
import { api, queryKeys } from '@/lib/api'
import type { BluetoothDevice, ConnectionPreference, PrinterStatus } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge } from '@/components/shared'

const selectClass = 'h-8 rounded-lg border border-input bg-background px-2.5 text-sm outline-none focus:ring-2 focus:ring-ring/50'
const ubuntuStopCommand = "pkill -f '^/usr/bin/gnome-control-center bluetooth$'"
const normalizeAddress = (value?: string) => (value || '').trim().replaceAll('-', ':').toUpperCase()
const endpointAddress = (endpoint?: string) => { const value = endpoint || ''; const index = value.indexOf('://'); return index >= 0 ? normalizeAddress(value.slice(index + 3)) : '' }
const activeConnectionType = (status: PrinterStatus) => ['connected', 'printing'].includes(status.state) ? status.endpoint.split('://')[0] || 'unknown' : 'none'
function configuredFor(device: BluetoothDevice, printers: PrinterStatus[]) {
  return printers.filter(status => {
    const printer = status.printer
    if (device.endpoint && printer.endpoint.toLowerCase() === device.endpoint.toLowerCase()) return true
    return normalizeAddress(device.address) !== '' && (normalizeAddress(printer.deviceAddress) || endpointAddress(printer.endpoint)) === normalizeAddress(device.address)
  })
}

export default function PairDevicesPage() {
  const navigate = useNavigate(); const queryClient = useQueryClient(); const stopTimer = useRef<number | undefined>(undefined)
  const [scanning, setScanning] = useState(false); const [scanType, setScanType] = useState<ConnectionPreference>('auto')
  const [pins, setPins] = useState<Record<string, string>>({}); const [protocols, setProtocols] = useState<Record<string, ConnectionPreference | ''>>({}); const [pairing, setPairing] = useState<Set<string>>(new Set())
  const status = useQuery({ queryKey: queryKeys.status, queryFn: ({ signal }) => api.status(signal), refetchInterval: 2000 })
  const devices = useQuery({ queryKey: queryKeys.bluetooth, queryFn: ({ signal }) => api.bluetoothDevices(signal), refetchInterval: 2000 })
  const printers = status.data?.printers || []
  const refreshDevices = async () => Promise.all([queryClient.invalidateQueries({ queryKey: queryKeys.bluetooth }), queryClient.invalidateQueries({ queryKey: queryKeys.status })])
  const stop = useMutation({ mutationFn: () => api.mutate('/bluetooth/discovery/stop'), onSettled: () => { window.clearTimeout(stopTimer.current); setScanning(false); queryClient.invalidateQueries({ queryKey: queryKeys.bluetooth }) } })
  const start = useMutation({ mutationFn: () => api.mutate(`/bluetooth/discovery/start?connectionType=${encodeURIComponent(scanType)}`), onSuccess: () => { setScanning(true); queryClient.invalidateQueries({ queryKey: queryKeys.bluetooth }); stopTimer.current = window.setTimeout(() => stop.mutate(), 20_000) }, onError: error => toast.error(error.message) })
  const manage = useMutation({
    mutationFn: ({ path, method }: { path: string; method: string }) => api.mutate(path, method),
    onSuccess: (_, variables) => { toast.success(variables.method === 'DELETE' ? 'Bluetooth device forgotten' : 'Bluetooth device disconnected'); refreshDevices() },
    onError: error => toast.error(error.message),
  })
  useEffect(() => () => window.clearTimeout(stopTimer.current), [])

  const prepareProtocolForPairing = async (connectionType: ConnectionPreference) => {
    window.clearTimeout(stopTimer.current)
    await api.mutate('/bluetooth/discovery/stop').catch(() => undefined)
    setScanning(false)
    if (connectionType === 'auto') return
    await api.mutate(`/bluetooth/discovery/start?connectionType=${encodeURIComponent(connectionType)}`)
    setScanning(true)
    await new Promise(resolve => window.setTimeout(resolve, 2500))
    await api.mutate('/bluetooth/discovery/stop')
    setScanning(false)
  }
  const pair = async (device: BluetoothDevice) => {
    const connectionType = protocols[device.address]
    if (!connectionType) { toast.error('Choose Auto, RFCOMM, or BLE before pairing'); return }
    setPairing(previous => new Set(previous).add(device.address))
    try {
      await prepareProtocolForPairing(connectionType)
      const result = await api.mutate<{ ready: boolean }>(`/bluetooth/devices/${encodeURIComponent(device.address)}/pair`, 'POST', { pin: pins[device.address] || '', connectionType })
      toast.success(result.ready ? `${device.address} is ready to add.` : `Paired ${device.address}, but no supported endpoint was found.`)
      setPins(previous => { const next = { ...previous }; delete next[device.address]; return next })
      setProtocols(previous => { const next = { ...previous }; delete next[device.address]; return next })
    } catch (error) { toast.error(error instanceof Error ? error.message : 'Pairing failed') }
    finally { setPairing(previous => { const next = new Set(previous); next.delete(device.address); return next }); refreshDevices() }
  }
  const list = useMemo(() => devices.data?.devices || [], [devices.data])
  return <><PageHeader title="Pair Bluetooth devices" description="Scan for nearby printers, choose their Bluetooth protocol, then add each ready device." actions={<><div className="flex items-center gap-2"><Label htmlFor="scan-connection-type">Scan type</Label><select id="scan-connection-type" className={selectClass} value={scanType} onChange={event => setScanType(event.target.value as ConnectionPreference)}><option value="auto">Auto</option><option value="rfcomm">RFCOMM (Classic)</option><option value="ble">BLE</option></select></div><Button onClick={() => start.mutate()} disabled={scanning || start.isPending}><Radio />{scanning ? 'Scanning…' : 'Scan for devices'}</Button><Button variant="outline" onClick={() => stop.mutate()} disabled={!scanning || stop.isPending}><Square />Stop scan</Button></>} />
    <p className="mb-4 text-sm text-muted-foreground">{scanning ? 'Scanning… Keep each printer powered on and in pairing mode.' : 'Paired and ready devices remain available after scanning stops.'}</p>
    {devices.data?.supported && <Card className="mb-4"><CardHeader><CardTitle className="text-base">Ubuntu Bluetooth Settings can interfere with setup</CardTitle></CardHeader><CardContent className="space-y-3"><p className="text-sm text-muted-foreground">Close the GNOME Bluetooth panel before a protocol-specific scan. This command closes only that panel; it does not stop BlueZ.</p><div className="flex flex-wrap items-center gap-2"><code className="rounded-md bg-muted px-2 py-1 text-xs">{ubuntuStopCommand}</code><Button variant="outline" size="sm" onClick={() => navigator.clipboard.writeText(ubuntuStopCommand).then(() => toast.success('Ubuntu command copied')).catch(() => toast.error('Could not copy command'))}><Copy />Copy</Button></div></CardContent></Card>}
    {devices.isLoading && <LoadingState rows={4} />}{devices.isError && <ErrorState error={devices.error} retry={() => devices.refetch()} />}
    {devices.data && !devices.data.supported && <Card><CardContent className="flex flex-col items-start gap-3 pt-6"><p>Use your operating system Bluetooth settings to pair printers, then add the discovered endpoint under Printers.</p><div className="flex flex-wrap gap-2"><Button variant="outline" onClick={() => api.mutate('/system/open-bluetooth-settings').catch(error => toast.error(error.message))}><ExternalLink />Open Bluetooth settings</Button><Button onClick={() => navigate('/setup/printers?newEndpoint=')}><Plus />Add printer</Button></div></CardContent></Card>}
    {devices.data?.supported && !list.length && <EmptyState title={scanning ? 'Looking for devices…' : 'No devices found'} detail={scanning ? 'Nearby Bluetooth devices will appear automatically.' : 'Select Scan for devices to begin.'} />}
    <div className="grid gap-3">{list.map(device => {
      const configured = configuredFor(device, printers); const busy = pairing.has(device.address); const supported = device.supportedConnectionTypes || []
      const activeTypes = [...new Set(configured.map(activeConnectionType).filter(value => value !== 'none'))]
      return <Card key={device.address}><CardHeader className="flex-row items-start justify-between gap-4"><div><CardTitle className="flex flex-wrap items-center gap-2"><Bluetooth className="size-4" />{device.name || 'Unknown device'} {device.paired ? <StatusBadge value="connected" /> : device.endpoint ? <StatusBadge value="fulfilled" /> : <StatusBadge value="disabled" />}{device.connected && <StatusBadge value="printing" />}</CardTitle><p className="mt-2 font-mono text-xs text-muted-foreground">{device.address}{device.isPrinter ? ' · likely printer' : ''}{activeTypes.length ? ` · active: ${activeTypes.join(', ')}` : ''}</p></div></CardHeader><CardContent className="flex flex-wrap items-center justify-end gap-2">
        {configured.length > 0 && <><span className="mr-auto text-sm text-muted-foreground">Added as {configured.map(item => item.printer.displayName || item.printer.id).join(', ')}</span><Button variant="outline" onClick={() => navigate(`/setup/printers?printerId=${encodeURIComponent(configured[0].printer.id)}`)}>View printer</Button></>}
        {!configured.length && !device.paired && <><select aria-label={`Connection type for ${device.address}`} className={selectClass} value={protocols[device.address] || ''} onChange={event => setProtocols(previous => ({ ...previous, [device.address]: event.target.value as ConnectionPreference }))}><option value="" disabled>Choose protocol…</option><option value="auto">Auto</option><option value="rfcomm" disabled={!supported.includes('rfcomm')}>RFCOMM (Classic)</option><option value="ble" disabled={!supported.includes('ble')}>BLE</option></select><Input className="w-44" inputMode="numeric" maxLength={16} placeholder="PIN if required" value={pins[device.address] || ''} onChange={event => setPins(previous => ({ ...previous, [device.address]: event.target.value }))} /><Button disabled={busy} onClick={() => pair(device)}><RefreshCw className={busy ? 'animate-spin' : ''} />{busy ? 'Pairing…' : 'Pair device'}</Button></>}
        {!configured.length && device.endpoint && <Button onClick={() => { const connectionPreference = device.endpoint?.startsWith('rfcomm://') ? 'rfcomm' : device.endpoint?.startsWith('ble://') ? 'ble' : 'auto'; navigate(`/setup/printers?newEndpoint=${encodeURIComponent(device.endpoint || '')}&deviceAddress=${encodeURIComponent(device.address)}&connectionPreference=${connectionPreference}`) }}><Plus />Add printer</Button>}
        {!configured.length && device.paired && !device.endpoint && <span className="mr-auto text-sm text-muted-foreground">Paired, but no supported printer endpoint was found.</span>}
        {!configured.length && device.paired && <Button variant="destructive" disabled={manage.isPending} onClick={() => { if (window.confirm(`Forget ${device.address}? It will need to be paired again.`)) manage.mutate({ path: `/bluetooth/devices/${encodeURIComponent(device.address)}`, method: 'DELETE' }) }}><Trash2 />Forget</Button>}
        {device.connected && <Button variant="outline" disabled={manage.isPending} onClick={() => { if (window.confirm('Disconnect this device now? Auto-reconnect may connect it again immediately.')) manage.mutate({ path: `/bluetooth/devices/${encodeURIComponent(device.address)}/disconnect`, method: 'POST' }) }}><Unplug />Disconnect now</Button>}
      </CardContent></Card>
    })}</div>
  </>
}
