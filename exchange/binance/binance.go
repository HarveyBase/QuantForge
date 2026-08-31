// Package binance Binance 现货适配器（REST /api/v3，HMAC SHA256 签名）。
// 接口口径与 okx 对齐；合约（/fapi）二期。K 线唯一键 exchange="binance"。
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

const (
	defaultBaseURL = "https://api.binance.com"
	envAPIKey      = "BINANCE_API_KEY"
	envSecret      = "BINANCE_SECRET"
)

// Client Binance 现货客户端。
type Client struct {
	BaseURL string
	APIKey  string
	Secret  string
	HTTP    *http.Client
}

// New 构造（Key 走环境变量 BINANCE_API_KEY / BINANCE_SECRET）。
func New() *Client { return NewWithURL(defaultBaseURL) }

// NewWithURL 自定义 base URL（测试注入 mock）。
func NewWithURL(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  os.Getenv(envAPIKey),
		Secret:  os.Getenv(envSecret),
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() string { return "binance" }

func (c *Client) hasCreds() bool { return c.APIKey != "" && c.Secret != "" }

// sign HMAC SHA256(query)。
func (c *Client) sign(query string) string {
	mac := hmac.New(sha256.New, []byte(c.Secret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// do 发请求。signed=true 时追加 timestamp+recvWindow 并签名（query 签名）。
func (c *Client) do(ctx context.Context, method, path string, query url.Values, signed bool) ([]byte, error) {
	if signed {
		if !c.hasCreds() {
			return nil, fmt.Errorf("binance: 签名接口需要 API Key（环境变量 %s/%s）", envAPIKey, envSecret)
		}
		if query == nil {
			query = url.Values{}
		}
		query.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
		query.Set("recvWindow", "5000")
	}
	var full string
	if len(query) > 0 {
		full = c.BaseURL + path + "?" + query.Encode()
	} else {
		full = c.BaseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if signed {
		req.Header.Set("X-MBX-APIKEY", c.APIKey)
		q := req.URL.Query().Encode()
		req.URL.RawQuery = q + "&signature=" + c.sign(q)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance: 请求失败 %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("binance: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance: %s HTTP %d: %.200s", path, resp.StatusCode, data)
	}
	return data, nil
}

var intervalMap = map[string]string{
	"1m": "1m", "5m": "5m", "15m": "15m", "30m": "30m",
	"1H": "1h", "4H": "4h", "1D": "1d",
}

func toFloat(s interface{}) float64 {
	switch v := s.(type) {
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case float64:
		return v
	}
	return 0
}

func toInt64(s interface{}) int64 {
	switch v := s.(type) {
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	case float64:
		return int64(v)
	}
	return 0
}

func toBinanceSymbol(instID string) string {
	return strings.ReplaceAll(strings.ToUpper(instID), "-", "")
}

// GetCandles K 线（klines 数组：[openTime, o,h,l,c, vol, closeTime, ...]）。
func (c *Client) GetCandles(ctx context.Context, symbol, interval string, limit int) ([]exchange.Candle, error) {
	bi, ok := intervalMap[interval]
	if !ok {
		return nil, fmt.Errorf("binance: 不支持的周期 %q", interval)
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v3/klines",
		url.Values{"symbol": {toBinanceSymbol(symbol)}, "interval": {bi}, "limit": {strconv.Itoa(limit)}}, false)
	if err != nil {
		return nil, err
	}
	var rows [][]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("binance: 解析 K 线失败: %w", err)
	}
	out := make([]exchange.Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		out = append(out, exchange.Candle{
			Exchange: "binance", Symbol: symbol, Interval: interval,
			OpenTime: toInt64(r[0]), Open: toFloat(r[1]), High: toFloat(r[2]),
			Low: toFloat(r[3]), Close: toFloat(r[4]), Volume: toFloat(r[5]),
			Confirmed: true,
		})
	}
	return out, nil
}

// GetTicker 最优挂价。
func (c *Client) GetTicker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v3/ticker/bookTicker",
		url.Values{"symbol": {toBinanceSymbol(symbol)}}, false)
	if err != nil {
		return exchange.Ticker{}, err
	}
	var t struct {
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return exchange.Ticker{}, fmt.Errorf("binance: 解析 ticker 失败: %w", err)
	}
	return exchange.Ticker{Symbol: symbol, Bid: toFloat(t.BidPrice), Ask: toFloat(t.AskPrice)}, nil
}

// GetOrderBook 盘口（bids 降序 / asks 升序，最优在前）。
func (c *Client) GetOrderBook(ctx context.Context, symbol string, depth int) (exchange.OrderBook, error) {
	if depth <= 0 || depth > 100 {
		depth = 20
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v3/depth",
		url.Values{"symbol": {toBinanceSymbol(symbol)}, "limit": {strconv.Itoa(depth)}}, false)
	if err != nil {
		return exchange.OrderBook{}, err
	}
	var env struct {
		Bids [][]string `json:"bids"`
		Asks [][]string `json:"asks"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return exchange.OrderBook{}, fmt.Errorf("binance: 解析盘口失败: %w", err)
	}
	ob := exchange.OrderBook{Symbol: symbol}
	for _, lv := range env.Bids {
		if len(lv) >= 2 {
			ob.Bids = append(ob.Bids, exchange.OrderBookLevel{Price: toFloat(lv[0]), Qty: toFloat(lv[1])})
		}
	}
	for _, lv := range env.Asks {
		if len(lv) >= 2 {
			ob.Asks = append(ob.Asks, exchange.OrderBookLevel{Price: toFloat(lv[0]), Qty: toFloat(lv[1])})
		}
	}
	return ob, nil
}

// GetInstrument 交易对规格（exchangeInfo）。
func (c *Client) GetInstrument(ctx context.Context, instID string) (exchange.Instrument, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v3/exchangeInfo",
		url.Values{"symbol": {toBinanceSymbol(instID)}}, false)
	if err != nil {
		return exchange.Instrument{}, err
	}
	var env struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []struct {
				FilterType  string `json:"filterType"`
				StepSize    string `json:"stepSize"`
				MinQty      string `json:"minQty"`
				TickSize    string `json:"tickSize"`
				MinNotional string `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Symbols) == 0 {
		return exchange.Instrument{}, fmt.Errorf("binance: 解析交易对失败: %v", err)
	}
	s := env.Symbols[0]
	ins := exchange.Instrument{
		Exchange: "binance", InstID: instID, Market: exchange.MarketSPOT,
		Base: s.BaseAsset, Quote: s.QuoteAsset,
	}
	for _, f := range s.Filters {
		switch f.FilterType {
		case "LOT_SIZE":
			ins.LotSize, ins.MinSize = toFloat(f.StepSize), toFloat(f.MinQty)
		case "PRICE_FILTER":
			ins.TickSize = toFloat(f.TickSize)
		case "NOTIONAL":
			ins.MinNotional = toFloat(f.MinNotional)
		}
	}
	return ins, nil
}

// GetBalances 账户余额（签名）。
func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v3/account", nil, true)
	if err != nil {
		return nil, err
	}
	var env struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("binance: 解析余额失败: %w", err)
	}
	var out []exchange.Balance
	for _, b := range env.Balances {
		free := toFloat(b.Free)
		locked := toFloat(b.Locked)
		if free+locked <= 0 {
			continue
		}
		out = append(out, exchange.Balance{Asset: b.Asset, Total: free + locked, Available: free})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset < out[j].Asset })
	return out, nil
}

// PlaceOrder 下单（LIMIT 用 GTC；MARKET 按市价）。
func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	q := url.Values{
		"symbol":           {toBinanceSymbol(req.Symbol)},
		"side":             {strings.ToUpper(string(req.Side))},
		"type":             {strings.ToUpper(string(req.Type))},
		"quantity":         {strconv.FormatFloat(req.Qty, 'f', -1, 64)},
		"newClientOrderId": {req.ClientOrderID},
	}
	switch req.Type {
	case exchange.OrderLimit:
		q.Set("timeInForce", "GTC")
		q.Set("price", strconv.FormatFloat(req.Price, 'f', -1, 64))
	case exchange.OrderMarket:
	default:
		return exchange.Order{}, fmt.Errorf("binance: 不支持的订单类型 %q", req.Type)
	}
	data, err := c.do(ctx, http.MethodPost, "/api/v3/order", q, true)
	if err != nil {
		return exchange.Order{}, err
	}
	var o struct {
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return exchange.Order{}, fmt.Errorf("binance: 解析下单回执失败: %w", err)
	}
	return exchange.Order{
		Exchange: "binance", Symbol: req.Symbol, OrderID: strconv.FormatInt(o.OrderID, 10),
		ClientOrderID: o.ClientOrderID, Side: req.Side, Type: req.Type,
		Price: req.Price, Qty: req.Qty, Status: mapStatus(o.Status),
	}, nil
}

func mapStatus(s string) exchange.OrderStatus {
	switch s {
	case "PARTIALLY_FILLED":
		return exchange.StatusPartiallyFilled
	case "NEW":
		return exchange.StatusSubmitted
	case "FILLED":
		return exchange.StatusFilled
	case "CANCELED", "CANCELLED", "EXPIRED", "REJECTED":
		return exchange.StatusCancelled
	}
	return exchange.StatusNew
}

// CancelOrder 撤单。
func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v3/order",
		url.Values{"symbol": {toBinanceSymbol(symbol)}, "orderId": {orderID}}, true)
	return err
}

// GetOrder 查单（orderId）。
func (c *Client) GetOrder(ctx context.Context, symbol, orderID string) (exchange.Order, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v3/order",
		url.Values{"symbol": {toBinanceSymbol(symbol)}, "orderId": {orderID}}, true)
	if err != nil {
		return exchange.Order{}, err
	}
	return parseOrder(data, symbol)
}

// GetOrderByClientID 查单（clientOrderId）。
func (c *Client) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (exchange.Order, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v3/order",
		url.Values{"symbol": {toBinanceSymbol(symbol)}, "origClientOrderId": {clientOrderID}}, true)
	if err != nil {
		return exchange.Order{}, err
	}
	return parseOrder(data, symbol)
}

func parseOrder(data []byte, symbol string) (exchange.Order, error) {
	var o struct {
		OrderID             int64  `json:"orderId"`
		ClientOrderID       string `json:"clientOrderId"`
		Side                string `json:"side"`
		Type                string `json:"type"`
		Price               string `json:"price"`
		OrigQty             string `json:"origQty"`
		ExecutedQty         string `json:"executedQty"`
		CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
		Status              string `json:"status"`
		Time                int64  `json:"time"`
		UpdateTime          int64  `json:"updateTime"`
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return exchange.Order{}, fmt.Errorf("binance: 解析订单失败: %w", err)
	}
	avg := 0.0
	if executed := toFloat(o.ExecutedQty); executed > 0 {
		avg = toFloat(o.CummulativeQuoteQty) / executed
	}
	return exchange.Order{
		Exchange: "binance", Symbol: symbol, OrderID: strconv.FormatInt(o.OrderID, 10),
		ClientOrderID: o.ClientOrderID, Side: exchange.Side(strings.ToLower(o.Side)),
		Type:  exchange.OrderType(strings.ToLower(o.Type)),
		Price: toFloat(o.Price), Qty: toFloat(o.OrigQty), FilledQty: toFloat(o.ExecutedQty),
		AvgPrice: avg, Status: mapStatus(o.Status),
		CreatedAt: o.Time, UpdatedAt: o.UpdateTime,
	}, nil
}

// GetOpenOrders 当前挂单。
func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]exchange.Order, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v3/openOrders",
		url.Values{"symbol": {toBinanceSymbol(symbol)}}, true)
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	out := make([]exchange.Order, 0, len(raws))
	for _, raw := range raws {
		if o, err := parseOrder(raw, symbol); err == nil {
			out = append(out, o)
		}
	}
	return out, nil
}
