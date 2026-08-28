// Package trend 趋势跟踪策略（docs/07 海龟法则口径）：
// 唐奇安通道突破入场 + ATR 跟踪止损 + 通道下轨出场，波动率目标定仓。
// 特性：只赚单边趋势的钱，震荡市频繁假突破亏小钱（低胜率 ~30-40% / 高盈亏比），
// 与网格策略市况互补——策略与市况错配是最大风险源，组合后才敢谈收益。
// 防前视：信号只用已收盘 K 线（Context 契约），指标当前根不参与自身轨道（indicators.Donchian），
// 订单一律下一根成交（backtest 引擎挂单结算顺序保证）。
package trend

import (
	"fmt"
	"math"
	"sync"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/indicators"
	"github.com/HarveyBase/QuantForge/sizer"
	"github.com/HarveyBase/QuantForge/strategy"
)

var _ strategy.Strategy = (*Donchian)(nil)

// Params 趋势参数（越少越好：四个核心参数对应海龟经典配置）。
type Params struct {
	EntryN    int     // 入场：收盘突破前 EntryN 根最高价（不含当前根）
	ExitN     int     // 出场：收盘跌破前 ExitN 根最低价（不含当前根）
	AtrN      int     // ATR 周期（Wilder 平滑）
	AtrMult   float64 // 跟踪止损 = 入场后最高收盘 − AtrMult × ATR
	RiskPct   float64 // 单笔风险占权益比例（0.005 = 0.5%）：qty = equity×RiskPct / ATR
	MaxPosPct float64 // 单笔名义占权益上限（永不满仓）
}

// DefaultParams 海龟经典参数（20 日突破入场 / 10 日反向出场 / 2ATR 止损）。
func DefaultParams() Params {
	return Params{EntryN: 20, ExitN: 10, AtrN: 14, AtrMult: 2.0, RiskPct: 0.005, MaxPosPct: 0.5}
}

// Donchian 唐奇安趋势策略。
type Donchian struct {
	mu sync.Mutex
	p  Params

	entryPx   float64     // 最近一次入场成交价（ApplyFill 回报）
	peakClose float64     // 入场后最高收盘（跟踪止损锚点）
	sizer_    sizer.Sizer // 仓位决策器（默认 ATR 波动率目标，可注入半凯利）

	signalBar int64 // 上次发信号的根 OpenTime（防同根/挂单未成交期重复发单）
	cooldown  int   // 信号后 N 根内不重发（市价单下一根必结算，2 根足够覆盖被拒重试）
}

// New 校验并构造策略。
func New(p Params) (*Donchian, error) {
	if p.EntryN < 2 || p.ExitN < 2 || p.AtrN < 2 {
		return nil, fmt.Errorf("trend: 窗口参数必须 ≥2，当前 entry=%d exit=%d atr=%d", p.EntryN, p.ExitN, p.AtrN)
	}
	if p.AtrMult <= 0 || math.IsNaN(p.AtrMult) || math.IsInf(p.AtrMult, 0) {
		return nil, fmt.Errorf("trend: ATR 止损倍数必须为有限正数，当前 %v", p.AtrMult)
	}
	if p.RiskPct <= 0 || p.RiskPct > 0.02 {
		return nil, fmt.Errorf("trend: 单笔风险比例必须在 (0, 2%%]（凯利纪律：下注过重正期望也破产），当前 %v", p.RiskPct)
	}
	if p.MaxPosPct <= 0 || p.MaxPosPct > 1 {
		return nil, fmt.Errorf("trend: 单笔仓位上限必须在 (0,1]，当前 %v", p.MaxPosPct)
	}
	return &Donchian{
		p: p, cooldown: 2, signalBar: -1,
		sizer_: sizer.NewAtrVolTarget(p.RiskPct, p.MaxPosPct),
	}, nil
}

// SetSizer 注入自定义仓位决策器（如 sizer.HalfKelly——回测统计反哺）。
// nil 恢复默认 ATR 波动率目标。
func (d *Donchian) SetSizer(s sizer.Sizer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s == nil {
		d.sizer_ = sizer.NewAtrVolTarget(d.p.RiskPct, d.p.MaxPosPct)
		return
	}
	d.sizer_ = s
}

func (d *Donchian) Name() string { return "trend_donchian" }
func (d *Donchian) Warmup() int {
	w := d.p.EntryN
	if d.p.ExitN > w {
		w = d.p.ExitN
	}
	if d.p.AtrN > w {
		w = d.p.AtrN
	}
	return w + 1
}

// Params 返回当前参数（lab 参数敏感性扫描用）。
func (d *Donchian) Params() Params { return d.p }

// String 参数串（报告留痕）。
func (p Params) String() string {
	return fmt.Sprintf("entry%d/exit%d/atr%d×%.1f/risk%.4f", p.EntryN, p.ExitN, p.AtrN, p.AtrMult, p.RiskPct)
}

// Describe 参数描述（walk-forward 报告留痕：每折用了什么参数）。
func (d *Donchian) Describe() string {
	return fmt.Sprintf("entry%d/exit%d/atr%d×%.1f/risk%.3f", d.p.EntryN, d.p.ExitN, d.p.AtrN, d.p.AtrMult, d.p.RiskPct)
}

// OnCandle 每根收盘 K 线产出订单意图（经风控与撮合落地）。
func (d *Donchian) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(ctx.Candles)
	if n == 0 {
		return nil
	}
	last := ctx.Candles[n-1]

	// 冷却：上次信号后 cooldown 根内不重发（挂单未成交/被拒时的重试节奏）
	if d.signalBar >= 0 {
		gap := 0
		for i := n - 1; i >= 0 && ctx.Candles[i].OpenTime >= d.signalBar; i-- {
			gap++
		}
		if gap <= d.cooldown {
			return nil
		}
	}

	// 持仓以账本为准（重启后内部状态可能丢失），锚点丢失时保守从当前价重新跟踪
	if ctx.Position > 0 && d.peakClose <= 0 {
		d.peakClose = last.Close
	}
	if ctx.Position > 0 && last.Close > d.peakClose {
		d.peakClose = last.Close
	}

	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)
	for i, c := range ctx.Candles {
		closes[i], highs[i], lows[i] = c.Close, c.High, c.Low
	}
	i := n - 1

	if ctx.Position <= 0 {
		upper, _ := indicators.Donchian(highs, lows, d.p.EntryN)
		if !math.IsNaN(upper[i]) && last.Close > upper[i] {
			atr := indicators.ATR(highs, lows, closes, d.p.AtrN)
			if math.IsNaN(atr[i]) || atr[i] <= 0 {
				return nil
			}
			qty := d.sizePosition(ctx, last.Close, atr[i])
			if qty <= 0 {
				return nil
			}
			d.signalBar = last.OpenTime
			return []strategy.OrderIntent{{
				Kind: "trend_entry", Side: exchange.Buy, Type: exchange.OrderMarket,
				Qty: qty, Note: fmt.Sprintf("突破 %d 根高点 %.2f", d.p.EntryN, upper[i]),
			}}
		}
		return nil
	}

	// 持仓（以账本为准）
	_, lower := indicators.Donchian(highs, lows, d.p.ExitN)
	atr := indicators.ATR(highs, lows, closes, d.p.AtrN)
	trailStop := math.Inf(-1)
	if !math.IsNaN(atr[i]) && atr[i] > 0 {
		trailStop = d.peakClose - d.p.AtrMult*atr[i]
	}
	breakLower := !math.IsNaN(lower[i]) && last.Close < lower[i]
	if breakLower || last.Close < trailStop {
		d.signalBar = last.OpenTime
		reason := "跌破通道下轨"
		if !breakLower {
			reason = fmt.Sprintf("ATR 跟踪止损 %.2f", trailStop)
		}
		return []strategy.OrderIntent{{
			Kind: "trend_exit", Side: exchange.Sell, Type: exchange.OrderMarket,
			Qty: ctx.Position, Note: reason,
		}}
	}
	return nil
}

// sizePosition 定仓：委托给当前 sizer（默认 ATR 波动率目标；可注入半凯利）。
// 现金不足由风控/冻结层兜底（拒单留痕，不静默缩量）。
func (d *Donchian) sizePosition(ctx *strategy.Context, price, atr float64) float64 {
	if ctx.Equity <= 0 || price <= 0 || atr <= 0 {
		return 0
	}
	return d.sizer_.Size(sizer.Input{Equity: ctx.Equity, Price: price, Cash: ctx.Cash, ATR: atr})
}

// ApplyFill 成交回报（backtest 引擎与执行器驱动）：跟踪入场价与止损锚点。
func (d *Donchian) ApplyFill(side exchange.Side, qty, price float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch side {
	case exchange.Buy:
		if qty > 0 {
			d.entryPx = price
			d.peakClose = price
		}
	case exchange.Sell:
		if qty > 0 {
			d.entryPx = 0
			d.peakClose = 0
		}
	}
}

// EntryPx 最近入场价（测试与诊断用）。
func (d *Donchian) EntryPx() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entryPx
}
