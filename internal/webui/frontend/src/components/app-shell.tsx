import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, Outlet, useLocation, useNavigate } from 'react-router'
import { Activity, Bluetooth, ChevronDown, ClipboardList, FileClock, FlaskConical, Menu, Printer, RadioTower, ScrollText, Settings, Stethoscope } from 'lucide-react'
import { api, queryKeys } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Sheet, SheetClose, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from '@/components/ui/sheet'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { StatusBadge } from '@/components/shared'
import { JobDialogProvider } from '@/components/job-dialog'
import { cn } from '@/lib/utils'

const groups = [
  { label: 'Setup', items: [
    { to: '/setup/pair', label: 'Pair devices', icon: Bluetooth },
    { to: '/setup/printers', label: 'Printers', icon: Printer },
    { to: '/setup/pos', label: 'POS Pairing', icon: RadioTower },
  ] },
  { label: 'Operations', items: [
    { to: '/operations/jobs', label: 'Jobs', icon: ClipboardList },
    { to: '/operations/queue', label: 'Queue', icon: FileClock },
    { to: '/operations/printer-logs', label: 'Printer Logs', icon: ScrollText },
  ] },
  { label: 'System', items: [
    { to: '/system/diagnostics', label: 'Diagnostics', icon: Stethoscope },
    { to: '/system/logs', label: 'System Logs', icon: Activity },
  ] },
  { label: 'Dev', items: [{ to: '/dev/pos-simulator', label: 'POS Simulator', icon: FlaskConical }] },
]

function ThemeSync() {
  useEffect(() => {
    const query = window.matchMedia('(prefers-color-scheme: dark)')
    const sync = () => document.documentElement.classList.toggle('dark', query.matches)
    sync(); query.addEventListener('change', sync)
    return () => query.removeEventListener('change', sync)
  }, [])
  return null
}

function DesktopNav() {
  const location = useLocation(); const navigate = useNavigate()
  return <nav aria-label="Dashboard sections" className="hidden items-center gap-1 md:flex">{groups.map(group => {
    const active = group.items.some(item => location.pathname === item.to)
    return <DropdownMenu key={group.label}><DropdownMenuTrigger render={<Button variant={active ? 'secondary' : 'ghost'} size="sm" />}>
      {group.label}<ChevronDown className="size-3.5" />
    </DropdownMenuTrigger><DropdownMenuContent className="w-48">{group.items.map(item => <DropdownMenuItem key={item.to} onClick={() => navigate(item.to)} className={cn(location.pathname === item.to && 'bg-accent')}>
      <item.icon />{item.label}
    </DropdownMenuItem>)}</DropdownMenuContent></DropdownMenu>
  })}</nav>
}

function MobileNav() {
  const location = useLocation()
  return <Sheet><SheetTrigger render={<Button variant="outline" size="icon" className="md:hidden" />}><Menu /><span className="sr-only">Open navigation</span></SheetTrigger>
    <SheetContent side="left" className="w-72"><SheetHeader><SheetTitle>Print Agent</SheetTitle></SheetHeader><nav className="space-y-5 px-4">{groups.map(group => <div key={group.label}>
      <p className="mb-1 px-2 text-xs font-medium text-muted-foreground">{group.label}</p>{group.items.map(item => <SheetClose key={item.to} render={<Link to={item.to} className={cn('flex items-center gap-2 rounded-lg px-2 py-2 text-sm', location.pathname === item.to ? 'bg-accent font-medium' : 'hover:bg-accent/60')} />}><item.icon className="size-4" />{item.label}</SheetClose>)}
    </div>)}</nav></SheetContent></Sheet>
}

export function AppShell() {
  const status = useQuery({ queryKey: queryKeys.status, queryFn: ({ signal }) => api.status(signal), refetchInterval: 2000 })
  const state = status.isError ? 'unreachable' : status.data ? 'running' : 'connecting'
  const persistence = status.data?.persistence
  const alert = persistence?.paused ? `Printing is paused while queue state is being saved: ${persistence.reason || 'database unavailable'}` : status.data?.degraded ? 'Queue status is temporarily unavailable; check diagnostics.' : ''
  return <JobDialogProvider><ThemeSync /><div className="min-h-svh bg-background text-foreground">
    <header className="sticky top-0 z-40 border-b bg-background/90 backdrop-blur"><div className="mx-auto flex h-16 max-w-7xl items-center gap-4 px-4 sm:px-6">
      <MobileNav /><Link to="/operations/jobs" className="flex items-center gap-2 font-semibold"><span className="grid size-8 place-items-center rounded-lg bg-primary text-primary-foreground"><Printer className="size-4" /></span><span className="hidden sm:inline">Print Agent</span></Link>
      <DesktopNav /><div className="ml-auto"><StatusBadge value={state} /></div>
    </div></header>
    <main className="mx-auto max-w-7xl px-4 py-6 sm:px-6">{alert && <Alert variant="destructive" className="mb-5"><Settings /><AlertTitle>Printing paused</AlertTitle><AlertDescription>{alert}</AlertDescription></Alert>}<Outlet /></main>
  </div></JobDialogProvider>
}
