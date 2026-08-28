// 半凯利统计反哺闭环：回测 → 指标 → FromMetrics 生成仓位 → 再回测对比。
// 纪律：交易样本不足时 FromMetrics 拒绝——没有"证据不足还硬上凯利"的路径。
package lab

import (
	"fmt"
	"math"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/sizer"
	"github.com/HarveyBase/QuantForge/strategy"
	"github.com/HarveyBase/QuantForge/trend"
)

// KellyReport 半凯利闭环报告。
type KellyReport struct {
	Base      backtest.Metrics // 第一轮（ATR 波动率目标仓位）
	Kelly     backtest.Metrics // 第二轮（半凯利仓位）
	SizerDesc string           // 生成的仓位描述
	Note      string           // 结论留痕
	Trials    int
}

// KellyLoop 在同一样本上跑两轮：第一轮默认仓位出统计，第二轮半凯利仓位对比。
// 注意口径：同一样本内"统计→再跑"存在数据窥探风险，此工具用于研究敏感度，
// 真正部署应使用训练样本统计 + 样本外执行（walk-forward 分折喂参）。
func KellyLoop(candles []exchange.Candle, cost backtest.CostModel, seed float64,
	symbol, interval string, mkTrend func() *trend.Donchian, fraction, maxPosPct float64) (*KellyReport, error) {
	// 第一轮：默认 ATR 波动率目标
	eng1 := &backtest.Engine{Strategy: mkTrend(), Cost: cost, SeedCash: seed}
	res1, err := eng1.Run(candles, symbol, interval, 1)
	if err != nil {
		return nil, fmt.Errorf("lab: 基准回测失败: %w", err)
	}
	rep := &KellyReport{Base: res1.Metrics, Trials: 1}

	// 统计反哺（交易不足会被 FromMetrics 拒绝）
	hk, err := sizer.FromMetrics(res1.Metrics, fraction, maxPosPct)
	if err != nil {
		rep.Note = fmt.Sprintf("未生成半凯利仓位: %v（维持波动率目标仓位，此为正确行为）", err)
		return rep, nil
	}
	rep.SizerDesc = hk.Describe()

	// 第二轮：半凯利仓位
	t2 := mkTrend()
	t2.SetSizer(hk)
	eng2 := &backtest.Engine{Strategy: t2, Cost: cost, SeedCash: seed}
	res2, err := eng2.Run(candles, symbol, interval, 2)
	if err != nil {
		return nil, fmt.Errorf("lab: 半凯利回测失败: %w", err)
	}
	rep.Kelly = res2.Metrics
	rep.Trials = 2

	// 结论：半凯利通常降低收益同时降低回撤（先活着）。MDD 为负数，深浅按绝对值比较。
	switch {
	case math.Abs(rep.Kelly.MaxDrawdownPct) > math.Abs(rep.Base.MaxDrawdownPct)+1e-9:
		rep.Note = fmt.Sprintf("警告：半凯利回撤 %.2f%% 反而深于基准 %.2f%%——统计可疑，勿部署",
			rep.Kelly.MaxDrawdownPct, rep.Base.MaxDrawdownPct)
	case math.Abs(rep.Kelly.MaxDrawdownPct) < math.Abs(rep.Base.MaxDrawdownPct)-1e-9:
		rep.Note = fmt.Sprintf("回撤 %.2f%% → %.2f%%（降回撤优先于追收益；收益 %.2f%% → %.2f%%）",
			rep.Base.MaxDrawdownPct, rep.Kelly.MaxDrawdownPct, rep.Base.TotalReturnPct, rep.Kelly.TotalReturnPct)
	default:
		rep.Note = "回撤基本持平（凯利比例可能被上限压平）"
	}
	return rep, nil
}

var _ strategy.Strategy = (*trend.Donchian)(nil) // 编译期断言：Donchian 满足策略接口
