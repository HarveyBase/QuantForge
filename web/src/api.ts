import type {
  BacktestResult,
  Candle,
  GridStats,
  Order,
  Position,
  Rejection,
  Status,
} from './types'

// 沙箱/隐私模式下 localStorage 可能被禁用（模块级裸调会抛 SecurityError 导致整站白屏）
let token = ''
try {
  token = localStorage.getItem('quantforge_token') ?? ''
} catch {
  token = ''
}
const authHeaders = (): Record<string, string> =>
  token ? { Authorization: `Bearer ${token}` } : {}

export const streamURL = () =>
  token ? `/api/stream?token=${encodeURIComponent(token)}` : '/api/stream'

async function get<T>(path: string): Promise<T> {
  const resp = await fetch(path, { headers: authHeaders() })
  if (!resp.ok) throw new Error(`${path}: HTTP ${resp.status}`)
  return resp.json()
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const resp = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeaders() },
    body: JSON.stringify(body),
  })
  if (!resp.ok) {
    const text = await resp.text()
    throw new Error(text || `HTTP ${resp.status}`)
  }
  return resp.json()
}

export const api = {
  status: () => get<Status>('/api/status'),
  positions: () => get<{ cash: number; positions: Position[] }>('/api/positions'),
  orders: () => get<{ orders: Order[] }>('/api/orders'),
  rejections: () => get<{ rejections: Rejection[] }>('/api/rejections'),
  candles: () => get<{ candles: Candle[] }>('/api/candles'),
  grid: () => get<{ levels?: number[]; stats?: GridStats }>('/api/grid'),
  backtest: () => post<BacktestResult>('/api/backtest', {}),
  killTrip: (reason: string) => post('/api/killswitch', { action: 'trip', reason }),
  killReset: () => post('/api/killswitch', { action: 'reset', confirm: 'RESET' }),
}
