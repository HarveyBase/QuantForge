package portfolio

import (
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func filled(symbol string, side exchange.Side, qty, px, fee float64) exchange.Order {
	return exchange.Order{
		Exchange: "okx", Symbol: symbol, Side: side, Type: exchange.OrderMarket,
		Qty: qty, FilledQty: qty, AvgPrice: px, Fee: -fee, FeeCcy: "USDT",
		Status: exchange.StatusFilled,
	}
}

func TestEquityBuyHold(t *testing.T) {
	p := New(10000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 0.1, 50000, 5))
	p.UpdateMark("BTC-USDT", 60000)
	// cash = 10000 - 5000 - 5 = 4995; BTC = 0.1 * 60000 = 6000
	if e := p.Equity(); e < 10994.9 || e > 10995.1 {
		t.Fatalf("权益计算错误: %v", e)
	}
}

func TestAvgPriceOnAdd(t *testing.T) {
	p := New(100000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 200, 0))
	pos := p.Positions["BTC-USDT"]
	if pos.AvgPrice != 150 {
		t.Fatalf("加仓均价应 150: %v", pos.AvgPrice)
	}
}

func TestSellUpdatesCashAndQty(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	p.ApplyTrade(filled("BTC-USDT", exchange.Sell, 0.5, 150, 0))
	pos := p.Positions["BTC-USDT"]
	if pos.Qty != 0.5 {
		t.Fatalf("卖出后持仓错误: %v", pos.Qty)
	}
	// 1000 - 100 + 75 = 975
	if p.Cash != 975 {
		t.Fatalf("现金计算错误: %v", p.Cash)
	}
}

func TestFreezeReleaseAffectsSellable(t *testing.T) {
	p := New(1000)
	p.ApplyTrade(filled("BTC-USDT", exchange.Buy, 1, 100, 0))
	req := exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Price: 110, Qty: 0.4}
	p.Freeze(req)
	if pos := p.Positions["BTC-USDT"]; pos.Available != 0.6 {
		t.Fatalf("挂单后可卖应为 0.6: %v", pos.Available)
	}
	p.Release(req)
	if pos := p.Positions["BTC-USDT"]; pos.Available != 1 {
		t.Fatalf("撤单后可卖应恢复 1: %v", pos.Available)
	}
}

func TestReconcileDetectsDrift(t *testing.T) {
	p := New(500)
	diffs := p.Reconcile([]exchange.Balance{{Asset: "USDT", Total: 600, Available: 600}}, nil)
	if len(diffs) == 0 {
		t.Fatal("现金差异必须被对账发现")
	}
}
