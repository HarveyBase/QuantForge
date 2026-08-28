// Package portfolio 仓位与权益管理：持仓、可用余额、权益曲线口径。
package portfolio

import (
	"fmt"
	"math"
	"sync"

	"github.com/HarveyBase/QuantForge/exchange"
)

type Position struct {
	Symbol    string  `json:"symbol"`
	Qty       float64 `json:"qty"`
	AvgPrice  float64 `json:"avg_price"`
	Available float64 `json:"available"`
}

type freezeEntry struct {
	req       exchange.OrderRequest
	remaining float64
}

type Portfolio struct {
	mu        sync.RWMutex
	Cash      float64              `json:"cash"`
	Positions map[string]*Position `json:"positions"`
	marks     map[string]float64
	freezes   map[string]freezeEntry
}

func New(seedCash float64) *Portfolio {
	return &Portfolio{Cash: seedCash, Positions: map[string]*Position{}, marks: map[string]float64{}, freezes: map[string]freezeEntry{}}
}

// Seed 用交易所余额初始化现货账本。余额中的 Available 用于可交易资产。
func (p *Portfolio) Seed(balances []exchange.Balance, symbol, base, quote string, mark float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range balances {
		if b.Asset == quote {
			p.Cash = b.Available
		}
		if b.Asset == base && b.Available > 0 {
			p.Positions[symbol] = &Position{Symbol: symbol, Qty: b.Available, Available: b.Available, AvgPrice: mark}
		}
	}
	if mark > 0 {
		p.marks[base] = mark
	}
}

func (p *Portfolio) UpdateMark(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updateMarkLocked(symbol, price)
}
func (p *Portfolio) updateMarkLocked(symbol string, price float64) {
	if price > 0 {
		p.marks[symbol] = price
	}
}
func (p *Portfolio) Mark(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.marks[symbol]
}

func (p *Portfolio) ApplyTrade(o exchange.Order) {
	if o.FilledQty <= 0 {
		return
	}
	p.applyFill(exchange.Fill{Symbol: o.Symbol, ClientOrderID: o.ClientOrderID, Side: o.Side, Qty: o.FilledQty, Price: o.AvgPrice, Fee: o.Fee, FeeCcy: o.FeeCcy, Ts: o.UpdatedAt})
}

// ApplyFill 应用一笔增量成交，并自动消费对应订单的冻结。
func (p *Portfolio) ApplyFill(f exchange.Fill) {
	if f.Qty <= 0 || f.Price <= 0 {
		return
	}
	p.applyFill(f)
}

func (p *Portfolio) applyFill(f exchange.Fill) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.Positions[f.Symbol]
	if pos == nil {
		pos = &Position{Symbol: f.Symbol}
		p.Positions[f.Symbol] = pos
	}
	frozen := false
	if f.ClientOrderID != "" {
		if entry, ok := p.freezes[f.ClientOrderID]; ok {
			frozen = true
			qty := f.Qty
			if qty > entry.remaining {
				qty = entry.remaining
			}
			if entry.req.Side == exchange.Buy {
				p.Cash += entry.req.Price * qty
			}
		}
	}
	notional := f.Qty * f.Price
	switch f.Side {
	case exchange.Buy:
		newQty := pos.Qty + f.Qty
		if newQty > 0 {
			if pos.Qty >= 0 {
				pos.AvgPrice = (pos.AvgPrice*pos.Qty + notional) / newQty
			}
			// 买入的币即时可用：买单冻结占用的是现金而非币（卖侧冻结才扣 Available）
			pos.Available += f.Qty
		}
		pos.Qty = newQty
		p.Cash -= notional
	case exchange.Sell:
		pos.Qty -= f.Qty
		if !frozen {
			pos.Available -= f.Qty
		}
		p.Cash += notional
		if math.Abs(pos.Qty) < 1e-12 {
			pos.Qty = 0
			pos.AvgPrice = 0
		}
	}
	if f.Fee < 0 {
		p.Cash += f.Fee
	} else {
		p.Cash -= f.Fee
	}
	p.updateMarkLocked(f.Symbol, f.Price)
	if frozen && f.ClientOrderID != "" {
		entry := p.freezes[f.ClientOrderID]
		entry.remaining -= f.Qty
		if entry.remaining < 0 {
			entry.remaining = 0
		}
		p.freezes[f.ClientOrderID] = entry
	}
}

func freezeKey(req exchange.OrderRequest) string {
	if req.ClientOrderID != "" {
		return req.ClientOrderID
	}
	return fmt.Sprintf("%s:%s:%g:%g", req.Symbol, req.Side, req.Price, req.Qty)
}

// Freeze 按订单精确冻结现金或可卖数量；失败时不改变账本。
func (p *Portfolio) Freeze(req exchange.OrderRequest) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := freezeKey(req)
	if _, ok := p.freezes[key]; ok {
		return true
	}
	amount := req.Price * req.Qty
	if req.Side == exchange.Buy {
		if req.Price <= 0 || amount > p.Cash+1e-12 {
			return false
		}
		p.Cash -= amount
	} else if req.Side == exchange.Sell {
		pos := p.Positions[req.Symbol]
		if pos == nil || req.Qty <= 0 || req.Qty > pos.Available+1e-12 {
			return false
		}
		pos.Available -= req.Qty
	} else {
		return false
	}
	p.freezes[key] = freezeEntry{req: req, remaining: req.Qty}
	return true
}

// Release 释放订单剩余冻结；重复调用幂等。
func (p *Portfolio) Release(req exchange.OrderRequest) { p.ReleaseOrder(freezeKey(req)) }
func (p *Portfolio) ReleaseOrder(clientOrderID string) {
	if clientOrderID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.freezes[clientOrderID]
	if !ok {
		return
	}
	p.releaseLocked(f.req, f.remaining)
	delete(p.freezes, clientOrderID)
}

// ConsumeFreeze 消耗成交对应的冻结量，终态时调用 ReleaseOrder 释放剩余量。
func (p *Portfolio) ConsumeFreeze(clientOrderID string, qty float64) {
	if clientOrderID == "" || qty <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.freezes[clientOrderID]
	if !ok {
		return
	}
	if qty > f.remaining {
		qty = f.remaining
	}
	f.remaining -= qty
	p.freezes[clientOrderID] = f
}

func (p *Portfolio) releaseLocked(req exchange.OrderRequest, qty float64) {
	if req.Side == exchange.Buy {
		p.Cash += req.Price * qty
	} else if pos := p.Positions[req.Symbol]; pos != nil {
		pos.Available += qty
	}
}

func (p *Portfolio) Equity() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.Cash
	for _, pos := range p.Positions {
		e += pos.Qty * p.marks[pos.Symbol]
	}
	return e
}
func (p *Portfolio) PositionNotional(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pos := p.Positions[symbol]
	if pos == nil {
		return 0
	}
	mark := p.marks[symbol]
	if mark == 0 {
		mark = pos.AvgPrice
	}
	return math.Abs(pos.Qty * mark)
}
func (p *Portfolio) Snapshot() (float64, []Position, map[string]float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ps := make([]Position, 0, len(p.Positions))
	for _, pos := range p.Positions {
		ps = append(ps, *pos)
	}
	ms := make(map[string]float64, len(p.marks))
	for k, v := range p.marks {
		ms[k] = v
	}
	return p.Cash, ps, ms
}

func (p *Portfolio) Reconcile(balances []exchange.Balance, positions []Position) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var diffs []string
	for _, b := range balances {
		if b.Asset == "USDT" {
			if d := b.Total - p.Cash; abs(d) > max(1, abs(p.Cash)*.001) {
				diffs = append(diffs, fmt.Sprintf("USDT 差异: 本地 %.2f vs 交易所 %.2f", p.Cash, b.Total))
			}
		}
	}
	for _, ep := range positions {
		local := p.Positions[ep.Symbol]
		lq := 0.0
		if local != nil {
			lq = local.Qty
		}
		if d := ep.Qty - lq; abs(d) > 1e-8 {
			diffs = append(diffs, fmt.Sprintf("%s 持仓差异: 本地 %.8f vs 交易所 %.8f", ep.Symbol, lq, ep.Qty))
		}
	}
	return diffs
}
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
