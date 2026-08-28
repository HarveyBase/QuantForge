import { useEffect, useRef, useState } from 'react'
import { api, streamURL } from './api'
import type { BacktestResult, Candle, GridStats, Order, Rejection } from './types'

const fmt = (v: number, digits = 2) =>
  v === undefined || v === null || Number.isNaN(v) ? '-' : v.toFixed(digits)

const MODE_BADGE: Record<string, string> = {
  research: '研究',
  paper: '模拟盘',
  live: '实盘',
}

function usePolling<T>(fetcher: () => Promise<T>, intervalMs: number): T | null {
  const [data, setData] = useState<T | null>(null)
  useEffect(() => {
    let alive = true
    const tick = async () => {
      try {
        const d = await fetcher()
        if (alive) setData(d)
      } catch {
        /* 下轮重试 */
      }
    }
    tick()
    const id = setInterval(tick, intervalMs)
    return () => {
      alive = false
      clearInterval(id)
    }
  }, [intervalMs])
  return data
}

export default function App() {
  const status = usePolling(api.status, 2000)
  const orders = usePolling(api.orders, 3000)
  const rejections = usePolling(api.rejections, 5000)
  const candles = usePolling(api.candles, 15000)
  const [grid, setGrid] = useState<{ levels?: number[]; stats?: GridStats }>({})
  const [bt, setBt] = useState<BacktestResult | null>(null)
  const [btRunning, setBtRunning] = useState(false)
  const [btError, setBtError] = useState('')
  const [flash, setFlash] = useState('')

  useEffect(() => {
    api.grid().then(setGrid).catch(() => {})
  }, [])

  // SSE 事件流（自动重连由浏览器完成）
  useEffect(() => {
    const es = new EventSource(streamURL())
    es.addEventListener('update', (e) => {
      const msg = JSON.parse((e as MessageEvent).data)
      setFlash(`${new Date(msg.ts).toLocaleTimeString()} ${msg.kind}`)
    })
    es.onerror = () => es.close()
    return () => es.close()
  }, [])

  const runBacktest = async () => {
    setBtRunning(true)
    setBtError('')
    try {
      setBt(await api.backtest())
    } catch (e) {
      setBtError(String(e))
    } finally {
      setBtRunning(false)
    }
  }

  const trip = async () => {
    const reason = window.prompt('触发 Kill Switch 的原因（必填）')
    if (!reason) return
    try {
      await api.killTrip(reason)
    } catch (e) {
      window.alert(`触发失败: ${e}`)
    }
  }

  const reset = async () => {
    if (!window.confirm('确认复位 Kill Switch？（需人工确认风险）')) return
    try {
      await api.killReset()
    } catch (e) {
      window.alert(`复位失败: ${e}`)
    }
  }

  return (
    <div className="app">
      <header>
        <h1>QuantForge</h1>
        {status && (
          <div className="badges">
            <span className={`badge mode-${status.mode}`}>
              {MODE_BADGE[status.mode] ?? status.mode}
            </span>
            <span className="badge">
              {status.exchange} · {status.market} · {status.symbol} · {status.interval}
            </span>
            <span className={`badge ks-${status.kill_switch.tripped ? 'on' : 'off'}`}>
              Kill Switch {status.kill_switch.tripped ? '已触发' : '正常'}
            </span>
            {flash && <span className="flash">{flash}</span>}
          </div>
        )}
      </header>

      <section className="cards">
        <Card title="权益">
          {status ? (
            <>
              <div className="big">{fmt(status.equity)}</div>
              <div className="sub">现金 {fmt(status.cash)}</div>
              {status.positions.map((p) => (
                <div className="sub" key={p.symbol}>
                  {p.symbol}: {p.qty} @ {fmt(p.avg_price)}（可卖 {p.available}）
                </div>
              ))}
            </>
          ) : (
            <Loading />
          )}
        </Card>
        <Card title="风控限额">
          {status ? (
            <ul className="kv">
              <li>当日已用名义: {fmt(status.daily_notional_used)}</li>
              <li>单笔上限: {fmt(status.risk_limits.max_order_notional_usd)}</li>
              <li>单日上限: {fmt(status.risk_limits.max_daily_notional_usd)}</li>
              <li>敞口上限: {fmt(status.risk_limits.max_position_notional_usd)}</li>
              <li>频率: {status.risk_limits.max_orders_per_minute} 单/分钟</li>
              <li>当日回撤停机: {status.risk_limits.max_daily_loss_pct}%</li>
            </ul>
          ) : (
            <Loading />
          )}
        </Card>
        <Card title="网格策略">
          {grid.stats ? (
            <ul className="kv">
              <li>完成轮数: {grid.stats.rounds}</li>
              <li>已实现利润: {fmt(grid.stats.realized, 4)}</li>
              <li>当前持仓: {grid.stats.position}</li>
              <li className={grid.stats.broke ? 'warn' : ''}>
                {grid.stats.broke ? '⚠ 已打穿下界，停止补格' : '运行正常'}
              </li>
              {grid.levels && <li>网格线: {grid.levels.length} 档</li>}
            </ul>
          ) : (
            <div className="sub">未加载</div>
          )}
        </Card>
        <Card title="Kill Switch">
          <div className="btns">
            <button className="danger" onClick={trip}>
              触发停机（撤单+停止）
            </button>
            <button onClick={reset} disabled={!status?.kill_switch.tripped}>
              复位（需确认）
            </button>
          </div>
          {status?.kill_switch.tripped && (
            <div className="sub warn">原因: {status.kill_switch.reason}</div>
          )}
        </Card>
      </section>

      <section className="wide">
        <Card title={`K 线${candles ? `（${candles.candles.length} 根）` : ''}`}>
          {candles && candles.candles.length > 0 ? (
            <CandleChart candles={candles.candles} />
          ) : (
            <Loading />
          )}
        </Card>
      </section>

      <div className="row">
        <section>
          <Card title={`挂单（${orders?.orders.length ?? 0}）`}>
            <OrdersTable orders={orders?.orders ?? []} />
          </Card>
        </section>
        <section>
          <Card title={`风控拒单（${rejections?.rejections.length ?? 0}）`}>
            <RejectionsTable rejections={rejections?.rejections ?? []} />
          </Card>
        </section>
      </div>

      <section className="wide">
        <Card
          title="回测"
          action={
            <button onClick={runBacktest} disabled={btRunning}>
              {btRunning ? '运行中…' : '用当前配置与样本跑回测'}
            </button>
          }
        >
          {btError && <div className="sub warn">{btError}</div>}
          {bt ? (
            <>
              <ul className="kv grid2">
                <li>策略收益: {fmt(bt.metrics.total_return_pct)}%</li>
                <li>基准(买入持有): {fmt(bt.metrics.buy_hold_pct)}%</li>
                <li>最大回撤: {fmt(bt.metrics.max_drawdown_pct)}%</li>
                <li>Calmar: {fmt(bt.metrics.calmar)}</li>
                <li>Sharpe: {fmt(bt.metrics.sharpe)}</li>
                <li>胜率: {fmt(bt.metrics.win_rate, 1)}%</li>
                <li>交易次数: {bt.metrics.trade_count}</li>
                <li>手续费: {fmt(bt.metrics.total_fees, 4)}</li>
                <li>试验次数: {bt.num_trials}</li>
                <li>拒单: {bt.risk_rejections.length}</li>
              </ul>
              <div className="sub warn">
                回测输出不代表实盘收益；口径与限制见 docs/02、docs/08。
              </div>
            </>
          ) : (
            <div className="sub">尚未运行</div>
          )}
        </Card>
      </section>
    </div>
  )
}

function Card({
  title,
  action,
  children,
}: {
  title: string
  action?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <div className="card">
      <div className="card-head">
        <h2>{title}</h2>
        {action}
      </div>
      {children}
    </div>
  )
}

function Loading() {
  return <div className="sub">加载中…</div>
}

function OrdersTable({ orders }: { orders: Order[] }) {
  if (orders.length === 0) return <div className="sub">无挂单</div>
  return (
    <table>
      <thead>
        <tr>
          <th>时间</th>
          <th>方向</th>
          <th>价格</th>
          <th>数量</th>
          <th>已成交</th>
          <th>状态</th>
        </tr>
      </thead>
      <tbody>
        {orders.map((o) => (
          <tr key={o.order_id}>
            <td>{o.order_id}</td>
            <td className={o.side === 'buy' ? 'up' : 'down'}>{o.side}</td>
            <td>{fmt(o.price, 1)}</td>
            <td>{o.qty}</td>
            <td>{o.filled_qty}</td>
            <td>{o.status}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function RejectionsTable({ rejections }: { rejections: Rejection[] }) {
  if (rejections.length === 0) return <div className="sub">无拒单</div>
  return (
    <table>
      <thead>
        <tr>
          <th>时间</th>
          <th>规则</th>
          <th>原因</th>
        </tr>
      </thead>
      <tbody>
        {rejections.map((r, i) => (
          <tr key={i}>
            <td>{new Date(r.ts).toLocaleTimeString()}</td>
            <td>{r.rule_id}</td>
            <td className="reason">{r.reason}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function CandleChart({ candles }: { candles: Candle[] }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    let cleanup: (() => void) | undefined
    let cancelled = false
    import('lightweight-charts').then(({ createChart }) => {
      if (cancelled || !ref.current) return
      const chart = createChart(ref.current, {
        height: 320,
        layout: { background: { color: '#0f1420' }, textColor: '#9aa4b2' },
        grid: {
          vertLines: { color: '#1a2233' },
          horzLines: { color: '#1a2233' },
        },
        timeScale: { timeVisible: true, borderVisible: false },
      })
      const series = chart.addCandlestickSeries({
        upColor: '#22c55e',
        downColor: '#ef4444',
        wickUpColor: '#22c55e',
        wickDownColor: '#ef4444',
        borderVisible: false,
      })
      series.setData(
        candles.map((c) => ({
          time: (c.open_time / 1000) as never,
          open: c.open,
          high: c.high,
          low: c.low,
          close: c.close,
        })),
      )
      chart.timeScale().fitContent()
      const onResize = () => chart.applyOptions({ width: ref.current?.clientWidth })
      window.addEventListener('resize', onResize)
      cleanup = () => {
        window.removeEventListener('resize', onResize)
        chart.remove()
      }
    })
    return () => {
      cancelled = true
      cleanup?.()
    }
  }, [candles])
  return <div ref={ref} className="chart" />
}
