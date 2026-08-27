package execution

import (
	"context"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	paperex "github.com/HarveyBase/QuantForge/exchange/paper"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
)

func newExecutor(t *testing.T, seedCash float64) (*Executor, *paperex.Exchange) {
	t.Helper()
	pex := paperex.New(100, seedCash, paperex.FillModel{FeeBps: 0})
	pf := portfolio.New(seedCash)
	rk := risk.NewManager(risk.Limits{
		MaxOrderNotionalUSD: 100000, MaxDailyNotionalUSD: 1000000,
		MaxPositionNotionalUSD: 1000000, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50,
	}, pf, "")
	return New(pex, rk, pf, nil), pex
}

func TestSubmitGoesThroughRisk(t *testing.T) {
	e, _ := newExecutor(t, 10000)
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusSubmitted {
		t.Fatalf("未触及应挂起: %s", o.Status)
	}
	if len(e.OpenOrders()) != 1 {
		t.Fatal("订单簿应登记挂单")
	}
}

func TestIdempotentClientOrderID(t *testing.T) {
	e, _ := newExecutor(t, 10000)
	req := exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "dup",
	}
	if _, err := e.Submit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(context.Background(), req); err == nil {
		t.Fatal("相同 clientOrderID 重复提交必须被幂等拒绝（防重试风暴）")
	}
}

func TestRiskRejectionNoOrder(t *testing.T) {
	e, _ := newExecutor(t, 10000)
	e.Rk.Kill.Trip("演练")
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 0.01,
	}); err == nil {
		t.Fatal("Kill Switch 下必须拒单")
	}
	if len(e.OpenOrders()) != 0 {
		t.Fatal("拒单不得进入订单簿")
	}
	evs := e.Events(0)
	if len(evs) != 1 || evs[0].Kind != "rejected" {
		t.Fatalf("拒单事件必须留痕: %+v", evs)
	}
}

func TestReconcileDrivesPortfolio(t *testing.T) {
	e, pex := newExecutor(t, 10000)
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 105, Qty: 1, ClientOrderID: "t2", // 高于当前价，先挂起
	})
	if err != nil {
		t.Fatal(err)
	}
	pex.UpdatePrice(104) // 价格穿越成交
	e.ReconcileOnce(context.Background())
	if got, err := pex.GetOrder(context.Background(), "BTC-USDT", o.OrderID); err != nil || got.Status != exchange.StatusFilled {
		t.Fatalf("回报同步应看到成交: %+v err=%v", got, err)
	}
	_, positions, _ := e.Pf.Snapshot()
	found := false
	for _, p := range positions {
		if p.Symbol == "BTC-USDT" && p.Qty == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("成交必须驱动组合账本（先查后补，不盲目重发）")
	}
}

func TestCancelReleasesFreeze(t *testing.T) {
	e, _ := newExecutor(t, 10000)
	o, _ := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 90, Qty: 1, ClientOrderID: "t3",
	})
	cash, _, _ := e.Pf.Snapshot()
	if cash != 10000-90 {
		t.Fatalf("挂单应冻结资金: %v", cash)
	}
	if err := e.Cancel(context.Background(), "BTC-USDT", o.OrderID); err != nil {
		t.Fatal(err)
	}
	cash, _, _ = e.Pf.Snapshot()
	if cash != 10000 {
		t.Fatalf("撤单应释放冻结: %v", cash)
	}
}
