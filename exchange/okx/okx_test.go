package okx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func newMockClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client(), TdMode: "cross"}
}

func mustReq() exchange.OrderRequest {
	return exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50000, Qty: 0.001, ClientOrderID: "c1",
	}
}

func TestGetCandlesSortsAndParses(t *testing.T) {
	// OKX 返回最新在前 + 字符串数值 + confirm 标志
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/candles", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bar") != "1H" {
			t.Errorf("bar 参数错误: %s", r.URL.Query().Get("bar"))
		}
		w.Write([]byte(`{"code":"0","data":[
			["1700000000000","101","105","99","103","12.5","0","0","0"],
			["1699996400000","100","102","98","101","10.0","0","0","1"],
			["1699992800000","99","101","97","100","8.0","0","0","1"]
		]}`))
	})
	c := newMockClient(t, mux)
	cs, err := c.GetCandles(context.Background(), "BTC-USDT", "1H", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("应有 3 根 K 线，得到 %d", len(cs))
	}
	if cs[0].OpenTime >= cs[1].OpenTime || cs[1].OpenTime >= cs[2].OpenTime {
		t.Fatal("K 线必须升序")
	}
	if cs[2].Confirmed || !cs[0].Confirmed {
		t.Fatal("confirm 解析错误（未收盘 K 线必须标记，禁入回测）")
	}
	if cs[0].High != 101 || cs[0].Volume != 8 {
		t.Fatalf("数值解析错误: %+v", cs[0])
	}
	if cs[0].Key() == cs[1].Key() {
		t.Fatal("唯一键冲突")
	}
}

func TestBusinessError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"51001","msg":"Instrument ID does not exist"}`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetTicker(context.Background(), "BAD-PAIR"); err == nil {
		t.Fatal("业务错误码必须转为 error")
	}
}

func TestPlaceOrderRequiresCreds(t *testing.T) {
	c := NewPublic()
	_, err := c.PlaceOrder(context.Background(), mustReq())
	if err == nil {
		t.Fatal("无 Key 时交易接口必须拒绝，而不是静默失败")
	}
}

func TestPlaceOrderDemoHeader(t *testing.T) {
	var gotDemo, gotAuth bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		gotDemo = r.Header.Get(demoHeader) == "1"
		gotAuth = r.Header.Get("OK-ACCESS-KEY") == "k"
		w.Write([]byte(`{"code":"0","data":[{"ordId":"123","clOrdID":"c1","sCode":"0","sMsg":""}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	c.Simulated = true
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err != nil {
		t.Fatal(err)
	}
	if !gotDemo || !gotAuth {
		t.Fatal("paper 模式必须带 demo 头 + 鉴权头")
	}
}

func TestPlaceOrderSwapQtyInContracts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01","lotSz":"1","minSz":"1","tickSz":"0.1"}]}`))
	})
	var sz, tdMode, posSide string
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		sz, tdMode, posSide = body["sz"], body["tdMode"], body["posSide"]
		w.Write([]byte(`{"code":"0","data":[{"ordId":"9","clOrdID":"c2","sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	req := mustReq()
	req.Symbol = "BTC-USDT-SWAP"
	req.Qty = 0.03 // 0.03 BTC / 0.01 面值 = 3 张
	if _, err := c.PlaceOrder(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if sz != "3" {
		t.Fatalf("合约数量应换算为 3 张，得到 %s", sz)
	}
	if tdMode != "cross" || posSide != "net" {
		t.Fatalf("合约下单参数错误: tdMode=%s posSide=%s", tdMode, posSide)
	}
}
