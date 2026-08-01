import type {
  AgentStatus, BluetoothDevices, Candidate, JobDetail, JobsPage, PairingCode,
  PrinterConfig, PrinterLogPage, PrinterQueue, PrinterStatus, SystemLogPage, TokenInfo,
} from './types'

export class APIError extends Error {
  status: number
  code: string

  constructor(message: string, status: number, code = '') {
    super(message)
    this.status = status
    this.code = code
    this.name = 'APIError'
  }
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const response = await fetch(`/api/v1${path}`, { ...init, headers })
  const body = response.status === 204 ? null : await response.json().catch(() => null) as null | { error?: string; code?: string }
  if (!response.ok) throw new APIError(body?.error || response.statusText, response.status, body?.code)
  return body as T
}

export const api = {
  status: (signal?: AbortSignal) => request<AgentStatus>('/status', { signal }),
  printers: (signal?: AbortSignal) => request<PrinterStatus[]>('/printers', { signal }),
  jobs: (jobId: string, signal?: AbortSignal) => request<JobsPage>(`/jobs?limit=100${jobId ? `&jobId=${encodeURIComponent(jobId)}` : ''}`, { signal }),
  job: (uid: string, signal?: AbortSignal) => request<JobDetail>(`/jobs/${encodeURIComponent(uid)}`, { signal }),
  queue: (id: string, limit = 100, signal?: AbortSignal) => request<PrinterQueue>(`/printers/${encodeURIComponent(id)}/queue?limit=${limit}`, { signal }),
  candidates: (signal?: AbortSignal) => request<Candidate[]>('/bluetooth/candidates', { signal }),
  bluetoothDevices: (signal?: AbortSignal) => request<BluetoothDevices>('/bluetooth/devices', { signal }),
  tokens: (signal?: AbortSignal) => request<TokenInfo[]>('/admin/tokens', { signal }),
  pairingCode: () => request<PairingCode>('/admin/pairing-code', { method: 'POST' }),
  settings: (signal?: AbortSignal) => request<{ allowedOrigin: string }>('/admin/settings', { signal }),
  diagnostics: (signal?: AbortSignal) => request<Record<string, unknown>>('/diagnostics', { signal }),
  systemLogs: (params: URLSearchParams, signal?: AbortSignal) => request<SystemLogPage>(`/system-logs?${params}`, { signal }),
  printerLogs: (params: URLSearchParams, signal?: AbortSignal) => request<PrinterLogPage>(`/printer-logs?${params}`, { signal }),
  savePrinter: (printer: Partial<PrinterConfig>, editingId?: string) => request<PrinterConfig>(editingId ? `/printers/${encodeURIComponent(editingId)}` : '/printers', { method: editingId ? 'PUT' : 'POST', body: JSON.stringify(printer) }),
  mutate: <T = Record<string, unknown>>(path: string, method = 'POST', body?: unknown) => request<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) }),
}

export const queryKeys = {
  status: ['status'] as const,
  jobs: (filter: string) => ['jobs', filter] as const,
  job: (uid: string) => ['job', uid] as const,
  queue: (id: string, limit = 100) => ['queue', id, limit] as const,
  bluetooth: ['bluetooth'] as const,
  candidates: ['candidates'] as const,
  pairing: ['pairing'] as const,
  diagnostics: ['diagnostics'] as const,
  systemLogs: (search: string) => ['system-logs', search] as const,
  printerLogs: (search: string) => ['printer-logs', search] as const,
}
