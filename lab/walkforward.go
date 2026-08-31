// Package lab 研究验证工具（docs/01 防过拟合纪律的工程落地）：
// walk-forward 滚动验证、参数邻域高原检验、成本敏感性扫描。
// 铁律：参数选择只发生在训练窗，测试窗结构上不可见（防数据窥探由类型系统保证）；
// 全部试验含失败都计入 TotalTrials（多次回测须累计申报）。
package lab

import (
	"fmt"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

// WFConfig walk-forward 滚动窗口配置。
type WFConfig struct {
	TrainBars int // 训练窗（根）
	TestBars  int // 测试窗（根），同时是滚动步长
	SeedCash  float64
	Cost      backtest.CostModel
	Symbol    string
	Interval  string
}

// FoldResult 单折结果：训练窗上选出的策略描述 + 测试窗（样本外）表现。
type FoldResult struct {
	TrainFrom        int64  `json:"train_from_ms"` // 训练窗（ms）
	TrainTo          int64  `json:"train_to_ms"`
	TestFrom         int64  `json:"test_from_ms"`
	TestTo           int64  `json:"test_to_ms"`
	Strategy         string `json:"strategy"` // 本折选出的策略描述（参数留痕）
	Fold             int    `json:"fold"`
	backtest.Metrics        // 测试窗指标
	RiskRejections   int
}

// WFReport walk-forward 总报告。
type WFReport struct {
	Folds               []FoldResult           `json:"folds"`
	OOSMetrics          backtest.Metrics       `json:"oos_metrics"`  // 样本外拼接曲线复算
	OOSCurve            []backtest.EquityPoint `json:"oos_curve"`    // 复合归一
	BuyHoldPct          float64                `json:"buy_hold_pct"` // 同区间买入持有对照
	TotalTrials         int                    `json:"total_trials"` // 全部试验数（含训练窗搜索）
	Candles             int                    `json:"candles"`
	TrainBars, TestBars int                    `json:",omitempty"`
}

// StrategySelector 在训练窗上产出要上测试窗的策略。
// 返回值：策略（每折必须新建实例，防跨折状态泄漏）、参数描述、本折消耗的试验数（网格搜索时为网格大小）。
// selectStrategy 只能拿到训练窗切片——测试窗在结构上不可见。
type StrategySelector func(train []exchange.Candle, trialBase int) (s strategy.Strategy, describe string, trialsUsed int, err error)

// WalkForward 滚动验证：train 选参 → test 出样本外成绩 → 滚动推进。
// candles 必须已通过 market.Validate。
func WalkForward(candles []exchange.Candle, cfg WFConfig, selectStrategy StrategySelector) (*WFReport, error) {
	if err := validateWF(cfg); err != nil {
		return nil, err
	}
	if len(candles) < cfg.TrainBars+cfg.TestBars {
		return nil, fmt.Errorf("lab: 样本 %d 根不足一个窗口（train %d + test %d）", len(candles), cfg.TrainBars, cfg.TestBars)
	}

	rep := &WFReport{Candles: len(candles), TrainBars: cfg.TrainBars, TestBars: cfg.TestBars}
	trialBase := 0
	for start := 0; start+cfg.TrainBars+cfg.TestBars <= len(candles); start += cfg.TestBars {
		trainEnd := start + cfg.TrainBars
		testEnd := trainEnd + cfg.TestBars
		train := candles[:trainEnd] // 扩张式训练窗（anchored）：从头到训练窗尾的全部历史
		test := candles[trainEnd:testEnd]

		s, desc, used, err := selectStrategy(train, trialBase)
		if err != nil {
			return nil, fmt.Errorf("lab: 第 %d 折训练失败: %w", len(rep.Folds)+1, err)
		}
		trialBase += used

		eng := &backtest.Engine{Strategy: s, Cost: cfg.Cost, SeedCash: cfg.SeedCash}
		res, err := eng.Run(test, cfg.Symbol, cfg.Interval, trialBase)
		if err != nil {
			return nil, fmt.Errorf("lab: 第 %d 折测试失败: %w", len(rep.Folds)+1, err)
		}
		trialBase++ // 测试本身也是一次试验
		rep.Folds = append(rep.Folds, FoldResult{
			TrainFrom: train[start].OpenTime, TrainTo: train[len(train)-1].OpenTime,
			TestFrom: test[0].OpenTime, TestTo: test[len(test)-1].OpenTime,
			Strategy: desc, Fold: len(rep.Folds) + 1,
			Metrics: res.Metrics, RiskRejections: len(res.RiskRejections),
		})
		rep.OOSCurve = appendFoldCurve(rep.OOSCurve, res.EquityCurve, cfg.SeedCash)
	}

	// 样本外拼接指标 + 同区间买入持有对照
	first, last := candles[cfg.TrainBars], candles[len(candles)-1]
	rep.OOSMetrics = backtest.ComputeMetrics(rep.OOSCurve, nil, cfg.SeedCash, first.Close, last.Close)
	rep.BuyHoldPct = (last.Close/first.Close - 1) * 100
	rep.TotalTrials = trialBase
	return rep, nil
}

// appendFoldCurve 把单折测试曲线按相对收益复合拼进总曲线（各折独立本金归一）。
func appendFoldCurve(dst []backtest.EquityPoint, fold []backtest.EquityPoint, seed float64) []backtest.EquityPoint {
	if len(fold) == 0 {
		return dst
	}
	prev := seed // 上一折末权益（首折为种子资金）
	if len(dst) > 0 {
		prev = dst[len(dst)-1].Equity
	}
	base := fold[0].Equity
	if base <= 0 {
		return dst
	}
	for _, p := range fold {
		dst = append(dst, backtest.EquityPoint{Ts: p.Ts, Equity: p.Equity / base * prev, Price: p.Price})
	}
	return dst
}

func validateWF(cfg WFConfig) error {
	if cfg.TrainBars < 30 {
		return fmt.Errorf("lab: 训练窗至少 30 根，当前 %d", cfg.TrainBars)
	}
	if cfg.TestBars < 5 {
		return fmt.Errorf("lab: 测试窗至少 5 根，当前 %d", cfg.TestBars)
	}
	if cfg.SeedCash <= 0 {
		return fmt.Errorf("lab: 种子资金必须为正，当前 %v", cfg.SeedCash)
	}
	return nil
}

// FixedSelector 固定参数策略选择器（不做训练窗搜索——用作对照或参数已知场景）。
func FixedSelector(mk func() strategy.Strategy, desc string) StrategySelector {
	return func(train []exchange.Candle, trialBase int) (strategy.Strategy, string, int, error) {
		return mk(), desc, 1, nil
	}
}
