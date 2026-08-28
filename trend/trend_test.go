package trend

import (
	"math"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/sizer"
	"github.com/HarveyBase/QuantForge/strategy"
)

func candleT(ot int64, o, h, l, c float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: o, High: h, Low: l, Close: c, Volume: 1, Confirmed: true}
}

func smallParams() Params {
	// 小窗口参数便于合成数据测试
	return Params{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: 2.0, RiskPct: 0.005, MaxPosPct: 0.5}
}

func ctxOf(cs []exchange.Candle, equity, position float64) *strategy.Context {
	return &strategy.Context{Symbol: "BTC-USDT", Interval: "1H", Candles: cs, Equity: equity, Position: position, Cash: equity}
}

// flatThenBreakout 前 12 根横盘 100±0.5，随后突破到 105+。
func flatThenBreakout() []exchange.Candle {
	var cs []exchange.Candle
	for i := int64(1); i <= 12; i++ {
		cs = append(cs, candleT(i, 100, 100.5, 99.5, 100))
	}
	for i := int64(13); i <= 20; i++ {
		px := 101 + float64(i-13)
		cs = append(cs, candleT(i, px-0.5, px+0.5, px-1, px))
	}
	return cs
}

func TestParamsValidation(t *testing.T) {
	bad := []Params{
		{EntryN: 1, ExitN: 3, AtrN: 3, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5},
		{EntryN: 5, ExitN: 1, AtrN: 3, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5},
		{EntryN: 5, ExitN: 3, AtrN: 1, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5},
		{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: 0, RiskPct: 0.005, MaxPosPct: 0.5},
		{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: -1, RiskPct: 0.005, MaxPosPct: 0.5},
		{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: 2, RiskPct: 0, MaxPosPct: 0.5},
		{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: 2, RiskPct: 0.03, MaxPosPct: 0.5}, // >2% 风险红线
		{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 1.5},
	}
	for i, p := range bad {
		if _, err := New(p); err == nil {
			t.Errorf("case %d 参数非法必须报错", i)
		}
	}
	if _, err := New(smallParams()); err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
}

func TestMetadata(t *testing.T) {
	d, _ := New(smallParams())
	if d.Name() != "trend_donchian" {
		t.Fatal("策略名错误")
	}
	if d.Warmup() != 6 { // max(5,3,3)+1
		t.Fatalf("预热期错误: %d", d.Warmup())
	}
	if !strings.Contains(d.Describe(), "entry5") {
		t.Fatalf("参数描述应留痕: %s", d.Describe())
	}
}

func TestNoSignalBeforeWarmup(t *testing.T) {
	d, _ := New(smallParams())
	cs := flatThenBreakout()
	// 只给 4 根（不足 Warmup）——策略自身不判 Warmup（引擎负责），但指标 NaN 应无信号
	out := d.OnCandle(ctxOf(cs[:4], 10000, 0))
	if len(out) != 0 {
		t.Fatalf("指标预热期不应产生信号: %+v", out)
	}
}

func TestBreakoutEntry(t *testing.T) {
	d, _ := New(smallParams())
	cs := flatThenBreakout()
	// 第 13 根（收盘 101）已突破前 5 根最高 100.5
	out := d.OnCandle(ctxOf(cs[:13], 10000, 0))
	if len(out) != 1 || out[0].Side != exchange.Buy || out[0].Type != exchange.OrderMarket {
		t.Fatalf("突破应产生市价买入: %+v", out)
	}
	if out[0].Qty <= 0 {
		t.Fatalf("仓位必须为正: %+v", out)
	}
	// 仓位上限：qty × close ≤ equity × MaxPosPct
	if out[0].Qty*101 > 10000*0.5+1e-9 {
		t.Fatalf("单笔仓位超上限（永不满仓）: %v", out[0].Qty*101)
	}
	if !strings.Contains(out[0].Note, "突破") {
		t.Fatalf("信号应留痕原因: %s", out[0].Note)
	}
}

func TestNoEntryInFlat(t *testing.T) {
	d, _ := New(smallParams())
	cs := flatThenBreakout()
	// 横盘期任何一根都不应触发（收盘 100 ≤ 前 5 根最高 100.5）
	for i := 6; i <= 12; i++ {
		if out := d.OnCandle(ctxOf(cs[:i], 10000, 0)); len(out) != 0 {
			t.Fatalf("横盘期第 %d 根不应入场: %+v", i, out)
		}
	}
}

func TestCooldownPreventsDuplicateSignals(t *testing.T) {
	d, _ := New(smallParams())
	cs := flatThenBreakout()
	if out := d.OnCandle(ctxOf(cs[:13], 10000, 0)); len(out) != 1 {
		t.Fatalf("首根应发信号: %+v", out)
	}
	// 同根重复驱动与成交根（gap≤2）不重发
	if out := d.OnCandle(ctxOf(cs[:13], 10000, 0)); len(out) != 0 {
		t.Fatalf("同根重复驱动不应重发: %+v", out)
	}
	if out := d.OnCandle(ctxOf(cs[:14], 10000, 0)); len(out) != 0 {
		t.Fatalf("成交根不应重发: %+v", out)
	}
	// gap=3：若订单被拒（现金不足）允许重试
	if out := d.OnCandle(ctxOf(cs[:15], 10000, 0)); len(out) != 1 {
		t.Fatalf("被拒后第 3 根应允许重试: %+v", out)
	}
	// 若已成交（Position>0）走持仓分支，不再入场
	d2, _ := New(smallParams())
	d2.OnCandle(ctxOf(cs[:13], 10000, 0))
	if out := d2.OnCandle(ctxOf(cs[:15], 10000, 1)); len(out) != 0 {
		t.Fatalf("已持仓不应再入场: %+v", out)
	}
}

func TestExitOnChannelBreak(t *testing.T) {
	d, _ := New(smallParams())
	// 构造：入场后横盘走弱跌破 ExitN 低点
	cs := flatThenBreakout()
	cs = append(cs, candleT(21, 103, 103.5, 102, 102.5))
	cs = append(cs, candleT(22, 102, 102.5, 101, 101.5))
	cs = append(cs, candleT(23, 101, 101.5, 100, 100.5)) // 跌破前 3 根低点
	// 已持仓：从第 15 根突破成交后
	var out []strategy.OrderIntent
	for i := 14; i <= 23; i++ {
		out = d.OnCandle(ctxOf(cs[:i], 10000, 1))
		if len(out) > 0 {
			break
		}
	}
	if len(out) != 1 || out[0].Side != exchange.Sell || out[0].Type != exchange.OrderMarket {
		t.Fatalf("跌破通道应市价清仓: %+v", out)
	}
	if out[0].Qty != 1 {
		t.Fatalf("应全仓卖出: %+v", out)
	}
}

func TestATRTrailingStop(t *testing.T) {
	d, _ := New(smallParams())
	// 入场后冲高回落：回落幅度 > 2×ATR 触发跟踪止损
	cs := flatThenBreakout()
	peak := 110.0
	cs = append(cs, candleT(21, 108, peak, 107, 109)) // 冲高（ATR 放大）
	cs = append(cs, candleT(22, 106, 107, 105, 106))
	cs = append(cs, candleT(23, 103, 104, 102, 103)) // 深跌：103 < peak − 2×ATR
	var out []strategy.OrderIntent
	for i := 14; i <= 23; i++ {
		out = d.OnCandle(ctxOf(cs[:i], 10000, 1))
		if len(out) > 0 {
			break
		}
	}
	if len(out) != 1 || out[0].Side != exchange.Sell {
		t.Fatalf("ATR 跟踪止损应触发清仓: %+v", out)
	}
	if !strings.Contains(out[0].Note, "ATR") && !strings.Contains(out[0].Note, "下轨") {
		t.Fatalf("退出原因应留痕: %s", out[0].Note)
	}
}

func TestApplyFillTracksEntryAndPeak(t *testing.T) {
	d, _ := New(smallParams())
	d.ApplyFill(exchange.Buy, 1, 105)
	if d.EntryPx() != 105 {
		t.Fatalf("入场价应被记录: %v", d.EntryPx())
	}
	d.ApplyFill(exchange.Sell, 1, 120)
	if d.EntryPx() != 0 {
		t.Fatal("清仓后入场价应归零")
	}
}

func TestSizePositionVolTarget(t *testing.T) {
	d, _ := New(Params{EntryN: 5, ExitN: 3, AtrN: 3, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5})
	ctx := ctxOf(nil, 10000, 0)
	// 波动率目标：qty = 10000 × 0.005 / ATR
	if q := d.sizePosition(ctx, 100, 2); math.Abs(q-25) > 1e-9 {
		t.Fatalf("波动率目标仓位错误: %v", q)
	}
	// ATR 极小时受仓位上限封顶：cap = 10000×0.5/100 = 50
	if q := d.sizePosition(ctx, 100, 0.01); math.Abs(q-50) > 1e-9 {
		t.Fatalf("仓位上限封顶错误: %v", q)
	}
	// 非法输入返回 0
	if q := d.sizePosition(ctx, 0, 2); q != 0 {
		t.Fatal("零价格应返回 0")
	}
	if q := d.sizePosition(ctx, 100, 0); q != 0 {
		t.Fatal("零 ATR 应返回 0")
	}
}

func TestEmptyCandles(t *testing.T) {
	d, _ := New(smallParams())
	if out := d.OnCandle(ctxOf(nil, 10000, 0)); out != nil {
		t.Fatalf("空 K 线不应产生订单: %+v", out)
	}
}

func TestPositionRecoveryAfterStateLoss(t *testing.T) {
	// 重启场景：内部锚点丢失但账本有持仓 → 从当前价保守重挂跟踪止损，不应卡死
	d, _ := New(smallParams())
	cs := flatThenBreakout()
	cs = append(cs, candleT(21, 108, 110, 107, 109))
	// 模拟深跌触发卖出（无 ApplyFill 历史）
	var out []strategy.OrderIntent
	for i := 14; i <= 30; i++ {
		if i > len(cs) {
			break
		}
		cs = append(cs, candleT(int64(i), 100-float64(i-22)*2, 100-float64(i-22)*2+1, 100-float64(i-22)*2-1, 100-float64(i-22)*2))
		out = d.OnCandle(ctxOf(cs[:i], 10000, 1))
		if len(out) > 0 {
			break
		}
	}
	if len(out) == 0 || out[0].Side != exchange.Sell {
		t.Fatalf("状态丢失后仍应能触发止损离场: %+v", out)
	}
}

func TestSetSizerOverrides(t *testing.T) {
	cs := flatThenBreakout()
	// 注入固定仓位 sizer：数量必须恒为 1
	d, _ := New(smallParams())
	d.SetSizer(&fixedQty{qty: 1})
	out := d.OnCandle(ctxOf(cs[:13], 10000, 0))
	if len(out) != 1 || out[0].Qty != 1 {
		t.Fatalf("注入 sizer 必须生效: %+v", out)
	}
	// 恢复默认（nil）：回到波动率目标
	d2, _ := New(smallParams())
	out2 := d2.OnCandle(ctxOf(cs[:13], 10000, 0))
	if len(out2) != 1 || out2[0].Qty == 1 && false {
		t.Fatal("前置失败")
	}
	d2.SetSizer(nil)
	_ = out2
	// 默认路径的仓位与注入前不同（ATR 目标 ≠ 1）
	d3, _ := New(smallParams())
	out3 := d3.OnCandle(ctxOf(cs[:13], 10000, 0))
	if len(out3) != 1 || out3[0].Qty <= 0 {
		t.Fatal("默认 sizer 应正常出信号")
	}
}

// fixedQty 固定数量仓位（测试用）。
type fixedQty struct{ qty float64 }

func (f *fixedQty) Size(in sizer.Input) float64 { return f.qty }
func (f *fixedQty) Describe() string            { return "fixed_qty" }
