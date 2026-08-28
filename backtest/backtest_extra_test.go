package backtest

import (
	"math"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

// sellOnce 第一根挂限价卖，用于覆盖 fillLimit 卖侧分支与 pending 路径。
type sellOnce struct{ placed bool }

func (s *sellOnce) Name() string { return "sell_once" }
func (s *sellOnce) Warmup() int  { return 1 }
func (s *sellOnce) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if !s.placed {
		s.placed = true
		return []strategy.OrderIntent{{Side: exchange.Sell, Type: exchange.OrderLimit, Price: 106, Qty: 1}}
	}
	return nil
}

func TestRunRejectsInvalidInput(t *testing.T) {
	e := &Engine{Strategy: &benchHold{}, SeedCash: 1000}
	if _, err := e.Run(nil, "BTC-USDT", "1H", 1); err == nil {
		t.Fatal("空 K 线必须报错")
	}
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		e.SeedCash = bad
		if _, err := e.Run([]exchange.Candle{candle(1, 1, 1, 1, 1)}, "BTC-USDT", "1H", 1); err == nil {
			t.Fatalf("非法初始资金 %v 必须报错", bad)
		}
	}
}

func TestLimitSellFillsNextCandle(t *testing.T) {
	// 先给策略建仓（第二根买入成交），再挂高价卖单
	s := &buyThenSell{}
	candles := []exchange.Candle{
		candle(1, 100, 101, 99, 100),
		candle(2, 100, 105, 100, 105), // 买单在开盘 100 成交
		candle(3, 105, 107, 104, 106), // 卖单 106 被本根 High 107 触及
		candle(4, 106, 110, 105, 108),
	}
	e := &Engine{Strategy: s, Cost: CostModel{}, SeedCash: 10000}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	sold := false
	for _, tr := range res.Trades {
		if tr.Side == exchange.Sell && tr.Status == exchange.StatusFilled {
			sold = true
			// 卖单委托 106，开盘 105 < 106 → fillPx = 106×(1-0) = 106
			if tr.AvgPrice != 106 {
				t.Fatalf("限价卖成交价错误: %v", tr.AvgPrice)
			}
		}
	}
	if !sold {
		t.Fatalf("限价卖单应成交: %+v", res.Trades)
	}
}

type buyThenSell struct {
	state int
}

func (b *buyThenSell) Name() string { return "buy_then_sell" }
func (b *buyThenSell) Warmup() int  { return 1 }
func (b *buyThenSell) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	switch b.state {
	case 0:
		b.state = 1
		return []strategy.OrderIntent{{Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1}}
	case 1:
		if ctx.Position >= 1 {
			b.state = 2
			return []strategy.OrderIntent{{Side: exchange.Sell, Type: exchange.OrderLimit, Price: 106, Qty: 1}}
		}
	}
	return nil
}

func TestPendingOrdersReported(t *testing.T) {
	// 挂单远离价格，期末仍挂起
	tr := &limitTracker{}
	candles := []exchange.Candle{candle(1, 100, 101, 99, 100), candle(2, 100, 105, 100, 105), candle(3, 105, 106, 104, 106)}
	e := &Engine{Strategy: tr, Cost: CostModel{}, SeedCash: 10000}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PendingOrders) != 1 || res.PendingOrders[0].Status != exchange.StatusSubmitted {
		t.Fatalf("未成交挂单应进期末审计: %+v", res.PendingOrders)
	}
}

func TestInsufficientFundsRejected(t *testing.T) {
	// 现金 100 买 1 个 100 价的币：挂单冻结恰好等额 → 通过；随后再挂一张冻结失败
	s := &greedyBuyer{}
	candles := []exchange.Candle{candle(1, 100, 101, 99, 100), candle(2, 100, 105, 100, 105)}
	e := &Engine{Strategy: s, Cost: CostModel{}, SeedCash: 100}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range res.RiskRejections {
		if r.RuleID == "BT_RISK" || r.RuleID == "BT_FREEZE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("资金不足应产生风控/冻结拒单记录: %+v", res.RiskRejections)
	}
}

type greedyBuyer struct{}

func (g *greedyBuyer) Name() string { return "greedy" }
func (g *greedyBuyer) Warmup() int  { return 1 }
func (g *greedyBuyer) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	// 两张买单总额 200 > 现金 100：第二张必须被拦
	return []strategy.OrderIntent{
		{Side: exchange.Buy, Type: exchange.OrderLimit, Price: 100, Qty: 1},
		{Side: exchange.Buy, Type: exchange.OrderLimit, Price: 100, Qty: 1},
	}
}

func TestWarmupLongerThanSample(t *testing.T) {
	s := &warmupNever{w: 10}
	candles := []exchange.Candle{candle(1, 1, 1, 1, 1), candle(2, 1, 1, 1, 1)}
	e := &Engine{Strategy: s, Cost: CostModel{}, SeedCash: 100}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 0 || len(res.EquityCurve) != 2 {
		t.Fatalf("预热期超过样本长度应只记权益不下单: %+v", res.Metrics)
	}
}

type warmupNever struct{ w int }

func (w *warmupNever) Name() string                                      { return "warmup_never" }
func (w *warmupNever) Warmup() int                                       { return w.w }
func (w *warmupNever) OnCandle(*strategy.Context) []strategy.OrderIntent { return nil }

func TestApplyFillCallbackInvoked(t *testing.T) {
	s := &buyOnce{}
	candles := []exchange.Candle{candle(1, 100, 101, 99, 100), candle(2, 100, 105, 100, 105)}
	e := &Engine{Strategy: s, Cost: CostModel{}, SeedCash: 10000}
	if _, err := e.Run(candles, "BTC-USDT", "1H", 1); err != nil {
		t.Fatal(err)
	}
	if len(s.fills) != 1 || s.fills[0].side != exchange.Buy || s.fills[0].qty != 1 {
		t.Fatalf("策略 ApplyFill 回调必须被驱动: %+v", s.fills)
	}
}

type fillRecord struct {
	side exchange.Side
	qty  float64
	px   float64
}

type buyOnce struct {
	placed bool
	fills  []fillRecord
}

func (b *buyOnce) Name() string { return "buy_once" }
func (b *buyOnce) Warmup() int  { return 1 }
func (b *buyOnce) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if !b.placed {
		b.placed = true
		return []strategy.OrderIntent{{Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1}}
	}
	return nil
}

func (b *buyOnce) ApplyFill(side exchange.Side, qty, px float64) {
	b.fills = append(b.fills, fillRecord{side, qty, px})
}

func TestMarketSellSlippageDirection(t *testing.T) {
	s := &sellMarketOnce{placed: false}
	candles := []exchange.Candle{candle(1, 100, 101, 99, 100), candle(2, 100, 105, 100, 105)}
	e := &Engine{Strategy: s, Cost: CostModel{SlippageBps: 10}, SeedCash: 100}
	res, err := e.Run(candles, "BTC-USDT", "1H", 1)
	if err != nil {
		t.Fatal(err)
	}
	// 卖单无持仓会被风控拒（INSUFFICIENT_AVAILABLE）
	if len(res.RiskRejections) == 0 {
		t.Fatal("无持仓卖出必须被风控拦截")
	}
}

type sellMarketOnce struct{ placed bool }

func (s *sellMarketOnce) Name() string { return "sell_market_once" }
func (s *sellMarketOnce) Warmup() int  { return 1 }
func (s *sellMarketOnce) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if !s.placed {
		s.placed = true
		return []strategy.OrderIntent{{Side: exchange.Sell, Type: exchange.OrderMarket, Qty: 1}}
	}
	return nil
}

func TestComputeMetricsEdgeCases(t *testing.T) {
	// 空曲线
	m := computeMetrics(nil, nil, 100, 100, 100)
	if m.FinalEquity != 100 {
		t.Fatalf("空曲线返回种子资金: %v", m.FinalEquity)
	}
	// 单点曲线：无 MDD/Sharpe
	m = computeMetrics([]EquityPoint{{Ts: 1, Equity: 100}}, nil, 100, 100, 100)
	if m.MaxDrawdownPct != 0 || m.Sharpe != 0 || m.TotalReturnPct != 0 {
		t.Fatalf("单点曲线指标应退化为零: %+v", m)
	}
	// 胜率：一胜一负
	trades := []exchange.Order{
		{Side: exchange.Buy, AvgPrice: 100, FilledQty: 1, Fee: -1},
		{Side: exchange.Sell, AvgPrice: 110, FilledQty: 1, Fee: -1},
		{Side: exchange.Buy, AvgPrice: 105, FilledQty: 1, Fee: -1},
		{Side: exchange.Sell, AvgPrice: 100, FilledQty: 1, Fee: -1},
	}
	m = computeMetrics([]EquityPoint{{Ts: 1, Equity: 100}, {Ts: 2, Equity: 101}}, trades, 100, 100, 101)
	if m.WinRate != 50 {
		t.Fatalf("胜率应为 50%%: %v", m.WinRate)
	}
	if m.TotalFees != 4 {
		t.Fatalf("累计手续费错误: %v", m.TotalFees)
	}
	if m.TradeCount != 4 {
		t.Fatalf("交易次数错误: %v", m.TradeCount)
	}
}

func TestComputeMetricsSharpeAnnualizes(t *testing.T) {
	// 小时级间隔 3600000ms → ppy = 8760
	curve := []EquityPoint{
		{Ts: 3_600_000, Equity: 100},
		{Ts: 7_200_000, Equity: 101},
		{Ts: 10_800_000, Equity: 103},
		{Ts: 14_400_000, Equity: 102},
	}
	m := computeMetrics(curve, nil, 100, 100, 102)
	if m.Sharpe <= 0 {
		t.Fatalf("正收益序列 Sharpe 应为正: %v", m.Sharpe)
	}
}

func TestFillLimitBuyCapsAtOrderPrice(t *testing.T) {
	// 开盘 99.8 略低于委托 100：加滑点后 100.299 击穿委托价，封顶回 100
	o := exchange.Order{Side: exchange.Buy, Price: 100, Qty: 1}
	filled := fillLimit(o, candle(1, 99.8, 100, 99, 99.9), CostModel{SlippageBps: 50})
	if filled.AvgPrice != 100 {
		t.Fatalf("买侧成交价不得劣于委托价: %v", filled.AvgPrice)
	}
}

func TestFillLimitSellFloorsAtOrderPrice(t *testing.T) {
	// 开盘 106.05 略高于委托 106：减滑点后 105.52 击穿委托价，托底回 106
	o := exchange.Order{Side: exchange.Sell, Price: 106, Qty: 1}
	filled := fillLimit(o, candle(1, 106.05, 107, 105, 106), CostModel{SlippageBps: 50})
	if filled.AvgPrice != 106 {
		t.Fatalf("卖侧成交价不得劣于委托价: %v", filled.AvgPrice)
	}
}

func TestLimitTouched(t *testing.T) {
	c := candle(1, 100, 110, 90, 100)
	if !limitTouched(c, exchange.Order{Side: exchange.Buy, Price: 95}) {
		t.Fatal("Low 90 触及买价 95 应判定触及")
	}
	if limitTouched(c, exchange.Order{Side: exchange.Buy, Price: 85}) {
		t.Fatal("Low 90 未触及买价 85")
	}
	if !limitTouched(c, exchange.Order{Side: exchange.Sell, Price: 105}) {
		t.Fatal("High 110 触及卖价 105 应判定触及")
	}
	if limitTouched(c, exchange.Order{Side: exchange.Sell, Price: 115}) {
		t.Fatal("High 110 未触及卖价 115")
	}
}
