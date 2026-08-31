import { useEffect, useRef, useState } from 'react'
import { api, streamURL } from './api'
import type { FillEvent, ModeInfo, ReviewRecord, WFReport } from './api'
import type { BacktestResult, Candle, GridStats, Order, Rejection } from './types'

declare global {
  interface Window {
    __liveConfirmValue?: string
    __lastCancelErr?: string
  }
}

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
  const [tf, setTf] = useState('1H')
  const candles = usePolling(() => api.candles(tf), 15000)
  const [grid, setGrid] = useState<{ levels?: number[]; stats?: GridStats }>({})
  const [bt, setBt] = useState<BacktestResult | null>(null)
  const [btRunning, setBtRunning] = useState(false)
  const [btError, setBtError] = useState('')
  const [flash, setFlash] = useState('')
  const [tripReason, setTripReason] = useState('')
  const [killMsg, setKillMsg] = useState('')
  const modeInfo = usePolling(api.mode, 5000)
  const strategyInfo = usePolling(api.strategy, 8000)
  const [stratMsg, setStratMsg] = useState('')
  const [rsStrategy, setRsStrategy] = useState('trend')
  const [rsTrain, setRsTrain] = useState('300')
  const [rsTest, setRsTest] = useState('100')
  const [wf, setWf] = useState<WFReport | null>(null)
  const [wfRunning, setWfRunning] = useState(false)
  const [wfErr, setWfErr] = useState('')
  const [umpRes, setUmpRes] = useState<{ trade_samples: number; report: { usable: boolean; reason: string } } | null>(null)
  const [umpRunning, setUmpRunning] = useState(false)
  const [plateau, setPlateau] = useState<{
    base: { label: string; ret: number }
    neighbors: { label: string; ret: number }[]
    median_ret: number
    is_plateau: boolean
    reason: string
  } | null>(null)
  const [costScan, setCostScan] = useState<
    { multiplier: number; ret: number; trades: number }[]
  >([])
  const [ptRunning, setPtRunning] = useState(false)
  const [modeMsg, setModeMsg] = useState('')
  const reviews = usePolling(() => api.reviews(5), 15000)
  const fills = usePolling(api.fills, 5000)
  const equity = usePolling(api.equityCurve, 30000)
  const [mo, setMo] = useState({ side: 'buy', type: 'limit', price: '', qty: '' })
  const [moMsg, setMoMsg] = useState('')

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
    // onerror 不主动 close：EventSource 浏览器自动重连（主动 close 会永久断连）
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
  const btDone = bt ? new Date(bt.sample_to).toLocaleString() : ''

  const submitManual = async () => {
    setMoMsg('')
    const price = parseFloat(mo.price)
    const qty = parseFloat(mo.qty)
    if (!qty || qty <= 0 || (mo.type === 'limit' && (!price || price <= 0))) {
      setMoMsg('数量必须为正；限价单价格必须为正')
      return
    }
    try {
      const o = await api.manualOrder({ side: mo.side, type: mo.type, price: price || 0, qty })
      setMoMsg(`已提交：${o.order_id ?? '(挂起)'}`)
    } catch (e) {
      setMoMsg(String(e).replace('Error: ', ''))
    }
  }

  const cancelOne = async (orderId: string) => {
    try {
      await api.cancelOrder(orderId)
    } catch (e) {
      window.__lastCancelErr = String(e)
    }
  }

  const runWF = async () => {
    setWfRunning(true)
    setWfErr('')
    setWf(null)
    try {
      setWf(await api.researchWF({
        strategy: rsStrategy,
        train: parseInt(rsTrain) || 300,
        test: parseInt(rsTest) || 100,
      }))
    } catch (e) {
      setWfErr(String(e).replace('Error: ', ''))
    } finally {
      setWfRunning(false)
    }
  }

  const runPlateauScan = async () => {
    setPtRunning(true)
    setPlateau(null)
    setCostScan([])
    try {
      const [pt, cs] = await Promise.all([
        api.researchPlateau({ strategy: rsStrategy }),
        api.researchCostScan({ strategy: rsStrategy }),
      ])
      setPlateau(pt)
      setCostScan(cs.points ?? [])
    } catch (e) {
      setPlateau({
        base: { label: '', ret: 0 }, neighbors: [], median_ret: 0,
        is_plateau: false, reason: String(e).replace('Error: ', ''),
      })
    } finally {
      setPtRunning(false)
    }
  }

  const runUMP = async () => {
    setUmpRunning(true)
    setUmpRes(null)
    try {
      setUmpRes(await api.researchUMP({ strategy: rsStrategy }))
    } catch (e) {
      setUmpRes({ trade_samples: 0, report: { usable: false, reason: String(e).replace('Error: ', '') } })
    } finally {
      setUmpRunning(false)
    }
  }

  const switchStrategy = async (name: string) => {
    setStratMsg('')
    try {
      await api.switchStrategy(name)
      setStratMsg(`策略已切到 ${name}（无持仓时生效）`)
    } catch (e) {
      setStratMsg(String(e).replace('Error: ', ''))
    }
  }

  const switchTo = async (m: string) => {
    setModeMsg('')
    let confirm: string | undefined
    if (m === 'live') {
      confirm = window.__liveConfirmValue
      if (!confirm) {
        setModeMsg('切 live 需先在确认框输入 I_UNDERSTAND_THE_RISK')
        return
      }
    }
    try {
      await api.switchMode(m, confirm)
      setModeMsg(`已切换到 ${MODE_BADGE[m] ?? m}`)
    } catch (e) {
      setModeMsg(String(e).replace('Error: ', ''))
    }
  }

  const trip = async () => {
    const reason = tripReason.trim()
    if (!reason) {
      setKillMsg('必须填写触发原因（审计留痕）')
      return
    }
    try {
      await api.killTrip(reason)
      setTripReason('')
      setKillMsg('已触发：撤单 + 停止一切新下单')
    } catch (e) {
      setKillMsg(`触发失败: ${e}`)
    }
  }

  const reset = async () => {
    try {
      await api.killReset()
      setKillMsg('已复位')
    } catch (e) {
      setKillMsg(`复位失败: ${e}`)
    }
  }

  return (
    <div className="app">
      <header>
        <h1>QuantForge</h1>
        <ModeSwitcher info={modeInfo} onSwitch={switchTo} msg={modeMsg} />
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
            {strategyInfo && (
              <span className="badge">策略: {strategyInfo.desc || strategyInfo.name}</span>
            )}
            {strategyInfo?.available?.map((m) => (
              <button key={m} className="mode-btn" onClick={() => switchStrategy(m)}>
                {m === 'grid' ? '切网格' : m === 'trend' ? '切趋势' : '切组合'}
              </button>
            ))}
            {stratMsg && <span className="sub">{stratMsg}</span>}
            {status.regime && (
              <span className={`badge regime-${status.regime.kind}`}>
                市况: {status.regime.kind === 'trending' ? '趋势' : status.regime.kind === 'range' ? '震荡' : '过渡'}
                {status.regime.er > 0 ? ` (ER ${status.regime.er.toFixed(2)})` : ''}
              </span>
            )}
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
          <input
            className="ks-input"
            placeholder="触发原因（必填，审计留痕）"
            value={tripReason}
            onChange={(e) => setTripReason(e.target.value)}
          />
          <div className="btns">
            <button className="danger" onClick={trip}>
              触发停机（撤单+停止）
            </button>
            <button onClick={reset} disabled={!status?.kill_switch.tripped}>
              复位
            </button>
          </div>
          {killMsg && <div className="sub">{killMsg}</div>}
          {status?.kill_switch.tripped && (
            <div className="sub warn">触发中: {status.kill_switch.reason}</div>
          )}
        </Card>
      </section>

      <section className="wide">
        <Card
          title={`K 线${candles?.candles?.length ? `（${candles.candles.length} 根）` : ''}`}
          action={
            <div className="tf-switch">
              {['1H', '4H', '1D'].map((m) => (
                <button
                  key={m}
                  className={tf === m ? 'tf-btn active' : 'tf-btn'}
                  onClick={() => setTf(m)}
                >
                  {m}
                </button>
              ))}
            </div>
          }
        >
          {candles?.candles && candles.candles.length > 0 ? (
            <CandleChart candles={candles.candles} />
          ) : (
            <Loading />
          )}
        </Card>
      </section>

      <div className="row">
        <section>
          <Card title={`挂单（${orders?.orders?.length ?? 0}）`}>
            <OrdersTable orders={orders?.orders ?? []} />
          </Card>
        </section>
        <section>
          <Card title={`风控拒单（${rejections?.rejections?.length ?? 0}）`}>
            <RejectionsTable rejections={rejections?.rejections ?? []} />
          </Card>
        </section>
      </div>

      <section className="wide">
        <Card title={`最近复盘（${reviews?.reviews?.length ?? 0} 份，每小时一份）`}>
          {reviews?.reviews && reviews.reviews.length > 0 ? (
            <table>
              <thead>
                <tr>
                  <th>时间</th>
                  <th>环境</th>
                  <th>窗口收益</th>
                  <th>买入持有</th>
                  <th>成交</th>
                  <th>拒单</th>
                  <th>UMP拦截</th>
                  <th>诊断</th>
                </tr>
              </thead>
              <tbody>
                {reviews.reviews.map((rv: ReviewRecord) => (
                  <tr key={rv.ts}>
                    <td>{new Date(rv.ts).toLocaleTimeString()}</td>
                    <td>{MODE_BADGE[rv.stage] ?? rv.stage}</td>
                    <td className={rv.window_ret_pct >= 0 ? 'up' : 'down'}>
                      {rv.window_ret_pct.toFixed(2)}%
                    </td>
                    <td>{rv.price_chg_pct.toFixed(2)}%</td>
                    <td>{rv.fills?.length ?? 0}</td>
                    <td>{rv.rejections?.length ?? 0}</td>
                    <td>{rv.ump_blocked}</td>
                    <td className="reason">{rv.notes?.[0] ?? ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <div className="sub">暂无复盘记录（每小时自动生成，服务运行满一个复盘周期后出现）</div>
          )}
        </Card>
      </section>

      <section className="wide">
        <Card title={`权益曲线（${equity?.points?.length ?? 0} 个复盘点）`}>
          {equity?.points && equity.points.length >= 2 ? (
            <EquityChart points={equity.points} />
          ) : (
            <div className="sub">复盘点不足（服务运行满一个复盘周期后出现）</div>
          )}
        </Card>
      </section>

      <div className="row">
        <section>
          <Card title={`成交历史（${fills?.fills?.length ?? 0}）`}>
            {fills?.fills && fills.fills.length > 0 ? (
              <table>
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>方向</th>
                    <th>数量</th>
                    <th>价格</th>
                    <th>订单</th>
                  </tr>
                </thead>
                <tbody>
                  {fills.fills.slice(0, 20).map((f: FillEvent) => (
                    <tr key={f.ts + f.order.order_id}>
                      <td>{new Date(f.ts).toLocaleTimeString()}</td>
                      <td className={f.order.side === 'buy' ? 'up' : 'down'}>{f.order.side}</td>
                      <td>{f.delta_qty}</td>
                      <td>{f.delta_price}</td>
                      <td>{f.order.order_id}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div className="sub">暂无成交</div>
            )}
          </Card>
        </section>
        <section>
          <Card title="手动交易台（走完整风控）">
            <div className="manual-grid">
              <select value={mo.side} onChange={(e) => setMo({ ...mo, side: e.target.value })}>
                <option value="buy">买入</option>
                <option value="sell">卖出</option>
              </select>
              <select value={mo.type} onChange={(e) => setMo({ ...mo, type: e.target.value })}>
                <option value="limit">限价</option>
                <option value="market">市价</option>
              </select>
              <input
                placeholder={mo.type === 'limit' ? '价格' : '价格（市价忽略）'}
                value={mo.price}
                disabled={mo.type === 'market'}
                onChange={(e) => setMo({ ...mo, price: e.target.value })}
              />
              <input
                placeholder="数量（BTC）"
                value={mo.qty}
                onChange={(e) => setMo({ ...mo, qty: e.target.value })}
              />
              <button className={mo.side === 'buy' ? 'up-btn' : 'danger'} onClick={submitManual}>
                提交订单
              </button>
            </div>
            {moMsg && <div className="sub">{moMsg}</div>}
            <div className="sub">挂单列表每行可单独撤单：</div>
            {orders?.orders?.map((o) => (
              <div className="order-row" key={o.order_id}>
                <span className={o.side === 'buy' ? 'up' : 'down'}>
                  {o.side} {o.qty} @ {o.price}（{o.status}）
                </span>
                <button onClick={() => cancelOne(o.order_id)}>撤单</button>
              </div>
            ))}
            {window.__lastCancelErr && (
              <div className="sub warn">撤单失败: {window.__lastCancelErr}</div>
            )}
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
                <li>样本至: {btDone}</li>
                <li>策略收益: {fmt(bt.metrics.total_return_pct)}%</li>
                <li>基准(买入持有): {fmt(bt.metrics.buy_hold_pct)}%</li>
                <li>最大回撤: {fmt(bt.metrics.max_drawdown_pct)}%</li>
                <li>Calmar: {fmt(bt.metrics.calmar)}</li>
                <li>Sharpe: {fmt(bt.metrics.sharpe)}</li>
                <li>胜率: {fmt(bt.metrics.win_rate, 1)}%</li>
                <li>交易次数: {bt.metrics.trade_count}</li>
                <li>手续费: {fmt(bt.metrics.total_fees, 4)}</li>
                <li>试验次数: {bt.num_trials}</li>
                <li>拒单: {bt.risk_rejections?.length ?? 0}</li>
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

      <section className="wide">
        <Card
          title="研究工作台（lab 验证流水线）"
          action={
            <div className="manual-grid" style={{ gridTemplateColumns: 'auto 100px 100px auto auto' }}>
              <select value={rsStrategy} onChange={(e) => setRsStrategy(e.target.value)}>
                <option value="trend">trend</option>
                <option value="grid">grid</option>
              </select>
              <input placeholder="train" value={rsTrain} onChange={(e) => setRsTrain(e.target.value)} />
              <input placeholder="test" value={rsTest} onChange={(e) => setRsTest(e.target.value)} />
              <button onClick={runWF} disabled={wfRunning}>{wfRunning ? 'WF 运行中…' : '跑 walk-forward'}</button>
              <button onClick={runUMP} disabled={umpRunning}>{umpRunning ? 'UMP 验证中…' : 'UMP 拦截器验证'}</button>
              <button onClick={runPlateauScan} disabled={ptRunning}>{ptRunning ? '检验中…' : '参数高原+成本扫描'}</button>
            </div>
          }
        >
          {wfErr && <div className="sub warn">{wfErr}</div>}
          {wf && (
            <>
              <ul className="kv grid2">
                <li>样本: {wf.candles} 根</li>
                <li>试验: {wf.total_trials} 次</li>
                <li>OOS 收益: {fmt(wf.oos_metrics.total_return_pct)}%</li>
                <li>OOS MDD: {fmt(wf.oos_metrics.max_drawdown_pct)}%</li>
                <li>Calmar: {fmt(wf.oos_metrics.calmar)}</li>
                <li>买入持有: {fmt(wf.buy_hold_pct)}%</li>
              </ul>
              <table>
                <thead>
                  <tr><th>折</th><th>策略</th><th>收益</th><th>MDD</th><th>交易</th></tr>
                </thead>
                <tbody>
                  {wf.folds.slice(-10).map((f) => (
                    <tr key={f.fold}>
                      <td>{f.fold}</td>
                      <td>{f.strategy}</td>
                      <td className={f.total_return_pct >= 0 ? 'up' : 'down'}>{f.total_return_pct.toFixed(2)}%</td>
                      <td>{f.max_drawdown_pct.toFixed(2)}%</td>
                      <td>{f.trade_count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <div className="sub warn">OOS 不代表实盘收益；结论口径见 docs/01、docs/02</div>
            </>
          )}
          {umpRes && (
            <div className={umpRes.report.usable ? 'sub up' : 'sub warn'}>
              UMP（{umpRes.trade_samples} 笔样本）：{umpRes.report.reason}
            </div>
          )}
          {plateau && (
            <>
              <div className={plateau.is_plateau ? 'sub up' : 'sub warn'}>
                参数高原：{plateau.reason}
              </div>
              <table>
                <thead>
                  <tr><th>参数点</th><th>收益</th><th>角色</th></tr>
                </thead>
                <tbody>
                  <tr>
                    <td>{plateau.base.label}</td>
                    <td className={plateau.base.ret >= 0 ? 'up' : 'down'}>{plateau.base.ret.toFixed(2)}%</td>
                    <td>基准</td>
                  </tr>
                  {plateau.neighbors.map((n) => (
                    <tr key={n.label}>
                      <td>{n.label}</td>
                      <td className={n.ret >= 0 ? 'up' : 'down'}>{n.ret.toFixed(2)}%</td>
                      <td>邻域</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
          {costScan.length > 0 && (
            <>
              <div className="sub">成本敏感性（2x 转负即不可交易化）：</div>
              <table>
                <thead>
                  <tr><th>成本倍数</th><th>收益</th><th>交易</th></tr>
                </thead>
                <tbody>
                  {costScan.map((p) => (
                    <tr key={p.multiplier}>
                      <td>{p.multiplier}x</td>
                      <td className={p.ret >= 0 ? 'up' : 'down'}>{p.ret.toFixed(2)}%</td>
                      <td>{p.trades}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
          {!wf && !umpRes && !wfErr && <div className="sub">walk-forward 样本外验证与 UMP 拦截器验证（需先 fetch 长历史）</div>}
        </Card>
      </section>
    </div>
  )
}

function ModeSwitcher({
  info,
  onSwitch,
  msg,
}: {
  info: ModeInfo | null
  onSwitch: (m: string) => void
  msg: string
}) {
  const [liveConfirm, setLiveConfirm] = useState('')
  if (!info) return <div className="mode-switch sub">环境加载中…</div>
  const labels: Record<string, string> = { research: '回测', paper: '模拟盘', live: '实盘' }
  return (
    <div className="mode-switch">
      {(info.switchable ?? []).map((m) => (
        <button
          key={m}
          className={info.active === m ? 'mode-btn active' : 'mode-btn'}
          onClick={() => onSwitch(m)}
        >
          {labels[m] ?? m}
        </button>
      ))}
      <span className={`badge mode-${info.active}`}>
        当前: {labels[info.active] ?? info.active} / 启动: {labels[info.boot] ?? info.boot}
      </span>
      {info.boot === 'live' && (
        <input
          className="ks-input live-confirm"
          placeholder="live 确认词 I_UNDERSTAND_THE_RISK"
          value={liveConfirm}
          onChange={(e) => {
            setLiveConfirm(e.target.value)
            window.__liveConfirmValue = e.target.value
          }}
        />
      )}
      {msg && <span className="sub">{msg}</span>}
      {info.boot !== 'live' && (
        <span className="sub">升 live 需 live 配置重启 + 环境变量门禁（安全红线）</span>
      )}
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

function EquityChart({ points }: { points: { ts: number; equity: number }[] }) {
  const w = 900
  const h = 180
  const pad = 30
  const eqs = points.map((p) => p.equity)
  const min = Math.min(...eqs)
  const max = Math.max(...eqs)
  const span = max - min || 1
  const xs = (i: number) => pad + (i / Math.max(points.length - 1, 1)) * (w - pad * 2)
  const ys = (v: number) => pad + (1 - (v - min) / span) * (h - pad * 2)
  const path = points.map((p, i) => `${i === 0 ? 'M' : 'L'}${xs(i)},${ys(p.equity)}`).join(' ')
  const up = eqs[eqs.length - 1] >= eqs[0]
  return (
    <svg width="100%" viewBox={`0 0 ${w} ${h}`} style={{ display: 'block' }}>
      <path
        d={path}
        fill="none"
        stroke={up ? '#22c55e' : '#ef4444'}
        strokeWidth="1.5"
      />
      <text x={pad} y={pad - 8} fill="#9aa4b2" fontSize="10">{fmt(max)}</text>
      <text x={pad} y={h - 8} fill="#9aa4b2" fontSize="10">{fmt(min)}</text>
      <text x={w - pad - 60} y={pad - 8} fill={up ? '#22c55e' : '#ef4444'} fontSize="11">
        {up ? '+' : ''}{(((eqs[eqs.length - 1] / eqs[0]) - 1) * 100).toFixed(2)}%
      </text>
    </svg>
  )
}

function CandleChart({ candles }: { candles: Candle[] }) {
  const ref = useRef<HTMLDivElement>(null)
  const tipRef = useRef<HTMLDivElement>(null)
  const legendRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    let cleanup: (() => void) | undefined
    let cancelled = false
    import('lightweight-charts').then(({ createChart, CrosshairMode }) => {
      if (cancelled || !ref.current) return
      const chart = createChart(ref.current, {
        height: 360,
        layout: { background: { color: '#0f1420' }, textColor: '#9aa4b2' },
        grid: { vertLines: { color: '#1a2233' }, horzLines: { color: '#1a2233' } },
        crosshair: {
          mode: CrosshairMode.Normal,
          vertLine: { color: '#3b82f6', labelBackgroundColor: '#1d4ed8' },
          horzLine: { color: '#3b82f6', labelBackgroundColor: '#1d4ed8' },
        },
        rightPriceScale: { borderColor: '#1a2233' },
        timeScale: { timeVisible: true, secondsVisible: false, borderVisible: false },
      })
      const series = chart.addCandlestickSeries({
        upColor: '#22c55e', downColor: '#ef4444',
        wickUpColor: '#22c55e', wickDownColor: '#ef4444', borderVisible: false,
      })
      series.setData(
        candles.map((c) => ({
          time: (c.open_time / 1000) as never,
          open: c.open, high: c.high, low: c.low, close: c.close,
        })),
      )
      const vol = chart.addHistogramSeries({
        priceFormat: { type: 'volume' },
        priceScaleId: 'vol',
      })
      chart.priceScale('vol').applyOptions({ scaleMargins: { top: 0.82, bottom: 0 } })
      vol.setData(
        candles.map((c) => ({
          time: (c.open_time / 1000) as never,
          value: c.volume,
          color: c.close >= c.open ? 'rgba(34,197,94,0.45)' : 'rgba(239,68,68,0.45)',
        })),
      )
      const lastBar = candles[candles.length - 1]
      if (lastBar) {
        series.createPriceLine({
          price: lastBar.close,
          color: lastBar.close >= lastBar.open ? '#22c55e' : '#ef4444',
          lineWidth: 1, lineStyle: 2, axisLabelVisible: true, title: '最新',
        })
      }
      chart.timeScale().fitContent()
      const barHTML = (c: Candle) => {
        const chg = ((c.close - c.open) / c.open) * 100
        const cls = chg >= 0 ? 'tt-up' : 'tt-down'
        return (
          '<b>' + new Date(c.open_time).toLocaleString() + '</b>　' +
          '开 ' + fmt(c.open) + '　高 ' + fmt(c.high) + '　低 ' + fmt(c.low) +
          '　收 <span class="' + cls + '">' + fmt(c.close) + '</span>' +
          '　<span class="' + cls + '">' + (chg >= 0 ? '+' : '') + chg.toFixed(2) + '%</span>' +
          '　量 ' + (c.volume >= 1000 ? (c.volume / 1000).toFixed(1) + 'K' : c.volume.toFixed(2))
        )
      }
      const byTime = new Map(candles.map((c) => [Math.floor(c.open_time / 1000), c]))
      if (legendRef.current && lastBar) legendRef.current.innerHTML = barHTML(lastBar)
      chart.subscribeCrosshairMove((param) => {
        const t = param.time as number | undefined
        const c = t ? byTime.get(t) : undefined
        if (!c || !param.point) {
          if (tipRef.current) tipRef.current.style.display = 'none'
          if (legendRef.current && lastBar) legendRef.current.innerHTML = barHTML(lastBar)
          return
        }
        const html = barHTML(c)
        if (legendRef.current) legendRef.current.innerHTML = html
        if (tipRef.current) {
          tipRef.current.innerHTML = html
          tipRef.current.style.display = 'block'
          const w = ref.current?.clientWidth ?? 600
          tipRef.current.style.left = Math.min(param.point.x + 18, w - 330) + 'px'
          tipRef.current.style.top = Math.max(param.point.y - 10, 4) + 'px'
        }
      })
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
  return (
    <div style={{ position: 'relative' }}>
      <div ref={legendRef} className="chart-legend" />
      <div ref={ref} className="chart" />
      <div ref={tipRef} className="chart-tooltip" style={{ display: 'none' }} />
    </div>
  )
}
