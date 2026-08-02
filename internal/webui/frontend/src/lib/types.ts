export type RunStatus = 'queued' | 'claimed' | 'transmitting' | 'transmitted' | 'failed' | 'uncertain' | 'cancelled'
export type RunTrigger = 'initial' | 'automatic_retry' | 'manual_reprint'
export type ConnectionPreference = 'auto' | 'rfcomm' | 'ble'

export interface PrintRun {
  uid: string
  jobUid: string
  printerId: string
  chainUid: string
  runNumber: number
  attemptNumber: number
  trigger: RunTrigger
  status: RunStatus
  retryDisposition: 'none' | 'pending_reconnect' | 'created' | 'exhausted' | 'suppressed'
  retryPending?: boolean
  resolution?: string
  errorCode?: string
  errorMessage?: string
  bytesAccepted: number
  createdAt: string
  claimedAt?: string
  transmissionStartedAt?: string
  finishedAt?: string
  transmittedAt?: string
  reprintRequestId?: string
}

export interface PrinterFulfillment {
  printerId: string
  fulfilled: boolean
  cancelled?: boolean
  retryPending?: boolean
  latestRun?: PrintRun
  runs?: PrintRun[]
}

export interface JobSummary {
  uid: string
  jobId: string
  template: string
  createdAt: string
  state: string
  fulfilledPrinterCount: number
  originalPrinterCount: number
  requiresAttention: boolean
  hasUncertainResult: boolean
  hasManualReprints: boolean
}

export interface JobDetail extends JobSummary {
  data: unknown
  updatedAt: string
  duplicate?: boolean
  originalPrinters: PrinterFulfillment[]
}

export interface JobsPage { jobs: JobSummary[]; nextCursor: string }

export interface PrinterConfig {
  id: string
  displayName: string
  enabled: boolean
  transport: string
  deviceAddress?: string
  endpoint: string
  connectionPreference: ConnectionPreference
  baudRate: number
  dataBits: number
  stopBits: number
  parity: string
  charactersPerLine: number
  encoding: string
  autoReconnect: boolean
  updatedAt?: string
}

export interface PrinterStatus {
  printer: PrinterConfig
  state: string
  activity: 'idle' | 'claimed' | 'transmitting'
  endpoint: string
  lastError?: string
  lastTransmission?: string
  queueDepth: number
  attentionCount: number
  reconnectAttempt?: number
  nextRetryAt?: string
}

export interface AgentStatus {
  agent: { version: string; uptimeSeconds?: number; databaseOk?: boolean }
  platform: string
  printers: PrinterStatus[]
  persistence?: { paused?: boolean; reason?: string }
  degraded?: boolean
}

export interface PrinterQueue {
  printer: PrinterStatus
  activeRun?: PrintRun
  queuedRuns: PrintRun[]
  retryPendingRuns: PrintRun[]
  attentionRuns: PrintRun[]
  recentTransmittedRuns: PrintRun[]
}

export interface BluetoothDevice {
  name?: string
  address: string
  paired: boolean
  connected: boolean
  isPrinter: boolean
  endpoint?: string
  supportedConnectionTypes?: ConnectionPreference[]
}

export interface BluetoothDevices { supported: boolean; scanning: boolean; devices: BluetoothDevice[] }
export interface Candidate { endpoint: string; deviceName?: string; deviceAddress?: string; connected: boolean; isPrinter: boolean }

export interface TokenInfo {
  id: string
  label: string
  allowedOrigin: string
  createdAt: string
  lastUsedAt?: string
  revoked: boolean
}

export interface SystemLogRecord {
  id: number
  createdAt: string
  level: 'debug' | 'info' | 'warn' | 'error'
  message: string
  printerId?: string
  runUid?: string
  attributes: Record<string, unknown>
}

export interface PrinterLogEvent {
  id: number
  createdAt: string
  type: string
  printerId?: string
  runUid?: string
  message?: string
}

export interface SystemLogPage { logs: SystemLogRecord[]; nextCursor: string; retentionSeconds: number }
export interface PrinterLogPage { events: PrinterLogEvent[]; nextCursor: string; retentionSeconds: number }

export interface PairingCode { code: string; expiresAt: string }
export interface WebSocketDestinationSettings {
  id: string; enabled: boolean; endpoint: string; authType: 'none' | 'bearer'; secretRef?: string
  customCaPath?: string; categories: string[]; connectTimeoutMs: number; heartbeatMs: number
  staleTimeoutMs: number; writeTimeoutMs: number; reconnectMinMs: number; reconnectMaxMs: number
  reconnectJitter: number; ackTimeoutMs: number; outboundQueueCapacity: number
}
export interface WebSocketSettings {
  agentId: string; mode: 'disabled' | 'server' | 'client' | 'both'; serverPath: string
  serverBindAddress: string; allowNonLoopback: boolean; serverAuthRequired: boolean; serverTls: boolean
  tlsCertPath?: string; tlsKeyRef?: string; clientQueueCapacity: number; connectionLimit: number
  heartbeatMs: number; writeTimeoutMs: number; maxMessageBytes: number; replayLimit: number
  eventRetentionSeconds: number; maxUnacknowledgedAgeSeconds: number; deadLetterRetentionSeconds: number
  eventDiskHighWaterBytes: number; allowedOrigins: string[]; destination?: WebSocketDestinationSettings; restartRequired?: boolean
}
export interface APIResult { [key: string]: unknown }
