package sizer

import (
	"math"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/backtest"
)

func TestAtrVolTarget(t *testing.T) {
	a := NewAtrVolTarget(0.005, 0.5)
	// 波动率目标：qty = 10000×0.005/2 = 25
	if q := a.Size(Input{Equity: 10000, Price: 100, Cash: 1e9, ATR: 2}); math.Abs(q-25) > 1e-9 {
		t.Fatalf("波动率目标仓位错误: %v", q)
	}
	// ATR 极小 → 名义上限封顶：cap = 10000×0.5/100 = 50
	if q := a.Size(Input{Equity: 10000, Price: 100, Cash: 1e9, ATR: 0.01}); math.Abs(q-50) > 1e-9 {
		t.Fatalf("上限封顶错误: %v", q)
	}
	// 现金约束
	if q := a.Size(Input{Equity: 10000, Price: 100, Cash: 1000, ATR: 2}); math.Abs(q-10) > 1e-9 {
		t.Fatalf("现金约束错误: %v", q)
	}
	// 非法输入
	for _, in := range []Input{{}, {Equity: 1, Price: 100, ATR: 1}, {Equity: 1, Price: 0, ATR: 1}, {Equity: 1, Price: 1, ATR: 0}} {
		if q := a.Size(in); q != 0 {
			t.Fatalf("非法输入应返回 0: %+v → %v", in, q)
		}
	}
	// 参数回退
	a2 := NewAtrVolTarget(-1, 2) // 非法 → 默认
	if a2.RiskPct != 0.005 || a2.MaxPosPct != 0.5 {
		t.Fatalf("非法参数应回退默认: %+v", a2)
	}
	if !strings.Contains(a.Describe(), "atr_vol_target") {
		t.Fatal("描述必须留痕")
	}
}

func TestHalfKellyKnownVector(t *testing.T) {
	// 经典向量：W=0.6, b=avgWin/avgLoss=8/4=2 → f* = 0.6 − 0.4/2 = 0.4
	h := &HalfKelly{WinRate: 0.6, AvgWin: 0.08, AvgLoss: 0.04, Fraction: 0.5, MaxPosPct: 1}
	if f := h.KellyFrac(); math.Abs(f-0.4) > 1e-9 {
		t.Fatalf("凯利公式错误: %v", f)
	}
	// 半凯利仓位：权益 10000 × 0.4×0.5 / 价格 100 = 20 个
	if q := h.Size(Input{Equity: 10000, Price: 100, Cash: 1e9}); math.Abs(q-20) > 1e-9 {
		t.Fatalf("半凯利仓位错误: %v", q)
	}
	// 负期望：W=0.3, b=1 → f=0.3−0.7=−0.4 → 不下注
	h2 := &HalfKelly{WinRate: 0.3, AvgWin: 0.04, AvgLoss: 0.04, Fraction: 0.5, MaxPosPct: 1}
	if f := h2.KellyFrac(); f != 0 {
		t.Fatalf("负期望必须不下注: %v", f)
	}
	if q := h2.Size(Input{Equity: 10000, Price: 100, Cash: 1e9}); q != 0 {
		t.Fatal("负期望仓位必须为 0")
	}
	// 证据退化（零输入）
	h3 := &HalfKelly{Fraction: 0.5, MaxPosPct: 1}
	if h3.KellyFrac() != 0 {
		t.Fatal("零统计不下注")
	}
}

func TestHalfKellyCaps(t *testing.T) {
	// 高优势：W=0.9 b=4 → f*=0.925 → 全额已 >MaxPosPct
	h := &HalfKelly{WinRate: 0.9, AvgWin: 0.08, AvgLoss: 0.02, Fraction: 1, MaxPosPct: 0.3}
	if q := h.Size(Input{Equity: 10000, Price: 100, Cash: 1e9}); math.Abs(q-30) > 1e-9 {
		t.Fatalf("MaxPosPct 封顶错误: %v", q)
	}
	// 现金约束
	h2 := &HalfKelly{WinRate: 0.6, AvgWin: 0.08, AvgLoss: 0.04, Fraction: 0.5, MaxPosPct: 1}
	if q := h2.Size(Input{Equity: 10000, Price: 100, Cash: 500}); math.Abs(q-5) > 1e-9 {
		t.Fatalf("现金约束错误: %v", q)
	}
	// KellyFrac 封顶 1
	h3 := &HalfKelly{WinRate: 1.0, AvgWin: 0.1, AvgLoss: 0.01, Fraction: 1, MaxPosPct: 1}
	if f := h3.KellyFrac(); f != 1 {
		t.Fatalf("f* 封顶 1: %v", f)
	}
	if !strings.Contains(h.Describe(), "half_kelly") {
		t.Fatal("描述必须留痕")
	}
}

func TestFromMetrics(t *testing.T) {
	m := backtest.Metrics{TradeCount: 50, WinRate: 60, AvgWinPct: 8, AvgLossPct: 4}
	h, err := FromMetrics(m, 0.5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if h.WinRate != 0.6 || h.AvgWin != 0.08 || h.AvgLoss != 0.04 || h.Fraction != 0.5 {
		t.Fatalf("统计映射错误: %+v", h)
	}
	if f := h.KellyFrac(); math.Abs(f-0.4) > 1e-9 {
		t.Fatalf("从指标计算的凯利错误: %v", f)
	}
	// 交易数不足拒绝
	if _, err := FromMetrics(backtest.Metrics{TradeCount: 19}, 0.5, 0.5); err == nil {
		t.Fatal("证据不足必须拒绝（防回测噪音上凯利）")
	}
	// 无亏损样本拒绝
	if _, err := FromMetrics(backtest.Metrics{TradeCount: 50, WinRate: 100, AvgWinPct: 8, AvgLossPct: 0}, 0.5, 0.5); err == nil {
		t.Fatal("无亏损样本必须拒绝（盈亏比不可估）")
	}
	// fraction/maxPosPct 回退默认
	h2, _ := FromMetrics(m, -1, 2)
	if h2.Fraction != 0.5 || h2.MaxPosPct != 0.5 {
		t.Fatalf("非法参数回退默认: %+v", h2)
	}
}
