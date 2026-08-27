import type {
  BacktestResult,
  Candle,
  GridStats,
  Order,
  Position,
  Rejection,
  Status,
} from './types'

async function get<T>(path: string): Promise<T> {
  const resp = await fetch(path)
  if (!resp.ok) throw new Error(`${path}: HTTP ${resp.status}`)
  return resp.json()
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const resp = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
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
