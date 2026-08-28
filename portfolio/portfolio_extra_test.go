package portfolio

import (
	"sync"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestSeedFromBalances(t *testing.T) {
	p := New(0)
	p.Seed([]exchange.Balance{
		{Asset: "USDT", Total: 5000, Available: 4000},
		{Asset: "BTC", Total: 0.2, Available: 0.2},
	}, "BTC-USDT", "BTC", "USDT", 50000)
	if p.Cash != 4000 {
		t.Fatalf("现金应取可用余额: %v", p.Cash)
	}
	pos := p.Positions["BTC-USDT"]
	if pos == nil || pos.Qty != 0.2 || pos.Available != 0.2 || pos.AvgPrice != 50000 {
		t.Fatalf("持仓应从余额初始化: %+v", pos)
	}
	if p.Mark("BTC") != 50000 {
		t.Fatalf("标记价应写入 base 资产: %v", p.Mark("BTC"))
	}
	// 空余额集合不改动
	p2 := New(123)
	p2.Seed(nil, "BTC-USDT", "BTC", "USDT", 0)
	if p2.Cash != 123 || len(p2.Positions) != 0 {
		t.Fatal("空余额不应改动账本")
	}
}

func TestFreezeBuyInsufficientCash(t *testing.T) {
	p := New(100)
	if p.Freeze(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 50, Qty: 3, ClientOrderID: "f1"}) {
		t.Fatal("现金不足冻结必须失败")
	}
	if p.Cash != 100 {
		t.Fatalf("失败的冻结不得改动账本: %v", p.Cash)
	}
}

func TestFreezeSellInsufficientPosition(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	if p.Freeze(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Price: 110, Qty: 2, ClientOrderID: "f2"}) {
		t.Fatal("卖出超持仓冻结必须失败")
	}
	if p.Freeze(exchange.OrderRequest{Symbol: "ETH-USDT", Side: exchange.Sell, Price: 110, Qty: 0.1, ClientOrderID: "f3"}) {
		t.Fatal("无持仓冻结必须失败")
	}
}

func TestFreezeInvalidSideRejected(t *testing.T) {
	p := New(1000)
	if p.Freeze(exchange.OrderRequest{Symbol: "BTC-USDT", Side: "junk", Price: 10, Qty: 1, ClientOrderID: "f4"}) {
		t.Fatal("非法方向冻结必须失败")
	}
}

func TestFreezeBuyZeroPriceRejected(t *testing.T) {
	p := New(1000)
	if p.Freeze(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 0, Qty: 1, ClientOrderID: "f5"}) {
		t.Fatal("买单零价冻结必须失败")
	}
}

func TestFreezeIdempotentSameKey(t *testing.T) {
	p := New(1000)
	req := exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 100, Qty: 1, ClientOrderID: "f6"}
	if !p.Freeze(req) || !p.Freeze(req) {
		t.Fatal("同键重复冻结应幂等成功")
	}
	if p.Cash != 900 {
		t.Fatalf("重复冻结不得重复扣款: %v", p.Cash)
	}
}

func TestReleaseOrderEmptyAndUnknown(t *testing.T) {
	p := New(1000)
	p.ReleaseOrder("")     // 空 ID 直接返回
	p.ReleaseOrder("nope") // 未知 ID 幂等
	if p.Cash != 1000 {
		t.Fatal("无效应不改动账本")
	}
}

func TestConsumeFreeze(t *testing.T) {
	p := New(1000)
	req := exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Price: 100, Qty: 1, ClientOrderID: "c1"}
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	p.Freeze(req)
	if pos := p.Positions["BTC-USDT"]; pos.Available != 0 {
		t.Fatalf("卖出冻结应清空可用: %v", pos.Available)
	}
	// ConsumeFreeze 是纯记账（减少冻结余量），不动持仓
	p.ConsumeFreeze("c1", 0.4)
	// 释放只归还剩余 0.6：已消费的 0.4 对应的持仓变动须由 ApplyFill 驱动
	p.ReleaseOrder("c1")
	if pos := p.Positions["BTC-USDT"]; pos.Available != 0.6 {
		t.Fatalf("消费 0.4 后释放应归还 0.6: %v", pos.Available)
	}
	// 边界：空 ID、零/负数量、未知 ID 均无副作用
	p.ConsumeFreeze("", 1)
	p.ConsumeFreeze("c1", 1) // 已释放，无效果
	p.ConsumeFreeze("unknown", -1)
	if pos := p.Positions["BTC-USDT"]; pos.Available != 0.6 {
		t.Fatalf("边界调用不得改动账本: %v", pos.Available)
	}
}

func TestApplyFillIgnoresInvalid(t *testing.T) {
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 0, Price: 100})
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 0})
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: -1, Price: 100})
	p.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 0, AvgPrice: 100}) // FilledQty=0 忽略
	if p.Cash != 1000 || len(p.Positions) != 0 {
		t.Fatal("非法成交不得改动账本")
	}
}

func TestFrozenBuyFillReturnsPriceDiff(t *testing.T) {
	p := New(1000)
	req := exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 100, Qty: 1, ClientOrderID: "b1"}
	if !p.Freeze(req) {
		t.Fatal("冻结失败")
	}
	if p.Cash != 900 {
		t.Fatalf("冻结应扣款: %v", p.Cash)
	}
	// 实际以更优价 95 成交：退冻结 100、扣成交 95
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", ClientOrderID: "b1", Side: exchange.Buy, Qty: 1, Price: 95, Fee: 0})
	if p.Cash != 905 {
		t.Fatalf("冻结单成交应按成交价结算: %v", p.Cash)
	}
	if pos := p.Positions["BTC-USDT"]; pos.Qty != 1 || pos.Available != 1 || pos.AvgPrice != 95 {
		t.Fatalf("冻结买单成交后持仓错误: %+v", pos)
	}
}

func TestFrozenSellFillKeepsAvailableConsistent(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 2, 100, 0))
	req := exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Price: 120, Qty: 1, ClientOrderID: "s1"}
	if !p.Freeze(req) {
		t.Fatal("冻结失败")
	}
	if pos := p.Positions["BTC-USDT"]; pos.Available != 1 {
		t.Fatalf("卖出冻结应扣可用: %v", pos.Available)
	}
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", ClientOrderID: "s1", Side: exchange.Sell, Qty: 1, Price: 115, Fee: 0})
	pos := p.Positions["BTC-USDT"]
	if pos.Qty != 1 || pos.Available != 1 {
		t.Fatalf("冻结卖单成交后 Qty/Available 应一致: %+v", pos)
	}
	if p.Cash != 1000-200+115 {
		t.Fatalf("卖出结算错误: %v", p.Cash)
	}
}

func TestFrozenPartialFillsThenRelease(t *testing.T) {
	p := New(1000)
	req := exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 100, Qty: 1, ClientOrderID: "p1"}
	p.Freeze(req)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", ClientOrderID: "p1", Side: exchange.Buy, Qty: 0.4, Price: 100, Fee: 0})
	p.ReleaseOrder("p1")
	// 成交 0.4 花 40：1000 - 40 = 960（冻结 100 已按份额退还并按成交价重扣）
	if p.Cash != 960 {
		t.Fatalf("部分成交后释放余额错误: %v", p.Cash)
	}
	if pos := p.Positions["BTC-USDT"]; pos.Qty != 0.4 {
		t.Fatalf("部分成交持仓错误: %v", pos.Qty)
	}
}

func TestFeePositiveAndNegative(t *testing.T) {
	// 当前口径：正负符号的 fee 均从现金扣除（兼容 OKX 正数=支付 与注释约定 负数=支付）
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100, Fee: -2})
	if p.Cash != 898 {
		t.Fatalf("负 fee 应扣现金: %v", p.Cash)
	}
	p2 := New(1000)
	p2.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100, Fee: 3})
	if p2.Cash != 897 {
		t.Fatalf("正 fee 应扣现金: %v", p2.Cash)
	}
}

func TestSellToZeroResetsAvgPrice(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	p.ApplyTrade(filled("BTC-USDT", exchange.Sell, 1, 100, 0))
	pos := p.Positions["BTC-USDT"]
	if pos.Qty != 0 || pos.AvgPrice != 0 {
		t.Fatalf("清仓后均价应归零: %+v", pos)
	}
}

func TestPositionNotionalFallsBackToAvgPrice(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 2, 50, 0))
	if n := p.PositionNotional("BTC-USDT"); n != 100 {
		t.Fatalf("无标记价时按均价估值: %v", n)
	}
	if n := p.PositionNotional("ETH-USDT"); n != 0 {
		t.Fatalf("无持仓估值应为 0: %v", n)
	}
}

func TestUpdateMarkIgnoresNonPositive(t *testing.T) {
	p := New(1000)
	p.UpdateMark("BTC-USDT", 100)
	p.UpdateMark("BTC-USDT", 0)
	p.UpdateMark("BTC-USDT", -5)
	if p.Mark("BTC-USDT") != 100 {
		t.Fatalf("非法标记价不得覆盖: %v", p.Mark("BTC-USDT"))
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	cash, positions, marks := p.Snapshot()
	positions[0].Qty = 999
	marks["BTC-USDT"] = 999
	if p.Positions["BTC-USDT"].Qty != 1 || p.Mark("BTC-USDT") != 100 || cash != 900 {
		t.Fatal("快照必须是深拷贝，外部改动不得泄漏回账本")
	}
}

func TestReconcilePositionDrift(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	diffs := p.Reconcile(nil, []Position{{Symbol: "BTC-USDT", Qty: 1.5}})
	if len(diffs) == 0 {
		t.Fatal("持仓差异必须被发现")
	}
	// 一致的持仓不报差异
	if d := p.Reconcile(nil, []Position{{Symbol: "BTC-USDT", Qty: 1}}); len(d) != 0 {
		t.Fatalf("一致持仓不应报差异: %v", d)
	}
}

func TestReconcileCashTolerance(t *testing.T) {
	p := New(1000)
	// 0.5‰ 以内不报
	if d := p.Reconcile([]exchange.Balance{{Asset: "USDT", Total: 1000.4}}, nil); len(d) != 0 {
		t.Fatalf("容差内不应报差异: %v", d)
	}
	// 超容差报差异
	if d := p.Reconcile([]exchange.Balance{{Asset: "USDT", Total: 1100}}, nil); len(d) == 0 {
		t.Fatal("超容差现金差异必须报告")
	}
}

func TestConcurrentAccess(t *testing.T) {
	p := New(100000)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 0.001, Price: 100, Fee: 0})
			p.UpdateMark("BTC-USDT", 101)
			_ = p.Equity()
			_ = p.PositionNotional("BTC-USDT")
			p.Freeze(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 10, Qty: 0.01})
		}()
	}
	wg.Wait()
}
