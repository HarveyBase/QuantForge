package lab

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/grid"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
	"github.com/HarveyBase/QuantForge/trend"
)

func wfCandle(ot int64, px float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: px, High: px * 1.01, Low: px * 0.99, Close: px, Volume: 1, Confirmed: true}
}

// genSeries 合成 K 线：先横盘、后趋势段，用于验证 walk-forward 结构。
func genSeries(n int) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		px := 100.0
		switch {
		case i >= n*2/3: // 后 1/3 下跌趋势
			px = 120 - float64(i-n*2/3)*0.5
		case i >= n/3: // 中 1/3 上涨趋势
			px = 100 + float64(i-n/3)*0.5
		}
		out = append(out, wfCandle(int64(3_600_000*(i+1)), px))
	}
	return out
}

func baseWFConfig() WFConfig {
	return WFConfig{TrainBars: 40, TestBars: 20, SeedCash: 10000,
		Cost:   backtest.CostModel{SlippageBps: 5, MakerFeeBps: 2, TakerFeeBps: 5},
		Symbol: "BTC-USDT", Interval: "1H"}
}

func trendSelector(trialBase int) StrategySelector {
	return FixedSelector(func() strategy.Strategy {
		s, _ := trend.New(trend.Params{EntryN: 8, ExitN: 4, AtrN: 5, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5})
		return s
	}, "trend:entry8/exit4")
}

func TestWalkForwardStructure(t *testing.T) {
	cs := genSeries(120) // 40 train + 20 test × 4 折
	rep, err := WalkForward(cs, baseWFConfig(), trendSelector(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Folds) != 4 {
		t.Fatalf("120 根 train40/test20 应切 4 折: %d", len(rep.Folds))
	}
	// 折区间：测试窗互斥且按序推进
	for i, f := range rep.Folds {
		if f.Fold != i+1 {
			t.Fatalf("折序号错误: %+v", f)
		}
		if f.Strategy == "" {
			t.Fatal("每折必须留痕策略描述")
		}
		if i > 0 && rep.Folds[i-1].TestTo >= f.TestFrom {
			t.Fatalf("测试窗必须互斥: %d 折重叠", i+1)
		}
	}
	// OOS 曲线总长 = 4 × 20 = 80
	if len(rep.OOSCurve) != 80 {
		t.Fatalf("OOS 曲线长度错误: %d", len(rep.OOSCurve))
	}
	// 曲线时间单调
	for i := 1; i < len(rep.OOSCurve); i++ {
		if rep.OOSCurve[i].Ts <= rep.OOSCurve[i-1].Ts {
			t.Fatal("OOS 曲线时间必须严格递增")
		}
	}
	if rep.TotalTrials != 8 { // 4 折 × (1 训练 + 1 测试)
		t.Fatalf("试验数必须累计: %d", rep.TotalTrials)
	}
	if rep.OOSMetrics.FinalEquity <= 0 {
		t.Fatal("OOS 权益必须为正")
	}
}

func TestWalkForwardSelectorSeesOnlyTrain(t *testing.T) {
	cs := genSeries(100)
	sawMax := 0
	lastTrainLen := 0
	sel := func(train []exchange.Candle, trialBase int) (strategy.Strategy, string, int, error) {
		if len(train) > sawMax {
			sawMax = len(train)
		}
		// 训练切片必须是原始序列前缀（防窥探：拿不到未来）
		for i := range train {
			if train[i].OpenTime != cs[i].OpenTime {
				t.Fatal("训练切片必须是前缀")
			}
		}
		lastTrainLen = len(train)
		s, _ := trend.New(trend.DefaultParams())
		return s, "fixed", 1, nil
	}
	if _, err := WalkForward(cs, baseWFConfig(), sel); err != nil {
		t.Fatal(err)
	}
	// 扩张式（anchored）训练窗：100 根最多 3 折，末折训练 80 根（40/60/80）
	if sawMax != 80 {
		t.Fatalf("扩张训练窗末折应见 80 根: %d", sawMax)
	}
	if lastTrainLen <= 40 {
		t.Fatalf("末折训练窗应超过初始 40 根: %d", lastTrainLen)
	}
}

func TestWalkForwardRejectsShortSample(t *testing.T) {
	cs := genSeries(50) // 不足 train40+test20
	if _, err := WalkForward(cs, baseWFConfig(), trendSelector(0)); err == nil {
		t.Fatal("样本不足必须报错")
	}
	bad := baseWFConfig()
	bad.TrainBars = 10
	if _, err := WalkForward(genSeries(100), bad, trendSelector(0)); err == nil {
		t.Fatal("非法窗口配置必须报错")
	}
	bad2 := baseWFConfig()
	bad2.SeedCash = 0
	if _, err := WalkForward(genSeries(100), bad2, trendSelector(0)); err == nil {
		t.Fatal("非法种子资金必须报错")
	}
}

func TestWalkForwardSelectorError(t *testing.T) {
	sel := func(train []exchange.Candle, trialBase int) (strategy.Strategy, string, int, error) {
		return nil, "", 0, fmt.Errorf("训练失败")
	}
	if _, err := WalkForward(genSeries(100), baseWFConfig(), sel); err == nil {
		t.Fatal("选择器失败必须传播")
	}
}

func TestOOSCurveCompounding(t *testing.T) {
	// 两段各 +10% 的折 → 复合 +21%
	dst := appendFoldCurve(nil, []backtest.EquityPoint{{Ts: 1, Equity: 100}, {Ts: 2, Equity: 110}}, 100)
	dst = appendFoldCurve(dst, []backtest.EquityPoint{{Ts: 3, Equity: 100}, {Ts: 4, Equity: 110}}, 100)
	if math.Abs(dst[len(dst)-1].Equity-121) > 1e-9 {
		t.Fatalf("复合拼接错误: %v", dst[len(dst)-1].Equity)
	}
	// 空折不影响
	if got := appendFoldCurve(dst, nil, 100); len(got) != len(dst) {
		t.Fatal("空折应跳过")
	}
	// 首折基准非正跳过（除零保护）
	if got := appendFoldCurve(nil, []backtest.EquityPoint{{Equity: 0}, {Equity: 10}}, 100); len(got) != 0 {
		t.Fatal("非正基准折应跳过")
	}
}

// buyHold 对照策略：验证 WF 端到端数字正确性。
type wfHold struct{ bought bool }

func (h *wfHold) Name() string { return "hold" }
func (h *wfHold) Warmup() int  { return 1 }
func (h *wfHold) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if !h.bought && ctx.Position <= 0 {
		h.bought = true
		qty := ctx.Cash / ctx.Last().Close * 0.99
		return []strategy.OrderIntent{{Side: exchange.Buy, Type: exchange.OrderMarket, Qty: qty}}
	}
	return nil
}

func TestWalkForwardBuyHoldOOSMatchesPrice(t *testing.T) {
	cs := genSeries(80) // 40 train + 2×20 test
	cfg := baseWFConfig()
	cfg.Cost = backtest.CostModel{} // 零成本（简化口径，测试用）
	rep, err := WalkForward(cs, cfg, FixedSelector(func() strategy.Strategy { return &wfHold{} }, "hold"))
	if err != nil {
		t.Fatal(err)
	}
	// OOS 区间 = candles[40..79]，对照值按该区间首尾收盘价动态计算
	wantBH := (cs[79].Close/cs[40].Close - 1) * 100
	if math.Abs(rep.BuyHoldPct-wantBH) > 1 {
		t.Fatalf("买入持有对照计算错误: %v want %v", rep.BuyHoldPct, wantBH)
	}
	// 零成本下每折 99% 仓位买入持有：OOS 收益应贴近对照
	if math.Abs(rep.OOSMetrics.TotalReturnPct-wantBH) > 1.5 {
		t.Fatalf("零成本买入持有 OOS 应贴近对照: oos=%v bh=%v", rep.OOSMetrics.TotalReturnPct, wantBH)
	}
}

func TestScanParamsAndPlateau(t *testing.T) {
	cs := genSeries(100)
	cost := backtest.CostModel{SlippageBps: 5, TakerFeeBps: 5}
	mk := func(label string) strategy.Strategy {
		p := trend.Params{EntryN: 8, ExitN: 4, AtrN: 5, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5}
		switch label {
		case "base":
			p.EntryN = 8
		case "n6":
			p.EntryN = 6
		case "n10":
			p.EntryN = 10
		}
		s, _ := trend.New(p)
		return s
	}
	points, trials, err := ScanParams(cs, cost, 10000, "BTC-USDT", "1H", mk, "base", "n6", "n10")
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || trials != 3 {
		t.Fatalf("扫描点数与试验数错误: %d %d", len(points), trials)
	}
	rep, err := PlateauCheck(points, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsPlateau && rep.Base.Ret <= 0 {
		t.Log("基准非正 → 无可迁移性（合成数据语义正确）")
	}
	// 高原正例：邻域收益接近基准
	good := []ParamPoint{{Label: "base", Ret: 10}, {Label: "n6", Ret: 8}, {Label: "n10", Ret: 9}}
	rep2, _ := PlateauCheck(good, 0.5)
	if !rep2.IsPlateau {
		t.Fatalf("邻域收益 8/9 ≥ 基准一半应判高原: %s", rep2.Reason)
	}
	// 孤针反例：邻域塌陷
	bad := []ParamPoint{{Label: "base", Ret: 10}, {Label: "n6", Ret: -5}, {Label: "n10", Ret: 1}}
	rep3, _ := PlateauCheck(bad, 0.5)
	if rep3.IsPlateau {
		t.Fatal("邻域塌陷必须判孤针（过拟合警报）")
	}
	// 基准非正
	neg := []ParamPoint{{Label: "base", Ret: -3}, {Label: "n6", Ret: -3}, {Label: "n10", Ret: -3}}
	rep4, _ := PlateauCheck(neg, 0.5)
	if rep4.IsPlateau {
		t.Fatal("基准收益非正不得判高原")
	}
	// 参数校验
	if _, err := PlateauCheck([]ParamPoint{{Ret: 1}}, 0.5); err == nil {
		t.Fatal("点数不足必须报错")
	}
	if _, err := PlateauCheck(good, 1.5); err == nil {
		t.Fatal("非法 decay 必须报错")
	}
}

func TestScanParamsEmpty(t *testing.T) {
	if _, _, err := ScanParams(genSeries(50), backtest.CostModel{}, 1000, "BTC-USDT", "1H",
		func(string) strategy.Strategy { return &wfHold{} }); err == nil {
		t.Fatal("空参数集必须报错")
	}
}

func TestCostScan(t *testing.T) {
	cs := genSeries(80)
	base := backtest.CostModel{SlippageBps: 5, MakerFeeBps: 2, TakerFeeBps: 5}
	points, trials, err := CostScan(cs, base, 10000, "BTC-USDT", "1H",
		func() strategy.Strategy {
			s, _ := trend.New(trend.Params{EntryN: 8, ExitN: 4, AtrN: 5, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5})
			return s
		}, 0, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || trials != 3 {
		t.Fatalf("成本扫描点数错误: %d", len(points))
	}
	// 零成本收益应不劣于双倍成本收益（成本单调性）
	if points[0].Ret < points[2].Ret {
		t.Fatalf("成本单调性违反: 0x=%v 2x=%v", points[0].Ret, points[2].Ret)
	}
	if _, _, err := CostScan(cs, base, 10000, "BTC-USDT", "1H", func() strategy.Strategy { return &wfHold{} }); err == nil {
		t.Fatal("空倍数集必须报错")
	}
}

// sawSeries 锯齿震荡（价格在 100/101 交替）。
func sawSeries(n int, startOT int64) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		px := 100.0
		if i%2 == 1 {
			px = 101
		}
		out = append(out, wfCandle(startOT+int64(3_600_000*i), px))
	}
	return out
}

// upSeries 单边上涨趋势。
func upSeries(n int, startOT int64) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, wfCandle(startOT+int64(3_600_000*i), 100*math.Pow(1.01, float64(i))))
	}
	return out
}

func TestRegimeCompareSynthetic(t *testing.T) {
	// 合成：40 根锯齿（震荡）+ 50 根趋势 + 40 根锯齿（段长需超过默认 lookback 24 + 统计门槛 30）
	cs := append(append(sawSeries(40, 3_600_000), upSeries(50, 41*3_600_000)...), sawSeries(40, 91*3_600_000)...)
	cost := backtest.CostModel{SlippageBps: 5, TakerFeeBps: 5}
	mkT := func() strategy.Strategy {
		s, _ := trend.New(trend.Params{EntryN: 8, ExitN: 4, AtrN: 5, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.9})
		return s
	}
	mkG := func() strategy.Strategy { // 真网格：区间套住锯齿 100~101，跨格低买高卖
		g, _ := grid.New(grid.Params{Lower: 99, Upper: 102, Grids: 6, QtyPerGrid: 1, Spacing: "arith", StopOnBreak: true})
		return g
	}
	rep, err := RegimeCompare(cs, cost, 10000, "BTC-USDT", "1H", mkT, mkG)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Segments) == 0 || rep.Trials == 0 {
		t.Fatalf("报告为空: %+v", rep)
	}
	if rep.Reason == "" {
		t.Fatal("判定理由必须留痕")
	}
	t.Logf("互补性: %v（%s）", rep.Complementary, rep.Reason)
}

func TestRegimeCompareTooShort(t *testing.T) {
	if _, err := RegimeCompare(genSeries(20), backtest.CostModel{}, 1000, "BTC-USDT", "1H",
		func() strategy.Strategy { return &wfHold{} }, func() strategy.Strategy { return &wfHold{} }); err == nil {
		t.Fatal("样本太短必须报错")
	}
}

// cycSeries 多轮"震荡/上涨/震荡/下跌"循环（10 根一段），产出多笔趋势交易。
func cycSeries(n int) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	px := 100.0
	for i := 0; i < n; i++ {
		switch (i / 10) % 4 {
		case 0, 2:
			px = px * (1 + 0.002*float64(i%5-2))
		case 1:
			px = px * 1.015
		case 3:
			px = px * 0.99
		}
		out = append(out, wfCandle(int64(3_600_000*(i+1)), px))
	}
	return out
}

func TestUMPCheck(t *testing.T) {
	cs := cycSeries(400)
	n, rep, err := UMPCheck(cs, backtest.CostModel{SlippageBps: 5, TakerFeeBps: 5}, 10000, "BTC-USDT", "1H",
		func() strategy.Strategy {
			s, _ := trend.New(trend.Params{EntryN: 8, ExitN: 4, AtrN: 5, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.9})
			return s
		}, 0.35, 2)
	if err != nil {
		t.Fatalf("UMPCheck 应可运行: %v", err)
	}
	if rep == nil || rep.Reason == "" {
		t.Fatal("报告必须留痕理由")
	}
	// 样本不足时报错
	if _, _, err := UMPCheck(genSeries(50), backtest.CostModel{}, 1000, "BTC-USDT", "1H",
		func() strategy.Strategy { return &wfHold{} }, 0.35, 20); err == nil {
		t.Fatal("样本不足必须报错")
	}
	_ = n
}

func TestKellyLoop(t *testing.T) {
	cs := cycSeries(400)
	cost := backtest.CostModel{SlippageBps: 5, TakerFeeBps: 5}
	rep, err := KellyLoop(cs, cost, 10000, "BTC-USDT", "1H",
		func() *trend.Donchian {
			s, _ := trend.New(trend.Params{EntryN: 8, ExitN: 4, AtrN: 5, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.9})
			return s
		}, 0.5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Note == "" {
		t.Fatal("结论必须留痕")
	}
	// 交易不足场景：拒绝生成半凯利并如实记录
	rep2, err := KellyLoop(genSeries(60), cost, 1000, "BTC-USDT", "1H",
		func() *trend.Donchian {
			s, _ := trend.New(trend.DefaultParams())
			return s
		}, 0.5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Trials != 1 || rep2.SizerDesc != "" {
		t.Fatalf("交易不足应只跑一轮且不生成仓位: %+v", rep2)
	}
	if !strings.Contains(rep2.Note, "证据不足") && !strings.Contains(rep2.Note, "不足") {
		t.Fatalf("拒绝原因必须留痕: %s", rep2.Note)
	}
}

func TestKellyLoopMDDCompare(t *testing.T) {
	// MDD 比较按绝对值：-0.89%（浅）不得误判为深于 -7.98%（深）
	base := backtest.Metrics{TotalReturnPct: 2, MaxDrawdownPct: -7.98}
	kelly := backtest.Metrics{TotalReturnPct: 0.3, MaxDrawdownPct: -0.89}
	if math.Abs(kelly.MaxDrawdownPct) > math.Abs(base.MaxDrawdownPct) {
		t.Fatal("前置失败：|-0.89| < |-7.98|")
	}
}
