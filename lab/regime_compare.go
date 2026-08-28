// 市况分层验证：把样本按 regime 切段，对每段分别回测两个策略，输出互补性证据。
// 这是"震荡开网格、趋势开突破"自动路由的前置验证——路由开关要有回测依据才开。
package lab

import (
	"fmt"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/regime"
	"github.com/HarveyBase/QuantForge/strategy"
)

// RegimeSegmentResult 单段的策略对照成绩。
type RegimeSegmentResult struct {
	Kind        regime.Kind `json:"kind"`
	From        int64       `json:"from_ms"` // 段区间（OpenTime）
	To          int64       `json:"to_ms"`
	Bars        int         `json:"bars"`
	TrendRetPct float64     `json:"trend_ret_pct"` // 趋势策略段内收益
	GridRetPct  float64     `json:"grid_ret_pct"`  // 网格策略段内收益
	Better      string      `json:"better"`        // trend / grid / tie
}

// RegimeCompareReport 市况分层对照总报告。
type RegimeCompareReport struct {
	Segments []RegimeSegmentResult
	Trials   int
	// 互补性判定：趋势段趋势策略合计收益 > 网格、且震荡段网格 > 趋势策略 → 互补成立
	TrendSegTrendTotal, TrendSegGridTotal float64
	RangeSegTrendTotal, RangeSegGridTotal float64
	Complementary                         bool
	Reason                                string
}

// RegimeCompare 市况分层对照：trend 与 grid 在各市况段的表现。
// mkTrend/mkGrid 必须每次返回新实例（防跨段状态泄漏）。
func RegimeCompare(candles []exchange.Candle, cost backtest.CostModel, seed float64,
	symbol, interval string, mkTrend, mkGrid func() strategy.Strategy) (*RegimeCompareReport, error) {
	segs := regime.Segments(candles, regime.DefaultLookback, regime.DefaultConfirmBars)
	if len(segs) == 0 {
		return nil, fmt.Errorf("lab: 未能切出任何市况段（样本太短）")
	}
	rep := &RegimeCompareReport{Trials: 0}
	for _, seg := range segs {
		// 段切片：按 OpenTime 区间过滤（段起点可能非索引对齐，直接线性扫）
		var part []exchange.Candle
		for _, c := range candles {
			if c.OpenTime >= seg.From && c.OpenTime <= seg.To {
				part = append(part, c)
			}
		}
		if len(part) < 30 {
			continue // 段太短无统计意义
		}
		row := RegimeSegmentResult{Kind: seg.Kind, From: seg.From, To: seg.To, Bars: len(part)}
		for _, tc := range []struct {
			mk  func() strategy.Strategy
			ret *float64
		}{
			{mkTrend, &row.TrendRetPct},
			{mkGrid, &row.GridRetPct},
		} {
			eng := &backtest.Engine{Strategy: tc.mk(), Cost: cost, SeedCash: seed}
			res, err := eng.Run(part, symbol, interval, rep.Trials+1)
			if err != nil {
				return nil, fmt.Errorf("lab: 段 %s 回测失败: %w", seg.Kind, err)
			}
			rep.Trials++
			*tc.ret = res.Metrics.TotalReturnPct
		}
		switch {
		case row.TrendRetPct > row.GridRetPct+1e-9:
			row.Better = "trend"
		case row.GridRetPct > row.TrendRetPct+1e-9:
			row.Better = "grid"
		default:
			row.Better = "tie"
		}
		rep.Segments = append(rep.Segments, row)
		switch seg.Kind {
		case regime.Trending:
			rep.TrendSegTrendTotal += row.TrendRetPct
			rep.TrendSegGridTotal += row.GridRetPct
		case regime.Range:
			rep.RangeSegTrendTotal += row.TrendRetPct
			rep.RangeSegGridTotal += row.GridRetPct
		}
	}
	if len(rep.Segments) == 0 {
		return nil, fmt.Errorf("lab: 没有足够长（≥30 根）的市况段")
	}
	// 互补性判定（口径：分段收益合计，两条件都满足才成立）
	trendSide := rep.TrendSegTrendTotal > rep.TrendSegGridTotal
	rangeSide := rep.RangeSegGridTotal > rep.RangeSegTrendTotal
	rep.Complementary = trendSide && rangeSide
	rep.Reason = fmt.Sprintf("趋势段: trend %.2f%% vs grid %.2f%%；震荡段: grid %.2f%% vs trend %.2f%%",
		rep.TrendSegTrendTotal, rep.TrendSegGridTotal, rep.RangeSegGridTotal, rep.RangeSegTrendTotal)
	return rep, nil
}
