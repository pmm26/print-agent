export const formatTime = (value?: string) => value ? new Date(value).toLocaleTimeString() : '—'
export const formatDate = (value?: string) => value ? new Date(value).toLocaleString() : '—'
export const uid = (prefix: string) => prefix + (crypto.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`)

export function localDateTimeValue(value?: string) {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 19)
}

export function urlTimeValue(value: string) {
  return value ? new Date(value).toISOString() : ''
}
