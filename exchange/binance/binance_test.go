package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func newMock(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewWithURL(srv.URL)
}

func TestCandlesTickerOrderBook(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/klines", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") != "BTCUSDT" || r.URL.Query().Get("interval") != "1h" {
			t.Errorf("参数错误: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[[1700000000000,"100","101","99","100.5","12",1700003600000,"x",0,0,0,0]]`))
	})
	mux.HandleFunc("/api/v3/ticker/bookTicker", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"bidPrice":"99.9","askPrice":"100.1"}`))
	})
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"bids":[["99.9","1"]],"asks":[["100.1","2"]]}`))
	})
	c := newMock(t, mux)
	cs, err := c.GetCandles(context.Background(), "BTC-USDT", "1H", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Close != 100.5 || cs[0].Exchange != "binance" {
		t.Fatalf("K 线解析错误: %+v", cs)
	}
	tk, _ := c.GetTicker(context.Background(), "BTC-USDT")
	if tk.Bid != 99.9 || tk.Ask != 100.1 {
		t.Fatalf("ticker 错误: %+v", tk)
	}
	ob, _ := c.GetOrderBook(context.Background(), "BTC-USDT", 5)
	if len(ob.Bids) != 1 || ob.Bids[0].Price != 99.9 || ob.Asks[0].Price != 100.1 {
		t.Fatalf("盘口错误: %+v", ob)
	}
	if bp := ob.SpreadBp(); bp < 19 || bp > 21 {
		t.Fatalf("价差错误: %v", bp)
	}
	if _, err := c.GetCandles(context.Background(), "BTC-USDT", "7x", 1); err == nil {
		t.Fatal("非法周期应报错")
	}
}

func TestSignedEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	var gotSig, gotKey bool
	mux.HandleFunc("/api/v3/account", func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.URL.Query().Get("signature") != ""
		gotKey = r.Header.Get("X-MBX-APIKEY") != ""
		w.Write([]byte(`{"balances":[{"asset":"BTC","free":"0.5","locked":"0.1"},{"asset":"DUST","free":"0","locked":"0"}]}`))
	})
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Query().Get("side") != "BUY" || r.URL.Query().Get("type") != "LIMIT" {
				t.Errorf("下单参数错误: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"orderId":123,"clientOrderId":"c1","status":"NEW"}`))
			return
		}
		w.Write([]byte(`{"orderId":123,"clientOrderId":"c1","side":"BUY","type":"LIMIT","price":"50000","origQty":"0.001","executedQty":"0.001","cummulativeQuoteQty":"50","status":"FILLED","time":1,"updateTime":2}`))
	})
	c := newMock(t, mux)
	c.APIKey, c.Secret = "k", "s"
	bs, err := c.GetBalances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !gotSig || !gotKey {
		t.Fatal("签名请求必须带 signature 与 X-MBX-APIKEY")
	}
	if len(bs) != 1 || bs[0].Total != 0.6 || bs[0].Available != 0.5 {
		t.Fatalf("余额解析错误（零余额应剔除）: %+v", bs)
	}
	o, err := c.PlaceOrder(context.Background(), mustReq())
	if err != nil {
		t.Fatal(err)
	}
	if o.OrderID != "123" || o.Status != exchange.StatusSubmitted {
		t.Fatalf("下单回执错误: %+v", o)
	}
	got, err := c.GetOrder(context.Background(), "BTC-USDT", "123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != exchange.StatusFilled || got.FilledQty != 0.001 || got.AvgPrice != 50000 {
		t.Fatalf("订单解析错误: %+v", got)
	}
	c2 := newMock(t, mux)
	if _, err := c2.GetBalances(context.Background()); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatal("无凭据必须拒绝签名接口")
	}
}

func mustReq() exchange.OrderRequest {
	return exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 50000, Qty: 0.001, ClientOrderID: "c1"}
}
