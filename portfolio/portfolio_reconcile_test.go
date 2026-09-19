package portfolio

import (
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func findDiff(rep ReconcileReport, kind string) *ReconcileDiff {
	for i := range rep.Diffs {
		if rep.Diffs[i].Kind == kind {
			return &rep.Diffs[i]
		}
	}
	return nil
}

// 全部一致：Ok=true 且无差异。
func TestReconcileDetailAllConsistent(t *testing.T) {
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100})
	rep := p.ReconcileDetail([]exchange.Balance{
		{Asset: "USDT", Total: 900.5, Available: 900.5}, // 差 0.5 < 0.9005（0.1% 相对容差）
		{Asset: "BTC", Total: 1, Available: 1},
	})
	if !rep.Ok || len(rep.Diffs) != 0 {
		t.Fatalf("容差内应判定一致: %+v", rep)
	}
	if rep.Ts.IsZero() {
		t.Fatal("报告必须带时间戳")
	}
}

// 现金差异：Kind=cash，携带 Local/Remote/Diff。
func TestReconcileDetailCashDiff(t *testing.T) {
	p := New(500)
	rep := p.ReconcileDetail([]exchange.Balance{{Asset: "USDT", Total: 600, Available: 600}})
	if rep.Ok || len(rep.Diffs) != 1 {
		t.Fatalf("现金差异必须报告: %+v", rep)
	}
	d := rep.Diffs[0]
	if d.Kind != "cash" || d.Item != "USDT" || d.Local != 500 || d.Remote != 600 || d.Diff != 100 {
		t.Fatalf("现金差异字段错误: %+v", d)
	}
}

// 持仓与可用差异：qty/available 双比对。
func TestReconcileDetailPositionAndAvailableDiff(t *testing.T) {
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100})
	rep := p.ReconcileDetail([]exchange.Balance{
		{Asset: "USDT", Total: 900.5, Available: 900.5}, // 容差内
		{Asset: "BTC", Total: 1.2, Available: 0.7},      // qty 差 0.2、available 差 0.3
	})
	if rep.Ok {
		t.Fatalf("超容差必须判为不一致: %+v", rep)
	}
	if len(rep.Diffs) != 2 {
		t.Fatalf("应有且仅有持仓/可用两条差异: %+v", rep)
	}
	if d := findDiff(rep, DiffPosition); d == nil || d.Item != "BTC-USDT" || d.Local != 1 || d.Remote != 1.2 || !approx(d.Diff, 0.2) {
		t.Fatalf("持仓差异字段错误: %+v", d)
	}
	if d := findDiff(rep, DiffAvailable); d == nil || d.Local != 1 || d.Remote != 0.7 || !approx(d.Diff, -0.3) {
		t.Fatalf("可用差异字段错误: %+v", d)
	}
}

// 本地无持仓的 Base 币：远程余额超容差也要报差异。
func TestReconcileDetailUnknownRemoteBase(t *testing.T) {
	p := New(500)
	rep := p.ReconcileDetail([]exchange.Balance{
		{Asset: "USDT", Total: 500.05, Available: 500.05}, // 容差内
		{Asset: "ETH", Total: 2, Available: 2},            // 本地无 ETH 持仓
	})
	if rep.Ok || len(rep.Diffs) != 1 {
		t.Fatalf("远程多出的 Base 币必须报告: %+v", rep)
	}
	d := rep.Diffs[0]
	if d.Kind != DiffPosition || d.Item != "ETH" || d.Local != 0 || d.Remote != 2 || d.Diff != 2 {
		t.Fatalf("差异字段错误: %+v", d)
	}
}

// 本地有持仓但远程无该 Base 币余额：按远程 0 报差异（qty 与 available 各一条）。
func TestReconcileDetailLocalPositionMissingRemote(t *testing.T) {
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100})
	rep := p.ReconcileDetail([]exchange.Balance{{Asset: "USDT", Total: 900, Available: 900}})
	if rep.Ok {
		t.Fatalf("远程缺 Base 币余额必须判为不一致: %+v", rep)
	}
	if d := findDiff(rep, DiffPosition); d == nil || d.Local != 1 || d.Remote != 0 || d.Diff != -1 {
		t.Fatalf("持仓差异字段错误: %+v", d)
	}
	if d := findDiff(rep, DiffAvailable); d == nil || d.Local != 1 || d.Remote != 0 {
		t.Fatalf("可用差异字段错误: %+v", d)
	}
}

// 容差边界：相对 0.1% 与绝对 1e-6 两条线的内外侧。
func TestReconcileDetailToleranceBoundary(t *testing.T) {
	p := New(1000)
	// 1000 vs 1001：差 1 ≤ 1001*0.1%=1.001，容差内
	if rep := p.ReconcileDetail([]exchange.Balance{{Asset: "USDT", Total: 1001, Available: 1001}}); !rep.Ok {
		t.Fatalf("相对容差边界内应一致: %+v", rep)
	}
	// 1000 vs 1001.2：差 1.2 > 1.0012，报差异
	if rep := p.ReconcileDetail([]exchange.Balance{{Asset: "USDT", Total: 1001.2, Available: 1001.2}}); rep.Ok || findDiff(rep, DiffCash) == nil {
		t.Fatalf("相对容差边界外必须报告: %+v", rep)
	}
	// 小额绝对容差：1e-7 vs 远程缺失(0) ≤ 1e-6，一致
	p2 := New(1e-7)
	if rep := p2.ReconcileDetail(nil); !rep.Ok {
		t.Fatalf("绝对容差内应一致: %+v", rep)
	}
	// 超绝对容差：1e-5 vs 0，报差异
	p3 := New(1e-5)
	if rep := p3.ReconcileDetail(nil); rep.Ok || findDiff(rep, DiffCash) == nil {
		t.Fatalf("超绝对容差必须报告: %+v", rep)
	}
}

// Seed 登记的币种参与对账映射（quote 计价现金、base 持仓）。
func TestReconcileDetailUsesSeededCurrencies(t *testing.T) {
	p := New(0)
	p.Seed([]exchange.Balance{
		{Asset: "USDT", Total: 1000, Available: 1000},
		{Asset: "BTC", Total: 1, Available: 1},
	}, "BTC-USDT", "BTC", "USDT", 0)
	rep := p.ReconcileDetail([]exchange.Balance{
		{Asset: "USDT", Total: 1000, Available: 1000},
		{Asset: "BTC", Total: 1, Available: 1},
	})
	if !rep.Ok {
		t.Fatalf("Seed 后账目一致应通过: %+v", rep)
	}
	// 现金漂移按登记 quote 币报告
	rep2 := p.ReconcileDetail([]exchange.Balance{{Asset: "BTC", Total: 1, Available: 1}})
	if d := findDiff(rep2, DiffCash); d == nil || d.Item != "USDT" || d.Remote != 0 {
		t.Fatalf("现金差异应按登记 quote 币报告: %+v", d)
	}
}
