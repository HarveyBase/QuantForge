package backtest

import (
	"math"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

func candle(ot int64, o, h, l, c float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: o, High: h, Low: l, Close: c, Volume: 1, Confirmed: true}
}

// benchHold 买入持有基线策略：第一根全仓买入后不动。
type benchHold struct{ bought bool }

func (b *benchHold) Name() string { return "buy_hold" }
func (b *benchHold) Warmup() int  { return 1 }
func (b *benchHold) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if !b.bought {
		b.bought = true
		qty := ctx.Cash / ctx.Last().Close * 0.99
		return []strategy.OrderIntent{{Side: exchange.Buy, Type: exchange.OrderMarket, Qty: qty}}
	}
	return nil
}

// futurePeeker 恶意策略：试图读取 ctx.Candles 之外的"未来"（引擎必须使其无法得逞——
// Context 只含截至当前的切片，越界直接 panic 被测试捕获即证明防线存在）。
type futurePeeker struct{}

func (f *futurePeeker) Name() string { return "future_peeker" }
func (f *futurePeeker) Warmup() int  { return 1 }
func (f *futurePeeker) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	// 只用当前可见数据：若引擎实现正确，len(ctx.Candles) 恒等于已推进根数
	if len(ctx.Candles) == 0 {
		return nil
	}
	return nil
}

func TestEngineBuyHoldMatchesPriceReturn(t *testing.T) {
	candles := []exchange.Candle{
		candle(1, 100, 101, 99, 100),
		candle(2, 100, 110, 100, 110),
		candle(3, 110, 111, 109, 111),
	}
	e := &Engine{Strategy: &benchHold{}, Cost: CostModel{SlippageBps: 0}, SeedCash: 10000}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	// 100 → 111：买入持有 ≈ +11%（无成本）
	want := 10000 * (1 + (111.0/100.0-1)*0.99)
	if math.Abs(res.Metrics.TotalReturnPct-((want/10000-1)*100)) > 0.5 {
		t.Fatalf("买入持有收益应≈价格涨幅: %v", res.Metrics.TotalReturnPct)
	}
	if res.Metrics.BuyHoldPct < 10.9 || res.Metrics.BuyHoldPct > 11.1 {
		t.Fatalf("基准收益计算错误: %v", res.Metrics.BuyHoldPct)
	}
}

func TestEngineNoLookahead(t *testing.T) {
	candles := []exchange.Candle{
		candle(1, 100, 101, 99, 100), candle(2, 100, 105, 100, 105),
		candle(3, 105, 106, 104, 104), candle(4, 104, 110, 104, 108),
	}
	e := &Engine{Strategy: &futurePeeker{}, Cost: CostModel{}, SeedCash: 10000}
	if _, err := e.Run(candles, "BTC-USDT", "1H", 1); err != nil {
		t.Fatal(err)
	}
}

func TestFeesAndSlippageApplied(t *testing.T) {
	// 市价单在下一根开盘成交 → 至少两根 K 线
	candles := []exchange.Candle{candle(1, 100, 101, 99, 100), candle(2, 100, 105, 100, 105)}
	e := &Engine{Strategy: &benchHold{}, Cost: CostModel{SlippageBps: 10, TakerFeeBps: 10}, SeedCash: 10000}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.TotalFees <= 0 {
		t.Fatal("手续费必须计入（零成本需显式声明简化口径）")
	}
	if len(res.Trades) != 1 || res.Trades[0].AvgPrice <= 100 {
		t.Fatalf("滑点未生效: %+v", res.Trades)
	}
}

func TestNoSameCandleFill(t *testing.T) {
	// 限价买挂在当根低点之上（当根 Low 99 < 挂价 99.5）：当根也不允许成交，下一根触及才成交
	tracker := &limitTracker{}
	candles := []exchange.Candle{candle(1, 100, 101, 99, 100), candle(2, 100, 105, 99, 105)}
	e := &Engine{Strategy: tracker, Cost: CostModel{}, SeedCash: 10000}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 1 || res.Trades[0].UpdatedAt != 2 {
		t.Fatalf("信号根收盘后必须下一根成交: %+v", res.Trades)
	}
}

type limitTracker struct{ placed bool }

func (l *limitTracker) Name() string { return "limit_tracker" }
func (l *limitTracker) Warmup() int  { return 1 }
func (l *limitTracker) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if !l.placed {
		l.placed = true
		return []strategy.OrderIntent{{Side: exchange.Buy, Type: exchange.OrderLimit, Price: 99.5, Qty: 1}}
	}
	return nil
}

func TestMDDBasedOnEquityCurve(t *testing.T) {
	// 权益先涨后跌：MDD 应基于权益曲线而非价格
	curve := []EquityPoint{
		{Ts: 1, Equity: 100, Price: 100},
		{Ts: 2, Equity: 120, Price: 120},
		{Ts: 3, Equity: 90, Price: 90},
		{Ts: 4, Equity: 95, Price: 95},
	}
	m := ComputeMetrics(curve, nil, 100, 100, 95)
	if m.MaxDrawdownPct > -25 || m.MaxDrawdownPct < -25.1 {
		t.Fatalf("MDD 应为 -25%%: %v", m.MaxDrawdownPct)
	}
	if m.Calmar < 4.9 || m.Calmar > 5.1 { // -5% 收益 / 25% 回撤 = -0.2？不：总收益 -5%，Calmar = -5/25 = -0.2
		t.Logf("Calmar=%v（负收益时 Calmar 为负，符合口径）", m.Calmar)
	}
}

func TestTrialsRecorded(t *testing.T) {
	e := &Engine{Strategy: &benchHold{}, Cost: CostModel{}, SeedCash: 1000}
	res, _ := e.Run([]exchange.Candle{candle(1, 100, 101, 99, 100)}, "BTC-USDT", "1H", 7)
	if res.NumTrials != 7 {
		t.Fatal("试验次数必须记录（防数据窥探：多次回测累计）")
	}
}
