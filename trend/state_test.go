// state_test.go 趋势策略运行态持久化测试：
// 往返等价（恢复后对相同后续 K 线序列产出与"从未重启"一致的信号）、
// 配置漂移拒绝导入、版本不兼容拒绝、非法锚点拒绝。
package trend

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

// mkCtx 构造策略上下文：n 根收盘 K 线（priceSeq 依次为收盘价），持仓 pos。
func mkCtx(priceSeq []float64, pos float64) *strategy.Context {
	candles := make([]exchange.Candle, len(priceSeq))
	for i, px := range priceSeq {
		px := px
		candles[i] = exchange.Candle{
			OpenTime: int64(i) * 60_000, Close: px,
			Open: px * 0.999, High: px * 1.001, Low: px * 0.998,
		}
	}
	return &strategy.Context{Candles: candles, Position: pos, Equity: 10000}
}

// runUntilSignal 推进策略直到产出意图或 K 线耗尽（返回最后一批意图）。
func runUntilSignal(t *testing.T, d *Donchian, seq []float64, pos float64) []strategy.OrderIntent {
	t.Helper()
	var out []strategy.OrderIntent
	for i := 30; i <= len(seq); i++ {
		out = d.OnCandle(mkCtx(seq[:i], pos))
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// 往返等价：同一 K 线序列上，"运行→导出→新实例导入"后对相同后续序列产出一致信号。
func TestDonchianStateRoundTripEquivalence(t *testing.T) {
	p := DefaultParams()
	p.EntryN, p.ExitN, p.AtrN = 5, 3, 5 // 缩短窗口便于测试构造突破

	// 持仓态：人为设置入场锚点（模拟持仓中），导出
	live, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	live.entryPx, live.peakClose, live.signalBar = 100, 105, 42
	raw, err := live.ExportState()
	if err != nil {
		t.Fatal(err)
	}

	restored, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.ImportState(raw); err != nil {
		t.Fatal(err)
	}
	// 锚点一致
	if restored.entryPx != 100 || restored.peakClose != 105 || restored.signalBar != 42 {
		t.Fatalf("恢复后运行态不一致: entry=%v peak=%v sig=%v", restored.entryPx, restored.peakClose, restored.signalBar)
	}

	// 后续序列行为等价：平稳下跌序列触发跟踪止损离场信号（长度 > runUntilSignal 起点 30）
	drop := make([]float64, 40)
	for i := range drop {
		drop[i] = 105 - 0.45*float64(i)
	}
	a := runUntilSignal(t, live, drop, 1)
	b := runUntilSignal(t, restored, drop, 1)
	if len(a) != len(b) {
		t.Fatalf("恢复实例信号数不一致: live=%d restored=%d", len(a), len(b))
	}
	for i := range a {
		if a[i].Side != b[i].Side || a[i].Qty != b[i].Qty || a[i].Kind != b[i].Kind {
			t.Fatalf("信号 %d 不一致: %+v vs %+v", i, a[i], b[i])
		}
	}
	if len(a) == 0 {
		t.Fatal("两实例都未出信号，等价测试无效（需调整序列构造）")
	}
}

// 配置漂移：参数变化后拒绝导入（防运行态与新配置语义错位）。
func TestDonchianStateRejectsParamDrift(t *testing.T) {
	p := DefaultParams()
	d, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := d.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	p2 := p
	p2.AtrMult = 3.0
	d2, err := New(p2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.ImportState(raw); err == nil {
		t.Fatal("参数漂移必须拒绝导入")
	} else if !strings.Contains(err.Error(), "atr_mult") {
		t.Fatalf("漂移错误应指明字段: %v", err)
	}
}

// 版本不兼容：伪造 version=99 的状态拒绝导入。
func TestDonchianStateRejectsUnknownVersion(t *testing.T) {
	d, err := New(DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"version": 99})
	if err := d.ImportState(raw); err == nil {
		t.Fatal("未知版本必须拒绝导入（防误解旧格式）")
	}
}

// 非法锚点（负入场价）拒绝导入。
func TestDonchianStateRejectsNegativeAnchor(t *testing.T) {
	d, err := New(DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"version": 1, "entry_px": -1, "peak_close": 0, "signal_bar": -1,
		"config": DefaultParams(),
	})
	if err := d.ImportState(raw); err == nil {
		t.Fatal("负锚点必须拒绝导入")
	}
}
