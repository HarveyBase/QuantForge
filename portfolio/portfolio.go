// Package portfolio 仓位与权益管理：持仓、可用余额、权益曲线口径（equity = cash + Σ qty×mark）。
package portfolio

import (
	"fmt"
	"sync"

	"github.com/HarveyBase/QuantForge/exchange"
)

// Position 单标的持仓（净持仓口径）。
type Position struct {
	Symbol    string  `json:"symbol"`
	Qty       float64 `json:"qty"`        // 持仓数量（有符号，负=空头，仅合约）
	AvgPrice  float64 `json:"avg_price"`  // 开仓均价
	Available float64 `json:"available"`  // 可卖数量（冻结剔除；卖出校验用它）
}

// Portfolio 账户状态。Available 概念对现货是"未挂单冻结"，对 T+1 市场是"非当日买入"——加密现货无 T+1，但卖出前校验可用是通用纪律。
type Portfolio struct {
	mu        sync.RWMutex
	Cash      float64              `json:"cash"` // 计价货币（USDT）
	Positions map[string]*Position `json:"positions"`
	marks     map[string]float64   // symbol -> 标记价
}

func New(seedCash float64) *Portfolio {
	return &Portfolio{
		Cash:      seedCash,
		Positions: map[string]*Position{},
		marks:     map[string]float64{},
	}
}

// UpdateMark 更新标记价（权益按 mark 计算）。
func (p *Portfolio) UpdateMark(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if price > 0 {
		p.marks[symbol] = price
	}
}

// Mark 当前标记价（无价返回 0）。
func (p *Portfolio) Mark(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.marks[symbol]
}

// ApplyTrade 应用一笔成交：更新现金与持仓（含手续费）。
func (p *Portfolio) ApplyTrade(o exchange.Order) {
	if o.FilledQty <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	notional := o.FilledQty * o.AvgPrice
	pos, ok := p.Positions[o.Symbol]
	if !ok {
		pos = &Position{Symbol: o.Symbol}
		p.Positions[o.Symbol] = pos
	}
	switch o.Side {
	case exchange.Buy:
		newQty := pos.Qty + o.FilledQty
		if pos.Qty >= 0 { // 加仓：摊均价
			pos.AvgPrice = (pos.AvgPrice*pos.Qty + notional) / newQty
		} else if pos.Qty+o.FilledQty >= 0 { // 空头完全回补，均价清零
			pos.AvgPrice = 0
		}
		pos.Qty = newQty
		pos.Available += o.FilledQty
		p.Cash -= notional
	case exchange.Sell:
		pos.Qty -= o.FilledQty
		pos.Available -= o.FilledQty
		p.Cash += notional
	}
	fee := -o.Fee // Fee 负=已支付
	if fee > 0 {
		p.Cash -= fee
	}
	p.UpdateMarkLocked(o.Symbol, o.AvgPrice)
	if approx(pos.Qty, 0) {
		pos.AvgPrice = 0
	}
}

func (p *Portfolio) UpdateMarkLocked(symbol string, price float64) {
	if price > 0 {
		p.marks[symbol] = price
	}
}

// Freeze / Release 挂单资金占用（可用余额管理）。
func (p *Portfolio) Freeze(o exchange.OrderRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if o.Side == exchange.Buy {
		p.Cash -= o.Price * o.Qty
	} else if pos := p.Positions[o.Symbol]; pos != nil {
		pos.Available -= o.Qty
	}
}

func (p *Portfolio) Release(o exchange.OrderRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if o.Side == exchange.Buy {
		p.Cash += o.Price * o.Qty
	} else if pos := p.Positions[o.Symbol]; pos != nil {
		pos.Available += o.Qty
	}
}

// Equity 总权益 = cash + Σ qty×mark。
func (p *Portfolio) Equity() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.Cash
	for _, pos := range p.Positions {
		e += pos.Qty * p.marks[pos.Symbol]
	}
	return e
}

// PositionNotional 单标的敞口名义价值（USD 估算）。
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
	return pos.Qty * mark
}

// Snapshot 只读副本（dashboard/回测输出用）。
func (p *Portfolio) Snapshot() (cash float64, positions []Position, marks map[string]float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	positions = make([]Position, 0, len(p.Positions))
	for _, pos := range p.Positions {
		positions = append(positions, *pos)
	}
	marks = make(map[string]float64, len(p.marks))
	for k, v := range p.marks {
		marks[k] = v
	}
	return p.Cash, positions, marks
}

// Reconcile 对账：本地持仓 vs 交易所回报，差异非零即告警项。
func (p *Portfolio) Reconcile(balances []exchange.Balance, positions []Position) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var diffs []string
	for _, b := range balances {
		switch b.Asset {
		case "USDT":
			if diff := b.Total - p.Cash; abs(diff) > max(1.0, abs(p.Cash)*0.001) {
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
		if diff := ep.Qty - lq; abs(diff) > 1e-8 {
			diffs = append(diffs, fmt.Sprintf("%s 持仓差异: 本地 %.8f vs 交易所 %.8f", ep.Symbol, lq, ep.Qty))
		}
	}
	return diffs
}

func approx(a, b float64) bool { return abs(a-b) < 1e-12 }

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
