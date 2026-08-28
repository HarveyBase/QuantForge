// Package sizer 仓位决策抽象：把「买多少」从策略中解耦（abu 对照表 #2）。
// 两个实现：
//   - AtrVolTarget：波动率目标（波动大自动减仓）——趋势策略默认；
//   - HalfKelly：半凯利——吃回测统计（胜率/平均盈亏），回测统计→仓位参数的闭环。
//
// 纪律（docs/04 skill：凯利仓位哲学）：回测胜率是历史值、真实 p 不可知，
// 必须用半凯利（×0.5 或更保守）；证据不足（交易数少）禁止上凯利。
package sizer

import (
	"fmt"

	"github.com/HarveyBase/QuantForge/backtest"
)

// Input 仓位决策输入（决策时点可见数据，防前视由调用方保证）。
type Input struct {
	Equity float64 // 当前权益
	Price  float64 // 信号价（收盘价）
	Cash   float64 // 可用现金
	ATR    float64 // 当前 ATR（绝对值，可选：波动率目标用）
}

// Sizer 仓位决策器：返回买入数量（Base）；0 表示不下注。
type Sizer interface {
	Size(in Input) float64
	Describe() string
}

// AtrVolTarget 波动率目标仓位：qty 使每根 K 线的 ATR 波动 ≈ equity × RiskPct。
// 波动大自动减仓、波动小自动加仓，再以 MaxPosPct 封顶（永不满仓）。
type AtrVolTarget struct {
	RiskPct   float64 // 单笔风险占权益比例（0.005 = 0.5%/根）
	MaxPosPct float64 // 单笔名义占权益上限
}

// NewAtrVolTarget 构造（非法参数回退默认）。
func NewAtrVolTarget(riskPct, maxPosPct float64) *AtrVolTarget {
	if riskPct <= 0 || riskPct > 0.02 {
		riskPct = 0.005 // 凯利纪律：单根风险不超过 2%
	}
	if maxPosPct <= 0 || maxPosPct > 1 {
		maxPosPct = 0.5
	}
	return &AtrVolTarget{RiskPct: riskPct, MaxPosPct: maxPosPct}
}

// Size 波动率目标定仓。
func (a *AtrVolTarget) Size(in Input) float64 {
	if in.Equity <= 0 || in.Price <= 0 || in.ATR <= 0 {
		return 0
	}
	qty := in.Equity * a.RiskPct / in.ATR
	if capQty := in.Equity * a.MaxPosPct / in.Price; qty > capQty {
		qty = capQty
	}
	if cashQty := in.Cash / in.Price; in.Cash >= 0 && qty > cashQty {
		qty = cashQty // 现金约束（零现金=买不起；冻结层会兜底，这里提前收敛减少拒单噪音）
	}
	return qty
}

// Describe 参数留痕。
func (a *AtrVolTarget) Describe() string {
	return fmt.Sprintf("atr_vol_target(risk=%.4f,max=%.2f)", a.RiskPct, a.MaxPosPct)
}

// HalfKelly 半凯利仓位：f* = W − (1−W)/(avgWin/avgLoss)，仓位 = f* × Fraction。
// 输入必须是样本外可信的统计（FromMetrics 有最小交易数门槛）。
type HalfKelly struct {
	WinRate   float64 // 胜率 [0,1]
	AvgWin    float64 // 平均盈利幅度（正数，如 0.08 = 8%）
	AvgLoss   float64 // 平均亏损幅度（正数，如 0.04）
	Fraction  float64 // 凯利折扣：0.5 半凯利 / 0.3 更保守
	MaxPosPct float64 // 单笔名义上限
}

// KellyFrac 全额凯利比例（未折扣）。
func (h *HalfKelly) KellyFrac() float64 {
	if h.WinRate <= 0 || h.AvgWin <= 0 || h.AvgLoss <= 0 {
		return 0 // 证据退化：不下注
	}
	b := h.AvgWin / h.AvgLoss // 盈亏比
	f := h.WinRate - (1-h.WinRate)/b
	if f < 0 {
		return 0 // 负期望：不下注（60% 胜率 + 50% 仓位，100 把后本金趋近 0）
	}
	if f > 1 {
		f = 1
	}
	return f
}

// Size 半凯利定仓：权益 × f* × Fraction，封顶 MaxPosPct 与现金。
func (h *HalfKelly) Size(in Input) float64 {
	if in.Equity <= 0 || in.Price <= 0 {
		return 0
	}
	frac := h.KellyFrac() * h.Fraction
	if frac <= 0 {
		return 0
	}
	qty := in.Equity * frac / in.Price
	if capQty := in.Equity * h.MaxPosPct / in.Price; qty > capQty {
		qty = capQty
	}
	if cashQty := in.Cash / in.Price; in.Cash >= 0 && qty > cashQty {
		qty = cashQty
	}
	return qty
}

// Describe 参数留痕。
func (h *HalfKelly) Describe() string {
	return fmt.Sprintf("half_kelly(f*=%.3f×%.2f, W=%.2f, b=%.2f)",
		h.KellyFrac(), h.Fraction, h.WinRate, h.AvgWin/max(h.AvgLoss, 1e-12))
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// FromMetricsMinTrades 统计反哺的最小交易数：低于此值回测统计不可信（防数据窥探式自信）。
const FromMetricsMinTrades = 20

// FromMetrics 从回测指标生成半凯利仓位（统计反哺闭环的入口）。
// TradeCount < minTrades 或缺少盈亏统计时拒绝——证据不足不上凯利。
func FromMetrics(m backtest.Metrics, fraction, maxPosPct float64) (*HalfKelly, error) {
	if m.TradeCount < FromMetricsMinTrades {
		return nil, fmt.Errorf("sizer: 交易 %d 笔不足 %d——回测统计不可信，禁止凯利定仓（证据不足）", m.TradeCount, FromMetricsMinTrades)
	}
	if m.AvgLossPct <= 0 {
		return nil, fmt.Errorf("sizer: 无亏损样本（AvgLossPct=0）——盈亏比不可估，禁止凯利定仓")
	}
	if fraction <= 0 || fraction > 1 {
		fraction = 0.5 // 默认半凯利
	}
	if maxPosPct <= 0 || maxPosPct > 1 {
		maxPosPct = 0.5
	}
	return &HalfKelly{
		WinRate:   m.WinRate / 100,
		AvgWin:    m.AvgWinPct / 100,
		AvgLoss:   m.AvgLossPct / 100,
		Fraction:  fraction,
		MaxPosPct: maxPosPct,
	}, nil
}
