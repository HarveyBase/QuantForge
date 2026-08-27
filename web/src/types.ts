// 后端 API 类型（与 Go 结构体对齐）
export interface Status {
  version: string
  mode: 'research' | 'paper' | 'live'
  exchange: string
  market: string
  symbol: string
  interval: string
  equity: number
  cash: number
  positions: Position[]
  marks: Record<string, number>
  kill_switch: { tripped: boolean; reason: string }
  daily_notional_used: number
  risk_limits: {
    max_order_notional_usd: number
    max_daily_notional_usd: number
    max_position_notional_usd: number
    max_orders_per_minute: number
    max_daily_loss_pct: number
    cooldown_after_reject_sec: number
  }
  uptime_sec: number
}

export interface Position {
  symbol: string
  qty: number
  avg_price: number
  available: number
}

export interface Order {
  order_id: string
  client_order_id: string
  symbol: string
  side: 'buy' | 'sell'
  type: 'limit' | 'market'
  price: number
  qty: number
  filled_qty: number
  avg_price: number
  fee: number
  status: string
}

export interface Rejection {
  ts: string
  rule_id: string
  reason: string
}

export interface Candle {
  open_time: number
  open: number
  high: number
  low: number
  close: number
  volume: number
}

export interface GridStats {
  rounds: number
  realized: number
  broke: boolean
  position: number
}

export interface BacktestMetrics {
  total_return_pct: number
  buy_hold_pct: number
  max_drawdown_pct: number
  sharpe: number
  calmar: number
  trade_count: number
  win_rate: number
  total_fees: number
  final_equity: number
}

export interface BacktestResult {
  metrics: BacktestMetrics
  num_trials: number
  sample_from: number
  sample_to: number
  risk_rejections: Rejection[]
}
