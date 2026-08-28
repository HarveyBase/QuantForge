// UMP 拦截器研究工作流：回测 → 提取交易情境 → 拦截器自身样本外验证。
// 纪律：拦截器在 OOS 验证通过前不得部署到 serve（docs/01：拦截规则也要过样本外）。
package lab

import (
	"fmt"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
	"github.com/HarveyBase/QuantForge/ump"
)

// UMPCheck 用回测成交样本验证统计拦截器的样本外有效性。
// 返回：交易样本数、OOS 报告。交易样本 < 2×minSamples 时报错（证据不足）。
func UMPCheck(candles []exchange.Candle, cost backtest.CostModel, seed float64,
	symbol, interval string, mk func() strategy.Strategy, minWinRate float64, minSamples int) (int, *ump.OOSReport, error) {
	eng := &backtest.Engine{Strategy: mk(), Cost: cost, SeedCash: seed}
	res, err := eng.Run(candles, symbol, interval, 1)
	if err != nil {
		return 0, nil, fmt.Errorf("lab: 回测失败: %w", err)
	}
	trades, err := ump.PairTrades(candles, res.Trades)
	if err != nil {
		return 0, nil, err
	}
	rep, err := ump.ValidateOOS(trades, minWinRate, minSamples)
	if err != nil {
		return len(trades), nil, err
	}
	return len(trades), rep, nil
}
