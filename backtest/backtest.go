// Package backtest 事件驱动回测引擎（docs/02）。
// 主循环推进顺序（每根 K 线）：结算挂单 → 写入历史 → 策略产出意图 → 风控门禁 → 撮合 → 记权益。
// 策略只能读到当前及历史 K 线；三类审计记录（trades/pending/risk_rejections）必须并存。
package backtest

import (
	"fmt"
	"math"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
	"github.com/HarveyBase/QuantForge/strategy"
)

// CostModel 成本假设（必须显式声明，零成本要标注简化口径）。
type CostModel struct {
	SlippageBps float64 // 滑点（基点）
	MakerFeeBps float64 // 挂单手续费
	TakerFeeBps float64 // 吃单手续费
}

// Result 回测结果。
type Result struct {
	Trades         []exchange.Order `json:"trades"`          // 成交记录
	PendingOrders  []exchange.Order `json:"pending_orders"`  // 期末未成交挂单
	RiskRejections []risk.Rejection `json:"risk_rejections"` // 风控拒单
	EquityCurve    []EquityPoint    `json:"equity_curve"`    // 权益曲线
	Metrics        Metrics          `json:"metrics"`
	SampleFrom     int64            `json:"sample_from"` // 样本区间（ms）
	SampleTo       int64            `json:"sample_to"`
	NumTrials      int              `json:"num_trials"` // 试验次数（防数据窥探：多次回测须累计）
}

// EquityPoint 权益检查点。
type EquityPoint struct {
	Ts     int64   `json:"ts"`
	Equity float64 `json:"equity"`
	Price  float64 `json:"price"`
}

// Metrics 绩效指标（口径见 docs/02）。
type Metrics struct {
	TotalReturnPct float64 `json:"total_return_pct"`
	BuyHoldPct     float64 `json:"buy_hold_pct"`     // 对照基准：买入持有
	MaxDrawdownPct float64 `json:"max_drawdown_pct"` // 基于权益曲线峰谷
	Sharpe         float64 `json:"sharpe"`           // 年化（按 periods_per_year）
	Calmar         float64 `json:"calmar"`           // 总收益% / |MDD%|（口径：非年化）
	TradeCount     int     `json:"trade_count"`
	WinRate        float64 `json:"win_rate"`
	AvgWinPct      float64 `json:"avg_win_pct"`  // 平均盈利幅度%（卖出/配对成本−1，仅盈利笔）
	AvgLossPct     float64 `json:"avg_loss_pct"` // 平均亏损幅度%（正数，仅亏损笔）
	TotalFees      float64 `json:"total_fees"`
	FinalEquity    float64 `json:"final_equity"`
}

// pendingOrder 回测内部挂单表示。
type pendingOrder struct {
	order    exchange.Order
	isMarket bool // 市价单：下一根开盘价 ± 滑点成交
}

// Engine 回测引擎。
type Engine struct {
	Strategy strategy.Strategy
	Cost     CostModel
	SeedCash float64
}

// Run 跑一次回测。candles 必须已通过 market.Validate（升序、已收盘、无缺口）。
func (e *Engine) Run(candles []exchange.Candle, symbol, interval string, numTrials int) (*Result, error) {
	if len(candles) == 0 {
		return nil, fmt.Errorf("backtest: 空 K 线")
	}
	if e.SeedCash <= 0 || math.IsNaN(e.SeedCash) || math.IsInf(e.SeedCash, 0) {
		return nil, fmt.Errorf("backtest: 初始资金必须为有限正数")
	}
	pf := portfolio.New(e.SeedCash)
	rk := risk.NewManager(risk.Limits{
		MaxOrderNotionalUSD:    math.Inf(1), // 回测不设限额？不——沿用默认上限的语义由调用方决定；
		MaxDailyNotionalUSD:    math.Inf(1), // 引擎内置只做可用性校验（现金/持仓），限额风控由实盘层负责。
		MaxPositionNotionalUSD: math.Inf(1),
		MaxOrdersPerMinute:     1 << 30,
		MaxDailyLossPct:        100,
	}, pf, "")

	var (
		trades  []exchange.Order
		pending []pendingOrder
		curve   []EquityPoint
		rejects []risk.Rejection
	)

	warmup := e.Strategy.Warmup()
	for i := 0; i < len(candles); i++ {
		c := candles[i]

		// 1. 结算上一轮挂单：策略在第 i-1 根收盘后发出的订单，从第 i 根起才可能成交
		//（防"同根先看后成"前视：信号依赖收盘价则下一根成交）
		var stillPending []pendingOrder
		for _, p := range pending {
			o := p.order
			if p.isMarket {
				o = fillMarket(o, c, e.Cost)
				pf.ApplyFill(exchange.Fill{Symbol: o.Symbol, ClientOrderID: o.ClientOrderID, Side: o.Side, Qty: o.FilledQty, Price: o.AvgPrice, Fee: o.Fee, Ts: o.UpdatedAt})
				pf.ReleaseOrder(o.ClientOrderID)
				applyStrategyFill(e.Strategy, o)
				trades = append(trades, o)
				continue
			}
			if limitTouched(c, o) {
				o = fillLimit(o, c, e.Cost)
				pf.ApplyFill(exchange.Fill{Symbol: o.Symbol, ClientOrderID: o.ClientOrderID, Side: o.Side, Qty: o.FilledQty, Price: o.AvgPrice, Fee: o.Fee, Ts: o.UpdatedAt})
				pf.ReleaseOrder(o.ClientOrderID)
				applyStrategyFill(e.Strategy, o)
				trades = append(trades, o)
				continue
			}
			stillPending = append(stillPending, pendingOrder{o, false})
		}
		pending = stillPending

		// 2. 当前 K 线写入历史上下文（策略只看到 [0, i]）
		if i+1 < warmup {
			pf.UpdateMark(symbol, c.Close)
			curve = append(curve, EquityPoint{Ts: c.OpenTime, Equity: pf.Equity(), Price: c.Close})
			continue
		}
		_, positions, _ := pf.Snapshot()
		posQty := 0.0
		for _, p := range positions {
			if p.Symbol == symbol {
				posQty = p.Qty
			}
		}
		cash, _, _ := pf.Snapshot()
		sctx := &strategy.Context{
			Symbol: symbol, Interval: interval, Candles: candles[:i+1],
			Equity: pf.Equity(), Position: posQty, Cash: cash,
		}

		// 3. 策略产出意图 → 4. 风控门禁 → 5. 撮合
		for intentIndex, intent := range e.Strategy.OnCandle(sctx) {
			req := exchange.OrderRequest{
				Symbol: symbol, Side: intent.Side, Type: intent.Type,
				Price: intent.Price, Qty: intent.Qty,
				ClientOrderID: fmt.Sprintf("bt-%d-%s-%d", c.OpenTime, intent.Kind, intentIndex),
			}
			if err := rk.CheckOrder(req, c.Close); err != nil {
				rejects = append(rejects, risk.Rejection{Ts: time.UnixMilli(c.OpenTime), RuleID: "BT_RISK", Reason: err.Error(), Order: req})
				continue
			}
			freezeReq := req
			if freezeReq.Price == 0 {
				freezeReq.Price = c.Close
			}
			if !pf.Freeze(freezeReq) {
				rejects = append(rejects, risk.Rejection{Ts: time.UnixMilli(c.OpenTime), RuleID: "BT_FREEZE", Reason: "可用资金或持仓不足", Order: req})
				continue
			}
			o := exchange.Order{
				Exchange: "backtest", Symbol: symbol, ClientOrderID: req.ClientOrderID, Side: intent.Side, Type: intent.Type,
				Price: freezeReq.Price, Qty: intent.Qty, Status: exchange.StatusSubmitted,
				CreatedAt: c.OpenTime, UpdatedAt: c.OpenTime,
			}
			// 当根不成交：一律入队，下一根起结算
			pending = append(pending, pendingOrder{o, intent.Type == exchange.OrderMarket})
		}

		// 6. 按当前收盘价记录权益
		pf.UpdateMark(symbol, c.Close)
		curve = append(curve, EquityPoint{Ts: c.OpenTime, Equity: pf.Equity(), Price: c.Close})
	}

	first, last := candles[0], candles[len(candles)-1]
	res := &Result{
		Trades: trades, PendingOrders: unwrap(pending), RiskRejections: rejects,
		EquityCurve: curve, SampleFrom: first.OpenTime, SampleTo: last.OpenTime,
		NumTrials: numTrials,
	}
	res.Metrics = ComputeMetrics(curve, trades, e.SeedCash, first.Close, last.Close)
	return res, nil
}

func applyStrategyFill(s strategy.Strategy, o exchange.Order) {
	if applier, ok := s.(interface {
		ApplyFill(exchange.Side, float64, float64)
	}); ok {
		applier.ApplyFill(o.Side, o.FilledQty, o.AvgPrice)
	}
}

// limitTouched 限价单在本根是否触及。
func limitTouched(c exchange.Candle, o exchange.Order) bool {
	if o.Side == exchange.Buy {
		return c.Low <= o.Price // 最低价触及买价
	}
	return c.High >= o.Price // 最高价触及卖价
}

func unwrap(ps []pendingOrder) []exchange.Order {
	out := make([]exchange.Order, len(ps))
	for i, p := range ps {
		out[i] = p.order
	}
	return out
}

// fillMarket 市价单按本根开盘价 ± 滑点成交（taker 费）。
func fillMarket(o exchange.Order, c exchange.Candle, cost CostModel) exchange.Order {
	fillPx := c.Open
	if o.Side == exchange.Buy {
		fillPx *= 1 + cost.SlippageBps/10000
		o.Fee = -(o.Qty * fillPx * cost.TakerFeeBps / 10000)
	} else {
		fillPx *= 1 - cost.SlippageBps/10000
		o.Fee = -(o.Qty * fillPx * cost.TakerFeeBps / 10000)
	}
	o.FilledQty = o.Qty
	o.AvgPrice = fillPx
	o.Status = exchange.StatusFilled
	o.UpdatedAt = c.OpenTime
	return o
}

// fillLimit 限价单成交：委托价与本根开盘价的较优者 + 滑点（maker 费）。
func fillLimit(o exchange.Order, c exchange.Candle, cost CostModel) exchange.Order {
	fillPx := o.Price
	if o.Side == exchange.Buy {
		if c.Open < o.Price {
			fillPx = c.Open
		}
		fillPx *= 1 + cost.SlippageBps/10000
		if fillPx > o.Price {
			fillPx = o.Price
		}
		o.Fee = -(o.Qty * fillPx * cost.MakerFeeBps / 10000)
	} else {
		if c.Open > o.Price {
			fillPx = c.Open
		}
		fillPx *= 1 - cost.SlippageBps/10000
		if fillPx < o.Price {
			fillPx = o.Price
		}
		o.Fee = -(o.Qty * fillPx * cost.MakerFeeBps / 10000)
	}
	o.FilledQty = o.Qty
	o.AvgPrice = fillPx
	o.Status = exchange.StatusFilled
	o.UpdatedAt = c.OpenTime
	return o
}

// ComputeMetrics 指标口径（docs/02）：
// MDD 基于权益曲线峰谷复算；Sharpe 用逐期收益 × √periods_per_year；胜率按移动成本法配对核算。
func ComputeMetrics(curve []EquityPoint, trades []exchange.Order, seed, firstPx, lastPx float64) Metrics {
	m := Metrics{FinalEquity: seed}
	if len(curve) == 0 {
		return m
	}
	m.FinalEquity = curve[len(curve)-1].Equity
	m.TotalReturnPct = (m.FinalEquity/seed - 1) * 100
	m.BuyHoldPct = (lastPx/firstPx - 1) * 100

	// MDD：权益曲线峰谷
	peak := curve[0].Equity
	mdd := 0.0
	for _, p := range curve {
		if p.Equity > peak {
			peak = p.Equity
		}
		if peak > 0 {
			d := (p.Equity/peak - 1) * 100
			if d < mdd {
				mdd = d
			}
		}
	}
	m.MaxDrawdownPct = mdd
	if mdd < 0 {
		m.Calmar = m.TotalReturnPct / -mdd
	}

	// Sharpe：逐期权益收益
	if len(curve) > 2 {
		var rets []float64
		for i := 1; i < len(curve); i++ {
			if curve[i-1].Equity > 0 {
				rets = append(rets, curve[i].Equity/curve[i-1].Equity-1)
			}
		}
		if len(rets) > 1 {
			mean := 0.0
			for _, r := range rets {
				mean += r
			}
			mean /= float64(len(rets))
			var ss float64
			for _, r := range rets {
				ss += (r - mean) * (r - mean)
			}
			sd := math.Sqrt(ss / float64(len(rets)-1))
			if sd > 0 {
				// 年化因子：按相邻检查点间隔推算
				dt := curve[1].Ts - curve[0].Ts
				if dt <= 0 {
					dt = 3_600_000
				}
				ppy := 365.0 * 24 * 3_600_000 / float64(dt)
				m.Sharpe = mean / sd * math.Sqrt(ppy)
			}
		}
	}

	m.TradeCount = len(trades)
	fees, wins, sells := 0.0, 0, 0
	avgCost := 0.0 // 单标的移动成本
	qty := 0.0
	var winPcts, lossPcts []float64
	for _, tr := range trades {
		fees += -tr.Fee
		switch tr.Side {
		case exchange.Buy:
			avgCost = (avgCost*qty + tr.AvgPrice*tr.FilledQty) / (qty + tr.FilledQty)
			qty += tr.FilledQty
		case exchange.Sell:
			if qty > 0 && avgCost > 0 {
				retPct := (tr.AvgPrice/avgCost - 1) * 100
				if tr.AvgPrice > avgCost {
					wins++
					winPcts = append(winPcts, retPct)
				} else {
					lossPcts = append(lossPcts, -retPct)
				}
			}
			qty -= tr.FilledQty
			sells++
		}
	}
	m.TotalFees = fees
	if sells > 0 {
		m.WinRate = float64(wins) / float64(sells) * 100
	}
	if len(winPcts) > 0 {
		sum := 0.0
		for _, v := range winPcts {
			sum += v
		}
		m.AvgWinPct = sum / float64(len(winPcts))
	}
	if len(lossPcts) > 0 {
		sum := 0.0
		for _, v := range lossPcts {
			sum += v
		}
		m.AvgLossPct = sum / float64(len(lossPcts))
	}
	return m
}
