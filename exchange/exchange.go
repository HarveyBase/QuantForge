// Package exchange 定义交易所统一抽象：行情、合约规格、订单生命周期。
// 三级执行模式（research/paper/live）共用该抽象，live 只替换适配器实例，风控路径同构。
package exchange

import (
	"context"
	"errors"
	"strconv"
)

// ErrNotImplemented 二期适配器占位。
var ErrNotImplemented = errors.New("exchange: 该适配器尚未实现")

// Market 交易品种类型。
type Market string

const (
	MarketSPOT Market = "SPOT"
	MarketSWAP Market = "SWAP" // 永续合约
)

// Side 方向。
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// OrderType 订单类型。
type OrderType string

const (
	OrderLimit  OrderType = "limit"
	OrderMarket OrderType = "market"
)

// OrderStatus 订单状态机：
// New → Submitted → PartiallyFilled → Filled / Cancelled / Rejected / Expired
type OrderStatus string

const (
	StatusNew             OrderStatus = "new"
	StatusSubmitted       OrderStatus = "submitted"
	StatusPartiallyFilled OrderStatus = "partially_filled"
	StatusFilled          OrderStatus = "filled"
	StatusCancelled       OrderStatus = "cancelled"
	StatusRejected        OrderStatus = "rejected"
	StatusExpired         OrderStatus = "expired"
)

// Terminal 判断订单是否已进入终态（不再变化）。
func (s OrderStatus) Terminal() bool {
	switch s {
	case StatusFilled, StatusCancelled, StatusRejected, StatusExpired:
		return true
	}
	return false
}

// Instrument 合约/交易对规格。
type Instrument struct {
	Exchange     string  // 所属交易所
	InstID       string  // BTC-USDT / BTC-USDT-SWAP
	Market       Market  // SPOT / SWAP
	Base         string  // BTC
	Quote        string  // USDT
	ContractSize float64 // 合约面值（SWAP 用，1 张 = ContractSize 个 Base）
	LotSize      float64 // 数量步长
	MinSize      float64 // 最小下单量
	TickSize     float64 // 价格步长
	MinNotional  float64 // 最小名义价值（USDT）
}

// Candle K 线。唯一键 = Exchange|Symbol|Interval|OpenTime。
type Candle struct {
	Exchange  string  `json:"exchange"`
	Symbol    string  `json:"symbol"`
	Interval  string  `json:"interval"`
	OpenTime  int64   `json:"open_time"` // 毫秒
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    float64 `json:"volume"`
	Confirmed bool    `json:"confirmed"` // 未收盘 K 线只可展示，禁入指标与回测
}

// Key K 线唯一键。
func (c Candle) Key() string {
	return c.Exchange + "|" + c.Symbol + "|" + c.Interval + "|" + strconv.FormatInt(c.OpenTime, 10)
}

// Ticker 最新成交与盘口。
type Ticker struct {
	Symbol string  `json:"symbol"`
	Last   float64 `json:"last"`
	Bid    float64 `json:"bid"`
	Ask    float64 `json:"ask"`
	Ts     int64   `json:"ts"`
}

// Mid 盘口中间价。
func (t Ticker) Mid() float64 {
	if t.Bid > 0 && t.Ask > 0 {
		return (t.Bid + t.Ask) / 2
	}
	return t.Last
}

// SpreadPct 买卖价差百分比（风控看门狗用）。
func (t Ticker) SpreadPct() float64 {
	if t.Bid <= 0 || t.Ask <= 0 {
		return 0
	}
	return (t.Ask - t.Bid) / t.Mid() * 100
}

// OrderRequest 下单请求。
type OrderRequest struct {
	Symbol        string    `json:"symbol"`
	Side          Side      `json:"side"`
	Type          OrderType `json:"type"`
	Price         float64   `json:"price"`           // limit 必填
	Qty           float64   `json:"qty"`             // Base 数量（适配器内部换算张数）
	ClientOrderID string    `json:"client_order_id"` // 幂等键
}

// Order 订单（含成交回报）。
type Order struct {
	Exchange      string      `json:"exchange"`
	Symbol        string      `json:"symbol"`
	OrderID       string      `json:"order_id"`
	ClientOrderID string      `json:"client_order_id"`
	Side          Side        `json:"side"`
	Type          OrderType   `json:"type"`
	Price         float64     `json:"price"`
	Qty           float64     `json:"qty"`        // 委托数量（Base）
	FilledQty     float64     `json:"filled_qty"` // 已成交数量（Base）
	AvgPrice      float64     `json:"avg_price"`
	Fee           float64     `json:"fee"` // 累计手续费（带符号，负=已支付）
	FeeCcy        string      `json:"fee_ccy"`
	Status        OrderStatus `json:"status"`
	CreatedAt     int64       `json:"created_at"`
	UpdatedAt     int64       `json:"updated_at"`
}

// Notional 委打名义价值（按委托价估算，市价单用 AvgPrice）。
func (o Order) Notional() float64 {
	p := o.Price
	if p == 0 {
		p = o.AvgPrice
	}
	return p * o.Qty
}

// Fill 单笔增量成交。Price/Fee 均为本次增量口径，不是订单累计值。
type Fill struct {
	Symbol        string  `json:"symbol"`
	ClientOrderID string  `json:"client_order_id"`
	Side          Side    `json:"side"`
	Qty           float64 `json:"qty"`
	Price         float64 `json:"price"`
	Fee           float64 `json:"fee"` // 带符号，负=已支付
	FeeCcy        string  `json:"fee_ccy"`
	Ts            int64   `json:"ts"`
}

// Balance 资产余额。
type Balance struct {
	Asset     string  `json:"asset"`
	Total     float64 `json:"total"`
	Available float64 `json:"available"` // 可用（冻结剔除），卖出校验用它
}

// Exchange 交易所统一接口。
// 实现方：okx（live/paper-demo）、paper（本地模拟，测试替身）、binance（占位）。
type Exchange interface {
	Name() string

	// 行情（公开，无需鉴权）
	GetCandles(ctx context.Context, symbol, interval string, limit int) ([]Candle, error)
	GetTicker(ctx context.Context, symbol string) (Ticker, error)
	GetInstrument(ctx context.Context, instID string) (Instrument, error)

	// 交易（需要 API Key；paper 模式用 OKX demo trading）
	GetBalances(ctx context.Context) ([]Balance, error)
	PlaceOrder(ctx context.Context, req OrderRequest) (Order, error)
	CancelOrder(ctx context.Context, symbol, orderID string) error
	GetOrder(ctx context.Context, symbol, orderID string) (Order, error)
	GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (Order, error)
	GetOpenOrders(ctx context.Context, symbol string) ([]Order, error)
}
