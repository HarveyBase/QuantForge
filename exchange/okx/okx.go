// Package okx 实现 OKX V5 REST 适配器，一套代码支持现货与永续合约。
// paper 模式 = OKX demo trading（x-simulated-trading: 1 + 演示环境的 API Key），与实盘同构。
package okx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

const (
	defaultBaseURL  = "https://www.okx.com"
	envAPIKey       = "OKX_API_KEY"
	envSecret       = "OKX_SECRET"
	envPassphrase   = "OKX_PASSPHRASE"
	demoHeader      = "x-simulated-trading"
	credMissingHint = "OKX 交易接口需要 API Key（环境变量 %s/%s/%s；paper 模式请用 OKX 演示环境的 Key）"
)

// Client OKX REST 客户端。
type Client struct {
	BaseURL    string
	APIKey     string
	Secret     string
	Passphrase string
	Simulated  bool // true = demo trading（paper 模式）
	HTTP       *http.Client
	TdMode     string // 合约全仓 cross / 逐仓 isolated；现货固定 cash
	Leverage   float64
	cacheMu    sync.Mutex
	contracts  map[string]float64
}

// NewLive 生产环境客户端，Key 从环境变量读取。
func NewLive(tdMode string, leverage float64) *Client {
	return NewLiveWithURL(defaultBaseURL, tdMode, leverage)
}

func NewLiveWithURL(baseURL, tdMode string, leverage float64) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), TdMode: tdMode, Leverage: leverage,
		APIKey: os.Getenv(envAPIKey), Secret: os.Getenv(envSecret), Passphrase: os.Getenv(envPassphrase),
		HTTP: &http.Client{Timeout: 15 * time.Second}, contracts: map[string]float64{},
	}
}

// NewPaper 演示环境（demo trading）客户端：与实盘同一套代码，仅多一个模拟头。
func NewPaper(tdMode string, leverage float64) *Client {
	return NewPaperWithURL(defaultBaseURL, tdMode, leverage)
}

func NewPaperWithURL(baseURL, tdMode string, leverage float64) *Client {
	c := NewLiveWithURL(baseURL, tdMode, leverage)
	c.Simulated = true
	return c
}

// NewPublic 仅公开行情（无 Key 也能跑 research 数据面）。
func NewPublic() *Client                      { return NewPublicWithURL(defaultBaseURL) }
func NewPublicWithURL(baseURL string) *Client { return NewLiveWithURL(baseURL, "cross", 1) }

func (c *Client) HasCredentials() bool { return c.hasCreds() }

func (c *Client) Name() string { return "okx" }

func (c *Client) hasCreds() bool {
	return c.APIKey != "" && c.Secret != "" && c.Passphrase != ""
}

// ---- 请求基础设施 ----

func (c *Client) sign(ts, method, path, body string) string {
	prehash := ts + method + path + body
	mac := hmac.New(sha256.New, []byte(c.Secret))
	mac.Write([]byte(prehash))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, needAuth bool) ([]byte, error) {
	if query != nil && len(query) > 0 {
		path += "?" + query.Encode()
	}
	var reqBody []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("okx: 序列化请求失败: %w", err)
		}
		reqBody = b
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if needAuth {
		if !c.hasCreds() {
			return nil, fmt.Errorf("okx: %s", fmt.Sprintf(credMissingHint, envAPIKey, envSecret, envPassphrase))
		}
		ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		req.Header.Set("OK-ACCESS-KEY", c.APIKey)
		req.Header.Set("OK-ACCESS-SIGN", c.sign(ts, method, path, string(reqBody)))
		req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
		req.Header.Set("OK-ACCESS-PASSPHRASE", c.Passphrase)
		if c.Simulated {
			req.Header.Set(demoHeader, "1")
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("okx: 请求失败 %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("okx: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("okx: %s HTTP %d: %.200s", path, resp.StatusCode, data)
	}
	var env struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("okx: 解析响应失败: %w", err)
	}
	if env.Code != "0" {
		return nil, fmt.Errorf("okx: %s 业务错误 code=%s msg=%s", path, env.Code, env.Msg)
	}
	return data, nil
}

func toFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func toInt64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// ---- 行情（公开） ----

var intervalMap = map[string]string{
	"1m": "1m", "3m": "3m", "5m": "5m", "15m": "15m", "30m": "30m",
	"1H": "1H", "2H": "2H", "4H": "4H", "6H": "6H", "12H": "12H", "1D": "1D",
}

func (c *Client) GetCandles(ctx context.Context, symbol, interval string, limit int) ([]exchange.Candle, error) {
	bar, ok := intervalMap[interval]
	if !ok {
		return nil, fmt.Errorf("okx: 不支持的 K 线周期 %q", interval)
	}
	if limit <= 0 || limit > 300 {
		limit = 300 // /market/candles 上限 300；历史分页用 /history-candles，二期需要再接
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v5/market/candles",
		url.Values{"instId": {symbol}, "bar": {bar}, "limit": {strconv.Itoa(limit)}}, nil, false)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data [][]string `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("okx: 解析 K 线失败: %w", err)
	}
	out := make([]exchange.Candle, 0, len(env.Data))
	for _, row := range env.Data {
		if len(row) < 6 {
			continue
		}
		out = append(out, exchange.Candle{
			Exchange:  "okx",
			Symbol:    symbol,
			Interval:  interval,
			OpenTime:  toInt64(row[0]),
			Open:      toFloat(row[1]),
			High:      toFloat(row[2]),
			Low:       toFloat(row[3]),
			Close:     toFloat(row[4]),
			Volume:    toFloat(row[5]),
			Confirmed: len(row) >= 9 && row[8] == "1",
		})
	}
	// OKX 返回最新在前，统一为时间升序
	sort.Slice(out, func(i, j int) bool { return out[i].OpenTime < out[j].OpenTime })
	return out, nil
}

func (c *Client) GetTicker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v5/market/ticker",
		url.Values{"instId": {symbol}}, nil, false)
	if err != nil {
		return exchange.Ticker{}, err
	}
	var env struct {
		Data []struct {
			InstID string `json:"instId"`
			Last   string `json:"last"`
			BidPx  string `json:"bidPx"`
			AskPx  string `json:"askPx"`
			Ts     string `json:"ts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Data) == 0 {
		return exchange.Ticker{}, fmt.Errorf("okx: 解析 ticker 失败: %v", err)
	}
	d := env.Data[0]
	return exchange.Ticker{Symbol: symbol, Last: toFloat(d.Last), Bid: toFloat(d.BidPx), Ask: toFloat(d.AskPx), Ts: toInt64(d.Ts)}, nil
}

func (c *Client) GetInstrument(ctx context.Context, instID string) (exchange.Instrument, error) {
	instType := "SPOT"
	if strings.HasSuffix(instID, "-SWAP") {
		instType = "SWAP"
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v5/public/instruments",
		url.Values{"instType": {instType}, "instId": {instID}}, nil, false)
	if err != nil {
		return exchange.Instrument{}, err
	}
	var env struct {
		Data []struct {
			InstID   string `json:"instId"`
			CtVal    string `json:"ctVal"`
			LotSz    string `json:"lotSz"`
			MinSz    string `json:"minSz"`
			TickSz   string `json:"tickSz"`
			Settle   string `json:"settleCcy"`
			BaseCcy  string `json:"baseCcy"`
			QuoteCcy string `json:"quoteCcy"`
			CtType   string `json:"ctType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Data) == 0 {
		return exchange.Instrument{}, fmt.Errorf("okx: 解析合约规格失败: %v", err)
	}
	d := env.Data[0]
	ins := exchange.Instrument{
		Exchange: "okx", InstID: d.InstID, Market: exchange.Market(instType),
		ContractSize: toFloat(d.CtVal), LotSize: toFloat(d.LotSz),
		MinSize: toFloat(d.MinSz), TickSize: toFloat(d.TickSz),
	}
	if instType == "SPOT" {
		ins.Base, ins.Quote = d.BaseCcy, d.QuoteCcy
	} else {
		parts := strings.Split(strings.TrimSuffix(d.InstID, "-SWAP"), "-")
		ins.Base, ins.Quote = parts[0], parts[1]
	}
	return ins, nil
}

// ---- 交易（鉴权） ----

// tdMode 下单模式：现货 cash，合约取配置（cross/isolated）。
func (c *Client) tdModeFor(market string) string {
	if market == "SWAP" || strings.HasSuffix(market, "-SWAP") {
		return c.TdMode
	}
	return "cash"
}

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v5/account/balance", nil, nil, true)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []struct {
			Details []struct {
				Ccy       string `json:"ccy"`
				AvailBal  string `json:"availBal"`
				CashBal   string `json:"cashBal"`
				FrozenBal string `json:"frozenBal"`
			} `json:"details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Data) == 0 {
		return nil, fmt.Errorf("okx: 解析余额失败: %v", err)
	}
	out := make([]exchange.Balance, 0, len(env.Data[0].Details))
	for _, d := range env.Data[0].Details {
		avail := toFloat(d.AvailBal)
		if avail == 0 {
			avail = toFloat(d.CashBal) - toFloat(d.FrozenBal)
		}
		out = append(out, exchange.Balance{
			Asset: d.Ccy, Total: toFloat(d.CashBal), Available: avail,
		})
	}
	return out, nil
}

// PlaceOrder 下单。数量换算：现货直接用 Base 数量；永续按张数 = qty / 面值 取整。
func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	isSwap := strings.HasSuffix(req.Symbol, "-SWAP")
	sz := req.Qty
	if isSwap {
		ins, err := c.GetInstrument(ctx, req.Symbol)
		if err != nil {
			return exchange.Order{}, fmt.Errorf("okx: 查询合约规格失败: %w", err)
		}
		if ins.ContractSize <= 0 {
			return exchange.Order{}, fmt.Errorf("okx: 合约面值非法 %v", ins.ContractSize)
		}
		sz = mathFloor(req.Qty / ins.ContractSize)
		if sz < 1 {
			return exchange.Order{}, fmt.Errorf("okx: 数量 %v 不足 1 张合约（面值 %v）", req.Qty, ins.ContractSize)
		}
	}
	body := map[string]string{
		"instId":  req.Symbol,
		"tdMode":  c.tdModeFor(req.Symbol),
		"side":    string(req.Side),
		"sz":      strconv.FormatFloat(sz, 'f', -1, 64),
		"clOrdID": req.ClientOrderID,
	}
	switch req.Type {
	case exchange.OrderLimit:
		body["ordType"] = "limit"
		body["px"] = strconv.FormatFloat(req.Price, 'f', -1, 64)
	case exchange.OrderMarket:
		body["ordType"] = "market"
	}
	if isSwap {
		body["posSide"] = "net" // 单向持仓模式
	}
	data, err := c.do(ctx, http.MethodPost, "/api/v5/trade/order", nil, body, true)
	if err != nil {
		return exchange.Order{}, err
	}
	var env struct {
		Data []struct {
			OrdID   string `json:"ordId"`
			ClOrdID string `json:"clOrdID"`
			SCode   string `json:"sCode"`
			SMsg    string `json:"sMsg"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Data) == 0 {
		return exchange.Order{}, fmt.Errorf("okx: 解析下单响应失败: %v", err)
	}
	d := env.Data[0]
	if d.SCode != "0" {
		return exchange.Order{}, fmt.Errorf("okx: 下单被拒 sCode=%s sMsg=%s", d.SCode, d.SMsg)
	}
	return exchange.Order{
		Exchange: "okx", Symbol: req.Symbol, OrderID: d.OrdID, ClientOrderID: d.ClOrdID,
		Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty,
		Status: exchange.StatusSubmitted, CreatedAt: time.Now().UnixMilli(),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	body := map[string]string{"instId": symbol, "ordId": orderID}
	_, err := c.do(ctx, http.MethodPost, "/api/v5/trade/cancel-order", nil, body, true)
	return err
}

func (c *Client) GetOrder(ctx context.Context, symbol, orderID string) (exchange.Order, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v5/trade/order",
		url.Values{"instId": {symbol}, "ordId": {orderID}}, nil, true)
	if err != nil {
		return exchange.Order{}, err
	}
	var env struct {
		Data []okxOrder `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Data) == 0 {
		return exchange.Order{}, fmt.Errorf("okx: 解析订单失败: %v", err)
	}
	return c.convert(env.Data[0]), nil
}

func (c *Client) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (exchange.Order, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v5/trade/order",
		url.Values{"instId": {symbol}, "clOrdId": {clientOrderID}}, nil, true)
	if err != nil {
		return exchange.Order{}, err
	}
	var env struct {
		Data []okxOrder `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return exchange.Order{}, fmt.Errorf("okx: 解析订单失败: %w", err)
	}
	if len(env.Data) == 0 {
		return exchange.Order{}, fmt.Errorf("okx: 按 clientOrderID %s 查询订单为空", clientOrderID)
	}
	return c.convert(env.Data[0]), nil
}

func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]exchange.Order, error) {
	instType := "SPOT"
	if strings.HasSuffix(symbol, "-SWAP") {
		instType = "SWAP"
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v5/trade/orders-pending",
		url.Values{"instType": {instType}, "instId": {symbol}}, nil, true)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []okxOrder `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("okx: 解析挂单失败: %w", err)
	}
	out := make([]exchange.Order, 0, len(env.Data))
	for _, o := range env.Data {
		out = append(out, c.convert(o))
	}
	return out, nil
}

type okxOrder struct {
	InstID    string `json:"instId"`
	OrdID     string `json:"ordId"`
	ClOrdID   string `json:"clOrdID"`
	State     string `json:"state"` // live | partially_filled | filled | canceled
	Side      string `json:"side"`
	OrdType   string `json:"ordType"`
	Px        string `json:"px"`
	Sz        string `json:"sz"` // 委托数量（现货=Base，合约=张）
	AccFillSz string `json:"accFillSz"`
	AvgPx     string `json:"avgPx"`
	Fee       string `json:"fee"`
	FeeCcy    string `json:"feeCcy"`
	CTime     string `json:"cTime"`
	UTime     string `json:"uTime"`
}

func (c *Client) convert(o okxOrder) exchange.Order {
	status := exchange.StatusSubmitted
	switch o.State {
	case "filled":
		status = exchange.StatusFilled
	case "canceled":
		status = exchange.StatusCancelled
	case "partially_filled":
		status = exchange.StatusPartiallyFilled
	case "live":
		if toFloat(o.AccFillSz) > 0 {
			status = exchange.StatusPartiallyFilled
		}
	}
	sz := toFloat(o.Sz)
	fill := toFloat(o.AccFillSz)
	if strings.HasSuffix(o.InstID, "-SWAP") && c.contractSizeOf(o.InstID) > 0 {
		// 张 → Base
		sz *= c.contractSizeOf(o.InstID)
		fill *= c.contractSizeOf(o.InstID)
	}
	return exchange.Order{
		Exchange: "okx", Symbol: o.InstID, OrderID: o.OrdID, ClientOrderID: o.ClOrdID,
		Side: exchange.Side(o.Side), Type: exchange.OrderType(o.OrdType),
		Price: toFloat(o.Px), Qty: sz, FilledQty: fill, AvgPrice: toFloat(o.AvgPx),
		Fee: toFloat(o.Fee), FeeCcy: o.FeeCcy, Status: status,
		CreatedAt: toInt64(o.CTime), UpdatedAt: toInt64(o.UTime),
	}
}

// contractSizeOf 使用客户端级缓存，避免跨客户端污染和并发 map 竞态。
func (c *Client) contractSizeOf(instID string) float64 {
	c.cacheMu.Lock()
	if c.contracts == nil {
		c.contracts = map[string]float64{}
	}
	if v, ok := c.contracts[instID]; ok {
		c.cacheMu.Unlock()
		return v
	}
	c.cacheMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ins, err := c.GetInstrument(ctx, instID)
	if err != nil || ins.ContractSize == 0 {
		return 0
	}
	c.cacheMu.Lock()
	c.contracts[instID] = ins.ContractSize
	c.cacheMu.Unlock()
	return ins.ContractSize
}

func mathFloor(v float64) float64 {
	if v < 0 {
		return 0
	}
	return float64(int64(v))
}

// GetCandlesHistory 分页拉取长历史（/api/v5/market/history-candles，单页 100，after 游标向前翻）。
// total 为目标根数（>1000 时自动多页合并去重）；返回时间升序、可能含未收盘尾部。
func (c *Client) GetCandlesHistory(ctx context.Context, symbol, interval string, total int) ([]exchange.Candle, error) {
	bar, ok := intervalMap[interval]
	if !ok {
		return nil, fmt.Errorf("okx: 不支持的 K 线周期 %q", interval)
	}
	if total <= 0 {
		total = 1000
	}
	if total > 20000 {
		total = 20000 // 防误配打爆 API 配额
	}
	var collected []exchange.Candle
	seen := map[string]bool{}
	// history-candles 用 after=ts 返回"该时间之前"的数据（最新在前）；
	// 首页不带 after，之后用上一页最旧一根的 OpenTime 作游标。
	var after int64
	for len(collected) < total {
		q := url.Values{"instId": {symbol}, "bar": {bar}, "limit": {"100"}}
		if after > 0 {
			q.Set("after", strconv.FormatInt(after, 10))
		}
		data, err := c.do(ctx, http.MethodGet, "/api/v5/market/history-candles", q, nil, false)
		if err != nil {
			return nil, err
		}
		var env struct {
			Data [][]string `json:"data"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("okx: 解析历史 K 线失败: %w", err)
		}
		if len(env.Data) == 0 {
			break // 交易所历史尽头
		}
		oldest := int64(1<<62 - 1)
		for _, row := range env.Data {
			if len(row) < 6 {
				continue
			}
			ot := toInt64(row[0])
			key := strconv.FormatInt(ot, 10)
			if seen[key] {
				continue
			}
			seen[key] = true
			collected = append(collected, exchange.Candle{
				Exchange: "okx", Symbol: symbol, Interval: interval,
				OpenTime: ot, Open: toFloat(row[1]), High: toFloat(row[2]),
				Low: toFloat(row[3]), Close: toFloat(row[4]), Volume: toFloat(row[5]),
				Confirmed: len(row) >= 9 && row[8] == "1",
			})
			if ot < oldest {
				oldest = ot
			}
		}
		if oldest == 1<<62-1 || oldest <= 0 {
			break
		}
		after = oldest
		// 游标未推进（页内全部重复）则终止，防死循环
		if after == 0 {
			break
		}
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i].OpenTime < collected[j].OpenTime })
	if len(collected) > total {
		collected = collected[len(collected)-total:] // 保最新 total 根
	}
	return collected, nil
}
