package paper

import (
	"context"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestLimitOrderFillsOnCross(t *testing.T) {
	e := New(100, 10000, FillModel{FeeBps: 10})
	// 价格 100，挂 95 买
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "c1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusSubmitted {
		t.Fatalf("未触及应挂起，得到 %s", o.Status)
	}
	e.UpdatePrice(94.9)
	got, _ := e.GetOrder(ctx(), "BTC-USDT", o.OrderID)
	if got.Status != exchange.StatusFilled || got.AvgPrice != 95 {
		t.Fatalf("价格穿越后应成交: %+v", got)
	}
	bal, _ := e.GetBalances(ctx())
	usdt := findAsset(bal, "USDT")
	if usdt < 10000-95.95-0.01 { // 95 本金 + 0.095 手续费，允许浮点误差
		t.Fatalf("余额扣减错误: %v", usdt)
	}
}

func TestMarketOrderSlippage(t *testing.T) {
	e := New(100, 10000, FillModel{SlippageBps: 10, FeeBps: 0})
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusFilled {
		t.Fatalf("市价单应立即成交: %s", o.Status)
	}
	if o.AvgPrice <= 100 {
		t.Fatalf("买单滑点应使成交价高于 last: %v", o.AvgPrice)
	}
}

func TestInsufficientFundsRejected(t *testing.T) {
	e := New(100, 50, FillModel{})
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusRejected {
		t.Fatalf("资金不足应拒单: %s", o.Status)
	}
}

func TestCancelReleasesFunds(t *testing.T) {
	e := New(100, 1000, FillModel{})
	o, _ := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 1, ClientOrderID: "c2",
	})
	bal, _ := e.GetBalances(ctx())
	if findAsset(bal, "USDT") != 950 {
		t.Fatalf("挂单应冻结资金: %v", findAsset(bal, "USDT"))
	}
	if err := e.CancelOrder(ctx(), "BTC-USDT", o.OrderID); err != nil {
		t.Fatal(err)
	}
	bal, _ = e.GetBalances(ctx())
	if findAsset(bal, "USDT") != 1000 {
		t.Fatalf("撤单应释放资金: %v", findAsset(bal, "USDT"))
	}
}

func ctx() context.Context { return context.Background() }

func findAsset(bs []exchange.Balance, a string) float64 {
	for _, b := range bs {
		if b.Asset == a {
			return b.Available
		}
	}
	return 0
}
