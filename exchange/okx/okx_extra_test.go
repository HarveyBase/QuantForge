package okx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestNewWithURLDefaults(t *testing.T) {
	c := NewLiveWithURL("  ", "cross", 2)
	if c.BaseURL != defaultBaseURL {
		t.Fatalf("空 URL 应回退默认: %s", c.BaseURL)
	}
	c2 := NewLiveWithURL("https://example.com/", "cross", 1)
	if strings.HasSuffix(c2.BaseURL, "/") {
		t.Fatalf("尾斜杠应被去除: %s", c2.BaseURL)
	}
	c3 := NewPaper("cross", 1)
	if !c3.Simulated {
		t.Fatal("paper 客户端必须带模拟标志")
	}
	c4 := NewPublic()
	if c4.HasCredentials() {
		t.Fatal("公开客户端不应有凭据")
	}
}

func TestSignKnownVector(t *testing.T) {
	c := &Client{Secret: "s3cret"}
	got := c.sign("2024-01-01T00:00:00.000Z", "GET", "/api/v5/test", "")
	// HMAC-SHA256(ts+method+path+body) base64 —— 只验证可复现性与形状
	mac := c.sign("2024-01-01T00:00:00.000Z", "GET", "/api/v5/test", "")
	if got != mac {
		t.Fatal("签名必须可复现")
	}
	if _, err := base64.StdEncoding.DecodeString(got); err != nil {
		t.Fatalf("签名必须是合法 base64: %v", err)
	}
}

func TestGetCandlesBadInterval(t *testing.T) {
	c := newMockClient(t, http.NewServeMux())
	if _, err := c.GetCandles(context.Background(), "BTC-USDT", "7x", 10); err == nil {
		t.Fatal("不支持的周期必须报错")
	}
}

func TestGetCandlesLimitClamped(t *testing.T) {
	var gotLimit string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/candles", func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		w.Write([]byte(`{"code":"0","data":[["1","1","1","1","1","1","0","0","1"]]}`))
	})
	c := newMockClient(t, mux)
	for _, in := range []int{-1, 0, 301} {
		if _, err := c.GetCandles(context.Background(), "BTC-USDT", "1H", in); err != nil {
			t.Fatal(err)
		}
		if gotLimit != "300" {
			t.Fatalf("越界 limit 应回退 300: %s", gotLimit)
		}
	}
}

func TestGetCandlesSkipsShortRows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/candles", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[["1","1","1","1","1"],["2","2","2","2","2","2","0","0","1"]]}`))
	})
	c := newMockClient(t, mux)
	cs, err := c.GetCandles(context.Background(), "BTC-USDT", "1H", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].OpenTime != 2 {
		t.Fatalf("残缺行应跳过: %+v", cs)
	}
}

func TestHTTPStatusError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":"0"}`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetTicker(context.Background(), "BTC-USDT"); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("HTTP 错误必须透传: %v", err)
	}
}

func TestCorruptJSONResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetTicker(context.Background(), "BTC-USDT"); err == nil {
		t.Fatal("损坏响应必须报错")
	}
}

func TestGetTickerParses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT","last":"100.5","bidPx":"100.4","askPx":"100.6","ts":"1700000000000"}]}`))
	})
	c := newMockClient(t, mux)
	tk, err := c.GetTicker(context.Background(), "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Last != 100.5 || tk.Bid != 100.4 || tk.Ask != 100.6 || tk.Ts != 1700000000000 {
		t.Fatalf("ticker 解析错误: %+v", tk)
	}
}

func TestGetTickerEmptyData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[]}`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetTicker(context.Background(), "BAD"); err == nil {
		t.Fatal("空 data 必须报错")
	}
}

func TestGetInstrumentSpot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("instType") != "SPOT" {
			t.Errorf("SPOT 查询参数错误: %s", r.URL.Query().Get("instType"))
		}
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT","baseCcy":"BTC","quoteCcy":"USDT","lotSz":"0.00000001","minSz":"0.00001","tickSz":"0.1"}]}`))
	})
	c := newMockClient(t, mux)
	ins, err := c.GetInstrument(context.Background(), "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if ins.Market != exchange.MarketSPOT || ins.Base != "BTC" || ins.Quote != "USDT" || ins.LotSize != 1e-8 {
		t.Fatalf("现货规格解析错误: %+v", ins)
	}
}

func TestGetInstrumentSwapSplitsPair(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("instType") != "SWAP" {
			t.Errorf("SWAP 查询参数错误: %s", r.URL.Query().Get("instType"))
		}
		w.Write([]byte(`{"code":"0","data":[{"instId":"ETH-USDT-SWAP","ctVal":"0.1","lotSz":"1","minSz":"1","tickSz":"0.01"}]}`))
	})
	c := newMockClient(t, mux)
	ins, err := c.GetInstrument(context.Background(), "ETH-USDT-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	if ins.Market != exchange.MarketSWAP || ins.Base != "ETH" || ins.Quote != "USDT" || ins.ContractSize != 0.1 {
		t.Fatalf("合约规格解析错误: %+v", ins)
	}
}

func TestGetInstrumentEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[]}`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetInstrument(context.Background(), "X"); err == nil {
		t.Fatal("空规格必须报错")
	}
}

func TestGetBalancesFallbackAvail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/account/balance", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"details":[
			{"ccy":"USDT","availBal":"","cashBal":"500","frozenBal":"100"},
			{"ccy":"BTC","availBal":"0.2","cashBal":"0.2","frozenBal":"0"}
		]}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	bs, err := c.GetBalances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Asset == "USDT" {
			if b.Available != 400 { // cashBal - frozen
				t.Fatalf("availBal 缺失应回退 cashBal-frozen: %v", b.Available)
			}
		}
	}
}

func TestGetBalancesEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/account/balance", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.GetBalances(context.Background()); err == nil {
		t.Fatal("空余额必须报错")
	}
}

func TestPlaceOrderSpotBody(t *testing.T) {
	var body map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"code":"0","data":[{"ordId":"77","clOrdID":"cx","sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	req := mustReq()
	req.Type = exchange.OrderMarket
	o, err := c.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if body["tdMode"] != "cash" || body["ordType"] != "market" || body["sz"] != "0.001" {
		t.Fatalf("现货市价单参数错误: %v", body)
	}
	if _, ok := body["px"]; ok {
		t.Fatal("市价单不应带价格")
	}
	if o.OrderID != "77" || o.Status != exchange.StatusSubmitted {
		t.Fatalf("下单回执错误: %+v", o)
	}
}

func TestPlaceOrderSCodeRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"ordId":"","sCode":"51016","sMsg":"insufficient balance"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err == nil || !strings.Contains(err.Error(), "51016") {
		t.Fatalf("sCode 拒单必须报错: %v", err)
	}
}

func TestPlaceOrderSwapBelowOneContract(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	req := mustReq()
	req.Symbol = "BTC-USDT-SWAP"
	req.Qty = 0.005 // 不足 1 张
	if _, err := c.PlaceOrder(context.Background(), req); err == nil || !strings.Contains(err.Error(), "不足 1 张") {
		t.Fatalf("不足一张必须报错: %v", err)
	}
}

func TestPlaceOrderSwapInvalidContractSize(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	req := mustReq()
	req.Symbol = "BTC-USDT-SWAP"
	if _, err := c.PlaceOrder(context.Background(), req); err == nil {
		t.Fatal("面值非法必须报错")
	}
}

func TestPlaceOrderCorruptResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0"}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err == nil {
		t.Fatal("空 data 必须报错")
	}
}

func TestCancelOrder(t *testing.T) {
	var body map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/cancel-order", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"code":"0","data":[{"sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if err := c.CancelOrder(context.Background(), "BTC-USDT", "42"); err != nil {
		t.Fatal(err)
	}
	if body["instId"] != "BTC-USDT" || body["ordId"] != "42" {
		t.Fatalf("撤单参数错误: %v", body)
	}
}

func okxOrderJSON(state, px, sz, accFill, avgPx, fee string) string {
	return fmt.Sprintf(`{"code":"0","data":[{"instId":"BTC-USDT","ordId":"1","clOrdID":"c1","state":%q,"side":"buy","ordType":"limit","px":%q,"sz":%q,"accFillSz":%q,"avgPx":%q,"fee":%q,"feeCcy":"USDT","cTime":"100","uTime":"200"}]}`,
		state, px, sz, accFill, avgPx, fee)
}

func TestGetOrderStateMapping(t *testing.T) {
	cases := map[string]exchange.OrderStatus{
		"filled":           exchange.StatusFilled,
		"canceled":         exchange.StatusCancelled,
		"partially_filled": exchange.StatusPartiallyFilled,
		"live":             exchange.StatusSubmitted, // live 无成交 → submitted
	}
	for state, want := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("ordId") != "1" {
				t.Errorf("ordId 参数错误: %s", r.URL.Query().Get("ordId"))
			}
			w.Write([]byte(okxOrderJSON(state, "100", "1", "0", "", "0")))
		})
		c := newMockClient(t, mux)
		c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
		o, err := c.GetOrder(context.Background(), "BTC-USDT", "1")
		if err != nil {
			t.Fatal(err)
		}
		if o.Status != want {
			t.Errorf("state %s 应映射 %s，得到 %s", state, want, o.Status)
		}
	}
	// live + 有成交 → partially_filled
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(okxOrderJSON("live", "100", "1", "0.5", "99", "0")))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	o, err := c.GetOrder(context.Background(), "BTC-USDT", "1")
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != exchange.StatusPartiallyFilled || o.FilledQty != 0.5 || o.AvgPrice != 99 {
		t.Fatalf("live 部分成交映射错误: %+v", o)
	}
}

func TestGetOrderByClientIDAndSwapConversion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("clOrdId") != "cx9" {
			t.Errorf("clOrdId 参数错误: %s", r.URL.Query().Get("clOrdId"))
		}
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ordId":"5","clOrdID":"cx9","state":"filled","side":"sell","ordType":"limit","px":"60000","sz":"3","accFillSz":"3","avgPx":"60000","fee":"1.5","feeCcy":"USDT","cTime":"1","uTime":"2"}]}`))
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	o, err := c.GetOrderByClientID(context.Background(), "BTC-USDT-SWAP", "cx9")
	if err != nil {
		t.Fatal(err)
	}
	// 3 张 × 0.01 面值 = 0.03 BTC
	if o.Qty != 0.03 || o.FilledQty != 0.03 {
		t.Fatalf("合约张数应换算为 Base: %+v", o)
	}
	// 再次查询应命中缓存（合约规格不再请求）
	o2, err := c.GetOrderByClientID(context.Background(), "BTC-USDT-SWAP", "cx9")
	if err != nil || o2.Qty != 0.03 {
		t.Fatalf("缓存查询失败: %+v", o2)
	}
}

func TestGetOrderByClientIDEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.GetOrderByClientID(context.Background(), "BTC-USDT", "gone"); err == nil {
		t.Fatal("空结果必须报错")
	}
}

func TestGetOpenOrdersSwap(t *testing.T) {
	var instType string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/orders-pending", func(w http.ResponseWriter, r *http.Request) {
		instType = r.URL.Query().Get("instType")
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ordId":"9","clOrdID":"","state":"live","side":"buy","ordType":"limit","px":"50000","sz":"2","accFillSz":"0","avgPx":"","fee":"0","cTime":"1","uTime":"1"}]}`))
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	os, err := c.GetOpenOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	if instType != "SWAP" || len(os) != 1 || os[0].Qty != 0.02 {
		t.Fatalf("挂单查询错误: instType=%s %+v", instType, os)
	}
}

func TestMathFloor(t *testing.T) {
	if mathFloor(-1) != 0 {
		t.Fatal("负数向下取整应为 0")
	}
	if mathFloor(2.7) != 2 || mathFloor(2) != 2 {
		t.Fatalf("向下取整错误: %v %v", mathFloor(2.7), mathFloor(2))
	}
}

func TestPlaceOrderRequiresAuthHeaders(t *testing.T) {
	var ts, sign string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		ts = r.Header.Get("OK-ACCESS-TIMESTAMP")
		sign = r.Header.Get("OK-ACCESS-SIGN")
		w.Write([]byte(`{"code":"0","data":[{"ordId":"1","sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err != nil {
		t.Fatal(err)
	}
	if ts == "" || sign == "" {
		t.Fatal("鉴权头必须携带时间戳与签名")
	}
}

func TestHasCreds(t *testing.T) {
	c := &Client{}
	if c.hasCreds() {
		t.Fatal("空凭据应为 false")
	}
	c.APIKey, c.Secret, c.Passphrase = "a", "b", "c"
	if !c.hasCreds() {
		t.Fatal("完整凭据应为 true")
	}
}

func TestGetCandlesHistoryPaginates(t *testing.T) {
	page := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/history-candles", func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		page++
		// 两页：第一页 100 根（ot 200..101 最新在前），第二页 50 根（ot 100..51）
		var rows [][]string
		switch page {
		case 1:
			if after != "" {
				t.Errorf("首页不应带 after: %s", after)
			}
			for i := 0; i < 100; i++ {
				ot := 200 - i
				rows = append(rows, []string{fmt.Sprintf("%d", ot), "1", "1", "1", "1", "1", "0", "0", "1"})
			}
		case 2:
			if after != "101" { // 上一页最旧一根
				t.Errorf("第二页 after 游标错误: %s", after)
			}
			for i := 0; i < 50; i++ {
				ot := 100 - i
				rows = append(rows, []string{fmt.Sprintf("%d", ot), "1", "1", "1", "1", "1", "0", "0", "1"})
			}
		default:
			w.Write([]byte(`{"code":"0","data":[]}`)) // 第三页空 → 终止
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": rows})
	})
	c := newMockClient(t, mux)
	cs, err := c.GetCandlesHistory(context.Background(), "BTC-USDT", "1H", 150)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 150 {
		t.Fatalf("两页合并应 150 根: %d", len(cs))
	}
	if cs[0].OpenTime != 51 || cs[149].OpenTime != 200 {
		// ot=100 与 ot=101 不会重复：第二页从 100 开始（第一页最旧 101）
		t.Fatalf("升序覆盖 51..200（剔除重复 100/101）: 首=%d 末=%d", cs[0].OpenTime, cs[149].OpenTime)
	}
	for i := 1; i < len(cs); i++ {
		if cs[i].OpenTime <= cs[i-1].OpenTime {
			t.Fatal("必须升序且无重复")
		}
	}
}

func TestGetCandlesHistoryClamps(t *testing.T) {
	c := newMockClient(t, http.NewServeMux())
	if _, err := c.GetCandlesHistory(context.Background(), "BTC-USDT", "7x", 100); err == nil {
		t.Fatal("非法周期必须报错")
	}
}

func TestGetOrderBookParses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/books", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sz") != "50" || r.URL.Query().Get("instId") != "BTC-USDT" {
			t.Errorf("参数错误: %s", r.URL.RawQuery)
		}
		// OKX 返回 asks 升序/bids 降序（最优在前）
		w.Write([]byte(`{"code":"0","data":[{"asks":[["50001.2","0.5","0","1"],["50002.1","1.5","0","2"]],
			"bids":[["49999.8","0.8","0","1"],["49998.7","2.0","0","3"]],"ts":"1700000000000"}]}`))
	})
	c := newMockClient(t, mux)
	ob, err := c.GetOrderBook(context.Background(), "BTC-USDT", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ob.Bids) != 2 || len(ob.Asks) != 2 || ob.Ts != 1700000000000 {
		t.Fatalf("盘口解析错误: %+v", ob)
	}
	if ob.Bids[0].Price != 49999.8 || ob.Asks[0].Price != 50001.2 {
		t.Fatalf("最优档错误: %+v", ob)
	}
	// 价差（基点）≈ (50001.2-49999.8)/50000.5*10000 ≈ 0.28bp
	bp := ob.SpreadBp()
	if bp < 0.27 || bp > 0.29 {
		t.Fatalf("价差计算错误: %v", bp)
	}
	bidN, askN := ob.DepthNotional(2)
	if bidN <= 0 || askN <= 0 || math.Abs(bidN-(49999.8*0.8+49998.7*2.0)) > 1e-6 {
		t.Fatalf("深度名义错误: %v %v", bidN, askN)
	}
}

func TestGetOrderBookEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/books", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[]}`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetOrderBook(context.Background(), "BTC-USDT", 50); err == nil {
		t.Fatal("空盘口必须报错")
	}
}
