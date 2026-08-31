// Package paper 本地模拟撮合交易所：无网络的执行层测试替身（OKX demo trading 之外的离线兜底）。
// 行为约定与 docs/08 一致：限价单按价格穿越成交、市价单按滑点成交、部分成交可配置。
package paper

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

// FillModel 撮合模型参数。
type FillModel struct {
	SlippageBps  float64 // 市价单滑点（基点）
	PartialRatio float64 // 限价单触及后成交比例（1=全成）
	FeeBps       float64 // taker 手续费（基点）
}

// Exchange 内存撮合的模拟交易所（仅现货；合约模拟走 OKX demo）。
type Exchange struct {
	mu       sync.Mutex
	last     float64
	balances map[string]float64 // asset -> available
	open     map[string]exchange.Order
	all      map[string]exchange.Order
	byClient map[string]string
	seq      int
	fill     FillModel
	nowFn    func() int64
}

// New 创建模拟交易所；seedPrice 为初始价格，seedCash 为初始 USDT。
func New(seedPrice, seedCash float64, model FillModel) *Exchange {
	if model.PartialRatio <= 0 || model.PartialRatio > 1 {
		model.PartialRatio = 1
	}
	return &Exchange{
		last: seedPrice, balances: map[string]float64{"USDT": seedCash},
		open: map[string]exchange.Order{}, all: map[string]exchange.Order{},
		byClient: map[string]string{},
		fill:     model, nowFn: func() int64 { return time.Now().UnixMilli() },
	}
}

func (e *Exchange) Name() string { return "paper-local" }

func (e *Exchange) GetCandles(ctx context.Context, symbol, interval string, limit int) ([]exchange.Candle, error) {
	return nil, fmt.Errorf("paper: 本地模拟不提供 K 线，请从 market 包拉真实数据")
}

func (e *Exchange) GetTicker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last <= 0 {
		return exchange.Ticker{}, fmt.Errorf("paper: 价格未初始化")
	}
	half := e.last * 0.0001
	return exchange.Ticker{Symbol: symbol, Last: e.last, Bid: e.last - half, Ask: e.last + half, Ts: e.nowFn()}, nil
}

// GetOrderBook 本地合成盘口：围绕 last 对称各 10 档（每档 0.5 个最小变动、1 BTC）。
func (e *Exchange) GetOrderBook(ctx context.Context, symbol string, depth int) (exchange.OrderBook, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last <= 0 {
		return exchange.OrderBook{}, fmt.Errorf("paper: 价格未初始化")
	}
	if depth <= 0 || depth > 10 {
		depth = 10
	}
	ob := exchange.OrderBook{Symbol: symbol, Ts: e.nowFn()}
	step := e.last * 0.0001
	for i := 0; i < depth; i++ {
		ob.Bids = append(ob.Bids, exchange.OrderBookLevel{Price: e.last - step*float64(i+1), Qty: 1})
		ob.Asks = append(ob.Asks, exchange.OrderBookLevel{Price: e.last + step*float64(i+1), Qty: 1})
	}
	return ob, nil
}

func (e *Exchange) GetInstrument(ctx context.Context, instID string) (exchange.Instrument, error) {
	return exchange.Instrument{
		Exchange: e.Name(), InstID: instID, Market: exchange.MarketSPOT,
		Base: "BTC", Quote: "USDT", LotSize: 0.00000001, MinSize: 0.00001, TickSize: 0.1,
	}, nil
}

func (e *Exchange) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]exchange.Balance, 0, len(e.balances))
	for a, v := range e.balances {
		out = append(out, exchange.Balance{Asset: a, Total: v, Available: v})
	}
	return out, nil
}

func (e *Exchange) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	if req.Qty <= 0 {
		return exchange.Order{}, fmt.Errorf("paper: 数量必须为正")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	id := fmt.Sprintf("paper-%d", e.seq)
	o := exchange.Order{
		Exchange: e.Name(), Symbol: req.Symbol, OrderID: id, ClientOrderID: req.ClientOrderID,
		Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty,
		Status: exchange.StatusSubmitted, CreatedAt: e.nowFn(), UpdatedAt: e.nowFn(),
	}
	if req.Type == exchange.OrderMarket {
		e.fillOrder(&o, e.marketPrice(req.Side))
	} else if e.priceTouches(req.Side, req.Price) {
		e.fillOrder(&o, req.Price)
	} else {
		// 挂单占用资金
		if !e.reserve(o) {
			o.Status = exchange.StatusRejected
		}
	}
	e.all[id] = o
	if req.ClientOrderID != "" {
		e.byClient[req.ClientOrderID] = id
	}
	if !o.Status.Terminal() {
		e.open[id] = o
	}
	return o, nil
}

func (e *Exchange) CancelOrder(ctx context.Context, symbol, orderID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.open[orderID]
	if !ok {
		return fmt.Errorf("paper: 挂单不存在或已终态 %s", orderID)
	}
	e.release(o)
	o.Status = exchange.StatusCancelled
	o.UpdatedAt = e.nowFn()
	e.all[orderID] = o
	delete(e.open, orderID)
	return nil
}

func (e *Exchange) GetOrder(ctx context.Context, symbol, orderID string) (exchange.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.all[orderID]
	if !ok {
		return exchange.Order{}, fmt.Errorf("paper: 订单不存在 %s", orderID)
	}
	return o, nil
}

func (e *Exchange) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (exchange.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.byClient[clientOrderID]
	if !ok {
		return exchange.Order{}, fmt.Errorf("paper: clientOrderID 不存在 %s", clientOrderID)
	}
	o := e.all[id]
	if symbol != "" && o.Symbol != symbol {
		return exchange.Order{}, fmt.Errorf("paper: clientOrderID %s 不属于 %s", clientOrderID, symbol)
	}
	return o, nil
}

func (e *Exchange) GetOpenOrders(ctx context.Context, symbol string) ([]exchange.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]exchange.Order, 0, len(e.open))
	for _, o := range e.open {
		if o.Symbol == symbol {
			out = append(out, o)
		}
	}
	return out, nil
}

// UpdatePrice 注入最新价并结算挂单（模拟盘的价格驱动入口）。
func (e *Exchange) UpdatePrice(p float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p <= 0 {
		return
	}
	e.last = p
	for id, o := range e.open {
		if o.Type != exchange.OrderLimit || !e.priceTouches(o.Side, o.Price) {
			continue
		}
		e.releaseRemaining(o)
		e.fillOrder(&o, o.Price)
		o.UpdatedAt = e.nowFn()
		e.all[id] = o
		if o.Status.Terminal() {
			delete(e.open, id)
		} else {
			if !e.reserveRemaining(o) {
				o.Status = exchange.StatusRejected
				e.all[id] = o
				delete(e.open, id)
			} else {
				e.open[id] = o
			}
		}
	}
}

func (e *Exchange) marketPrice(side exchange.Side) float64 {
	if side == exchange.Buy {
		return e.last * (1 + e.fill.SlippageBps/10000)
	}
	return e.last * (1 - e.fill.SlippageBps/10000)
}

func (e *Exchange) priceTouches(side exchange.Side, price float64) bool {
	if side == exchange.Buy {
		return e.last <= price // 买价 >= 委托价时成交
	}
	return e.last >= price
}

func (e *Exchange) fillOrder(o *exchange.Order, px float64) {
	remaining := o.Qty - o.FilledQty
	if remaining <= 1e-12 {
		o.Status = exchange.StatusFilled
		return
	}
	qty := remaining * e.fill.PartialRatio
	if qty <= 1e-12 {
		// 尘埃尾差：剩余量太小再打折就永远凑不满，直接一次性补齐，避免订单卡死在部分成交
		qty = remaining
	}
	notional := qty * px
	fee := notional * e.fill.FeeBps / 10000
	base, quote := "BTC", "USDT"
	if o.Side == exchange.Buy {
		if e.balances[quote] < notional+fee {
			o.Status = exchange.StatusRejected
			return
		}
		e.balances[quote] -= notional + fee
		e.balances[base] += qty
	} else {
		if e.balances[base] < qty {
			o.Status = exchange.StatusRejected
			return
		}
		e.balances[base] -= qty
		e.balances[quote] += notional - fee
	}
	oldQty := o.FilledQty
	o.FilledQty += qty
	o.AvgPrice = (o.AvgPrice*oldQty + px*qty) / o.FilledQty
	o.Fee -= fee
	o.FeeCcy = quote
	if o.FilledQty+1e-12 >= o.Qty {
		o.FilledQty = o.Qty
		o.Status = exchange.StatusFilled
	} else {
		o.Status = exchange.StatusPartiallyFilled
	}
}

func (e *Exchange) reserve(o exchange.Order) bool {
	return e.reserveQty(o, o.Qty)
}

func (e *Exchange) reserveRemaining(o exchange.Order) bool {
	return e.reserveQty(o, o.Qty-o.FilledQty)
}

func (e *Exchange) reserveQty(o exchange.Order, qty float64) bool {
	if qty <= 0 {
		return true
	}
	if o.Side == exchange.Buy {
		amount := o.Price * qty
		if e.balances["USDT"] < amount {
			return false
		}
		e.balances["USDT"] -= amount
		return true
	}
	if e.balances["BTC"] < qty {
		return false
	}
	e.balances["BTC"] -= qty
	return true
}

func (e *Exchange) release(o exchange.Order) { e.releaseRemaining(o) }

func (e *Exchange) releaseRemaining(o exchange.Order) {
	qty := o.Qty - o.FilledQty
	if qty <= 0 {
		return
	}
	if o.Side == exchange.Buy {
		e.balances["USDT"] += o.Price * qty
	} else {
		e.balances["BTC"] += qty
	}
}
