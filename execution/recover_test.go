package execution

import (
	"context"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
)

// TestAdoptOpenOrdersOrphan 交易所在途孤儿单被采纳：进入订单簿、claimed 标记、emit adopted 事件。
func TestAdoptOpenOrdersOrphan(t *testing.T) {
	orphan := exchange.Order{
		Exchange: "fake", Symbol: "BTC-USDT", OrderID: "ex-777", ClientOrderID: "qf-123-grid-0",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 95, Qty: 1, Status: exchange.StatusSubmitted,
	}
	f := &fakeEx{open: []exchange.Order{orphan}, byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{"ex-777": orphan}}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(f, rk, pf, nil)

	n, err := e.AdoptOpenOrders(context.Background(), "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应采纳 1 张孤儿单, got %d", n)
	}
	open := e.OpenOrders()
	if len(open) != 1 || open[0].OrderID != "ex-777" {
		t.Fatalf("孤儿单应进入订单簿: %+v", open)
	}
	// 幂等键被认领：重启后本地不得重复使用同一 clientOrderID
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "qf-123-grid-0",
	}); err == nil {
		t.Fatal("被采纳订单的 clientOrderID 必须标记 claimed 防重发")
	}
	evs := e.Events(0)
	if len(evs) != 1 || evs[0].Kind != "adopted" {
		t.Fatalf("采纳必须 emit adopted 事件留痕: %+v", evs)
	}
}

// TestAdoptOpenOrdersKnownAligns 本地已有的在途单：用交易所状态对齐而非重复采纳。
func TestAdoptOpenOrdersKnownAligns(t *testing.T) {
	fresh := exchange.Order{
		Exchange: "fake", Symbol: "BTC-USDT", OrderID: "ex-1", ClientOrderID: "c1",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 95, Qty: 1,
		Status: exchange.StatusFilled, AvgPrice: 95, FilledQty: 1,
	}
	f := &fakeEx{open: []exchange.Order{fresh}, byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{"ex-1": fresh}}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(f, rk, pf, nil)
	e.mu.Lock()
	e.orders["ex-1"] = exchange.Order{OrderID: "ex-1", ClientOrderID: "c1", Symbol: "BTC-USDT", Status: exchange.StatusSubmitted}
	e.claimed["c1"] = true
	e.mu.Unlock()

	n, err := e.AdoptOpenOrders(context.Background(), "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("本地已有订单不算采纳, got %d", n)
	}
	if len(e.OpenOrders()) != 0 {
		t.Fatal("交易所已成交的订单应对齐为终态离场")
	}
}

// TestAdoptOpenOrdersTerminalSkipped 在途列表若含终态单：只认领幂等键不进挂单表。
func TestAdoptOpenOrdersTerminalSkipped(t *testing.T) {
	term := exchange.Order{
		Exchange: "fake", Symbol: "BTC-USDT", OrderID: "ex-9", ClientOrderID: "qf-x",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 95, Qty: 1, Status: exchange.StatusCancelled,
	}
	f := &fakeEx{open: []exchange.Order{term}, byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{}}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(f, rk, pf, nil)
	if _, err := e.AdoptOpenOrders(context.Background(), "BTC-USDT"); err != nil {
		t.Fatal(err)
	}
	if len(e.OpenOrders()) != 0 {
		t.Fatal("终态单不得进入挂单表")
	}
}
