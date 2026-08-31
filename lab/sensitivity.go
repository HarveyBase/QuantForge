// 参数敏感性与成本敏感性检验（docs/01：好策略参数邻域是"高原"，过拟合是"孤针"）。
package lab

import (
	"fmt"
	"sort"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

// ParamPoint 单个参数点的回测成绩。
type ParamPoint struct {
	Label  string  `json:"label"`
	Ret    float64 `json:"ret"` // 总收益 %
	Mdd    float64 `json:"mdd"` // 最大回撤 %（负值）
	Calmar float64 `json:"calmar"`
	Trades int     `json:"trades"`
}

// ScanParams 参数扫描：对每个参数实例跑一次回测（试验数必须累计申报）。
// mk 由调用方闭包参数到策略的映射；labels 与 mk 一一对应。
func ScanParams(candles []exchange.Candle, cfg backtest.CostModel, seed float64,
	symbol, interval string, mk func(label string) strategy.Strategy, labels ...string) ([]ParamPoint, int, error) {
	if len(labels) == 0 {
		return nil, 0, fmt.Errorf("lab: 参数扫描至少需要一个点")
	}
	out := make([]ParamPoint, 0, len(labels))
	for i, label := range labels {
		eng := &backtest.Engine{Strategy: mk(label), Cost: cfg, SeedCash: seed}
		res, err := eng.Run(candles, symbol, interval, i+1)
		if err != nil {
			return nil, i, fmt.Errorf("lab: 参数 %s 回测失败: %w", label, err)
		}
		out = append(out, ParamPoint{Label: label, Ret: res.Metrics.TotalReturnPct, Mdd: res.Metrics.MaxDrawdownPct, Calmar: res.Metrics.Calmar, Trades: res.Metrics.TradeCount})
	}
	return out, len(labels), nil
}

// PlateauCheck 参数邻域高原检验。
// points 首元素视为基准点，其余为其邻域；decay 为邻域保真阈值（如 0.5 = 邻域中位收益不低于基准一半）。
// 判定：邻域收益中位数 ≥ 基准收益 × decay 且基准收益 > 0 → 高原（参数可迁移）；
// 否则孤针（过拟合警报，降级为研究线索，禁止晋级）。
type PlateauReport struct {
	Base      ParamPoint   `json:"base"`
	Neighbors []ParamPoint `json:"neighbors"`
	MedianRet float64      `json:"median_ret"` // 邻域收益中位数
	IsPlateau bool         `json:"is_plateau"`
	Reason    string       `json:"reason"`
}

func PlateauCheck(points []ParamPoint, decay float64) (*PlateauReport, error) {
	if len(points) < 2 {
		return nil, fmt.Errorf("lab: 高原检验至少需要基准点 + 1 个邻域点")
	}
	if decay <= 0 || decay > 1 {
		return nil, fmt.Errorf("lab: decay 必须在 (0,1]，当前 %v", decay)
	}
	base := points[0]
	rets := make([]float64, 0, len(points)-1)
	for _, p := range points[1:] {
		rets = append(rets, p.Ret)
	}
	sort.Float64s(rets)
	median := rets[len(rets)/2]
	rep := &PlateauReport{Base: base, Neighbors: points[1:], MedianRet: median}
	switch {
	case base.Ret <= 0:
		rep.Reason = fmt.Sprintf("基准点收益 %.2f%% 非正，无参数可迁移性可言", base.Ret)
	case median < base.Ret*decay:
		rep.Reason = fmt.Sprintf("孤针：邻域中位收益 %.2f%% 低于基准 %.2f%% × %.0f%%（参数邻域塌陷，过拟合警报）", median, base.Ret, decay*100)
	default:
		rep.IsPlateau = true
		rep.Reason = fmt.Sprintf("高原：邻域中位收益 %.2f%% ≥ 基准 %.2f%% × %.0f%%（参数可迁移）", median, base.Ret, decay*100)
	}
	return rep, nil
}

// CostPoint 成本敏感性单点。
type CostPoint struct {
	Multiplier float64 `json:"multiplier"` // 成本倍数（1 = 基准）
	Ret        float64 `json:"ret"`
	Mdd        float64 `json:"mdd"`
	Trades     int     `json:"trades"`
}

// CostScan 成本敏感性扫描：把滑点与费率同乘倍数重跑（mults 如 {0, 0.5, 1, 2, 4}）。
// 判定口径由调用方掌握：若 2× 成本下收益转负，策略边际被成本吃穿，高频化不可行。
func CostScan(candles []exchange.Candle, base backtest.CostModel, seed float64,
	symbol, interval string, mk func() strategy.Strategy, mults ...float64) ([]CostPoint, int, error) {
	if len(mults) == 0 {
		return nil, 0, fmt.Errorf("lab: 成本扫描至少需要一个倍数点")
	}
	out := make([]CostPoint, 0, len(mults))
	for i, m := range mults {
		cost := backtest.CostModel{
			SlippageBps: base.SlippageBps * m,
			MakerFeeBps: base.MakerFeeBps * m,
			TakerFeeBps: base.TakerFeeBps * m,
		}
		eng := &backtest.Engine{Strategy: mk(), Cost: cost, SeedCash: seed}
		res, err := eng.Run(candles, symbol, interval, i+1)
		if err != nil {
			return nil, i, fmt.Errorf("lab: 成本倍数 %v 回测失败: %w", m, err)
		}
		out = append(out, CostPoint{Multiplier: m, Ret: res.Metrics.TotalReturnPct, Mdd: res.Metrics.MaxDrawdownPct, Trades: res.Metrics.TradeCount})
	}
	return out, len(mults), nil
}
