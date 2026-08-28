package paper

import (
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestGetCandlesUnsupported(t *testing.T) {
	e := New(100, 1000, FillModel{})
	if _, err := e.GetCandles(ctx(), "BTC-USDT", "1H", 10); err == nil {
		t.Fatal("本地撮合不提供 K 线")
	}
}

func TestGetTickerBeforeInit(t *testing.T) {
	e := New(0, 1000, FillModel{})
	if _, err := e.GetTicker(ctx(), "BTC-USDT"); err == nil {
		t.Fatal("价格未初始化必须报错")
	}
}

func TestGetTickerSpread(t *testing.T) {
	e := New(100, 1000, FillModel{})
	tk, err := e.GetTicker(ctx(), "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Bid >= tk.Ask || tk.Bid >= 100 || tk.Ask <= 100 {
		t.Fatalf("盘口应围绕 last 对称: %+v", tk)
	}
}

func TestGetInstrumentStub(t *testing.T) {
	e := New(100, 1000, FillModel{})
	ins, err := e.GetInstrument(ctx(), "BTC-USDT")
	if err != nil || ins.Market != exchange.MarketSPOT || ins.Quote != "USDT" {
		t.Fatalf("合约规格存根错误: %+v", ins)
	}
}

func TestPlaceOrderInvalidQty(t *testing.T) {
	e := New(100, 1000, FillModel{})
	if _, err := e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 0}); err == nil {
		t.Fatal("零数量必须报错")
	}
}

func TestMarketSellSlippage(t *testing.T) {
	e := New(100, 1000, FillModel{SlippageBps: 10, FeeBps: 0})
	// 先买入建仓
	e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1})
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Type: exchange.OrderMarket, Qty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusFilled || o.AvgPrice >= 100 {
		t.Fatalf("卖单滑点应使成交价低于 last: %+v", o)
	}
}

func TestSellInsufficientBaseRejected(t *testing.T) {
	e := New(100, 10000, FillModel{})
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Type: exchange.OrderMarket, Qty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusRejected {
		t.Fatalf("无持仓卖出应拒单: %s", o.Status)
	}
}

func TestLimitBuyImmediateFillOnEntry(t *testing.T) {
	e := New(100, 1000, FillModel{FeeBps: 5})
	// 买价 105 >= last 100：下单价即可成交
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 105, Qty: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusFilled || o.AvgPrice != 105 {
		t.Fatalf("触及价应立即按委托价成交: %+v", o)
	}
}

func TestPartialFillThenComplete(t *testing.T) {
	e := New(100, 10000, FillModel{PartialRatio: 0.5, FeeBps: 0})
	o, _ := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 95, Qty: 1, ClientOrderID: "pp",
	})
	if o.Status != exchange.StatusSubmitted {
		t.Fatalf("未触及应挂起: %s", o.Status)
	}
	e.UpdatePrice(94) // 触及 → 半量成交

	got, _ := e.GetOrder(ctx(), "BTC-USDT", o.OrderID)
	if got.Status != exchange.StatusPartiallyFilled || got.FilledQty != 0.5 {
		t.Fatalf("应部分成交 0.5: %+v", got)
	}
	// 每次触发只成交剩余的一半，循环直至补足
	for i := 0; ; i++ {
		if i > 60 {
			t.Fatal("多次触发后仍未补足")
		}
		e.UpdatePrice(93 - float64(i))
		got2, _ := e.GetOrder(ctx(), "BTC-USDT", o.OrderID)
		if got2.Status == exchange.StatusFilled {
			if got2.FilledQty != 1 {
				t.Fatalf("补足后数量应精确: %+v", got2)
			}
			break
		}
	}
}

func TestPartialFillCannotRefreezeRejected(t *testing.T) {
	// 部分成交后现金不足以支撑剩余冻结 → 订单转 rejected
	e := New(100, 95, FillModel{PartialRatio: 0.5, FeeBps: 0})
	o, _ := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 90, Qty: 1,
	})
	// 冻结 90，剩 5 现金
	e.UpdatePrice(89) // 触发部分成交 0.5（45.0）
	got, _ := e.GetOrder(ctx(), "BTC-USDT", o.OrderID)
	if got.Status == exchange.StatusFilled {
		t.Fatalf("此场景不应全成: %+v", got)
	}
}

func TestUpdatePriceIgnoresNonPositive(t *testing.T) {
	e := New(100, 1000, FillModel{})
	e.UpdatePrice(-5)
	e.UpdatePrice(0)
	tk, _ := e.GetTicker(ctx(), "BTC-USDT")
	if tk.Last != 100 {
		t.Fatalf("非法价格不得更新: %v", tk.Last)
	}
}

func TestCancelNonexistentOrder(t *testing.T) {
	e := New(100, 1000, FillModel{})
	if err := e.CancelOrder(ctx(), "BTC-USDT", "paper-999"); err == nil {
		t.Fatal("撤不存在订单必须报错")
	}
}

func TestGetOrderNonexistent(t *testing.T) {
	e := New(100, 1000, FillModel{})
	if _, err := e.GetOrder(ctx(), "BTC-USDT", "nope"); err == nil {
		t.Fatal("查询不存在订单必须报错")
	}
}

func TestGetOrderByClientIDMismatch(t *testing.T) {
	e := New(100, 1000, FillModel{})
	e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 50, Qty: 1, ClientOrderID: "mm",
	})
	if _, err := e.GetOrderByClientID(ctx(), "ETH-USDT", "mm"); err == nil {
		t.Fatal("symbol 不匹配必须报错")
	}
	if _, err := e.GetOrderByClientID(ctx(), "BTC-USDT", "unknown"); err == nil {
		t.Fatal("未知 clientOrderID 必须报错")
	}
	if _, err := e.GetOrderByClientID(ctx(), "BTC-USDT", "mm"); err != nil {
		t.Fatalf("匹配查询应成功: %v", err)
	}
	// 空 symbol 跳过匹配检查
	if _, err := e.GetOrderByClientID(ctx(), "", "mm"); err != nil {
		t.Fatalf("空 symbol 应跳过校验: %v", err)
	}
}

func TestGetOpenOrdersFiltersSymbol(t *testing.T) {
	e := New(100, 1000, FillModel{})
	e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 50, Qty: 1})
	if os, _ := e.GetOpenOrders(ctx(), "BTC-USDT"); len(os) != 1 {
		t.Fatalf("应有一张挂单: %d", len(os))
	}
	if os, _ := e.GetOpenOrders(ctx(), "ETH-USDT"); len(os) != 0 {
		t.Fatalf("其他 symbol 不应返回: %d", len(os))
	}
}

func TestDefaultFillModelRatio(t *testing.T) {
	// PartialRatio 非法时回退 1
	e := New(100, 10000, FillModel{PartialRatio: 0})
	o, _ := e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 105, Qty: 1})
	if o.FilledQty != 1 {
		t.Fatalf("默认全成: %+v", o)
	}
	e2 := New(100, 10000, FillModel{PartialRatio: 1.5})
	o2, _ := e2.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 105, Qty: 1})
	if o2.FilledQty != 1 {
		t.Fatalf("超界比例回退全成: %+v", o2)
	}
}

func TestSellOrderFreezesBaseAndFills(t *testing.T) {
	e := New(100, 10000, FillModel{FeeBps: 0})
	// 建仓 1 BTC
	e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1})
	baseBefore, _ := e.GetBalances(ctx())
	btcBefore := findAsset(baseBefore, "BTC")
	// 挂高价卖单：冻结 0.6 BTC
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Sell, Type: exchange.OrderLimit, Price: 120, Qty: 0.6, ClientOrderID: "sf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusSubmitted {
		t.Fatalf("未触及应挂起: %s", o.Status)
	}
	balMid, _ := e.GetBalances(ctx())
	if got := findAsset(balMid, "BTC"); got != btcBefore-0.6 {
		t.Fatalf("卖单应冻结 BTC: %v", got)
	}
	// 撤单释放
	if err := e.CancelOrder(ctx(), "BTC-USDT", o.OrderID); err != nil {
		t.Fatal(err)
	}
	balEnd, _ := e.GetBalances(ctx())
	if got := findAsset(balEnd, "BTC"); got != btcBefore {
		t.Fatalf("撤单应归还冻结: %v", got)
	}
}

func TestSellOrderFillsOnPriceCross(t *testing.T) {
	e := New(100, 10000, FillModel{FeeBps: 10})
	e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1})
	o, _ := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Sell, Type: exchange.OrderLimit, Price: 105, Qty: 0.5,
	})
	e.UpdatePrice(106) // 价格上穿卖价
	got, _ := e.GetOrder(ctx(), "BTC-USDT", o.OrderID)
	if got.Status != exchange.StatusFilled || got.AvgPrice != 105 {
		t.Fatalf("上穿应按委托价成交: %+v", got)
	}
}

func TestSellReserveInsufficientRejected(t *testing.T) {
	e := New(100, 10000, FillModel{})
	e.PlaceOrder(ctx(), exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 0.1})
	// 挂 0.5 卖单但只有 0.1 BTC：冻结失败 → 拒单
	o, err := e.PlaceOrder(ctx(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Sell, Type: exchange.OrderLimit, Price: 105, Qty: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusRejected {
		t.Fatalf("卖侧冻结不足应拒单: %s", o.Status)
	}
}
