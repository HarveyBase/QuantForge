// Package portfolio 仓位与权益管理：持仓、可用余额、权益曲线口径。
package portfolio

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

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
	symbol    string // Seed 登记的主交易对（如 BTC-USDT），未登记时推导
	base      string // Base 币种（如 BTC）：手续费分账与对账用
	quote     string // Quote 币种（如 USDT）：现金口径与对账用
}

func New(seedCash float64) *Portfolio {
	return &Portfolio{Cash: seedCash, Positions: map[string]*Position{}, marks: map[string]float64{}, freezes: map[string]freezeEntry{}}
}

// Seed 用交易所余额初始化现货账本。
// 账本 Qty 记总持仓（= 余额 Total）、Available 记交易所可用（= 余额 Available）：
// 认领了挂卖单（交易所冻结 q）后重启，Qty=C−q vs 远程 Total=C 会产生永久对账差异；
// 冻结语义由本地 freezes 承担（卖单冻结扣 Available），两种口径各自对齐远程字段。
// 同时登记主交易对与 Base/Quote 币种，供手续费分账（FeeCcy 比对）与结构化对账使用。
func (p *Portfolio) Seed(balances []exchange.Balance, symbol, base, quote string, mark float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.symbol, p.base, p.quote = symbol, base, quote
	for _, b := range balances {
		if b.Asset == quote {
			p.Cash = b.Available
		}
		if b.Asset == base && b.Total > 0 {
			p.Positions[symbol] = &Position{Symbol: symbol, Qty: b.Total, Available: b.Available, AvgPrice: mark}
		}
	}
	if mark > 0 {
		p.marks[base] = mark
	}
}

// Reset 账本清零（现金/持仓/标记价/冻结四清）：启动 Seed 前清掉恢复阶段打到
// 未初始化账本上的增量（停机期间的成交已含在交易所余额里，Seed 以余额为唯一
// 权威重建）。替代调用方直写导出字段。
func (p *Portfolio) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Cash = 0
	p.Positions = map[string]*Position{}
	p.marks = map[string]float64{}
	p.freezes = map[string]freezeEntry{}
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

// FillResult 成交入账结果：手续费分账口径与异常 clamp 的留痕载体（供对账/看门狗审计）。
type FillResult struct {
	Applied   bool   // 是否实际入账（Qty/Price 非法时忽略）
	FeeInBase bool   // 手续费按 Base 币从持仓扣除（OKX 现货买侧典型口径）
	Clamped   bool   // 异常数据：扣费后持仓/可用为负，已 clamp 为 0
	Note      string // clamp/异常口径的明细留痕
}

func (r *FillResult) clampf(format string, args ...any) {
	r.Clamped = true
	msg := fmt.Sprintf(format, args...)
	if r.Note == "" {
		r.Note = msg
	} else {
		r.Note += "; " + msg
	}
}

// baseOfSymbol 由交易对推导 Base 币种（"BTC-USDT" → "BTC"；"BTC-USDT-SWAP" 同样取首段）。
func baseOfSymbol(symbol string) string {
	if i := strings.Index(symbol, "-"); i > 0 {
		return symbol[:i]
	}
	return symbol
}

// quoteOfSymbol 由交易对推导 Quote 币种（"BTC-USDT" → "USDT"），推导失败回退 USDT。
func quoteOfSymbol(symbol string) string {
	if parts := strings.SplitN(symbol, "-", 3); len(parts) >= 2 && parts[1] != "" {
		return parts[1]
	}
	return "USDT"
}

// feeInBaseLocked 判断该笔成交手续费是否以 Base 币收取。
// FeeCcy 为空按 Quote 币处理（向后兼容：旧数据/适配器未填 FeeCcy 时维持扣现金口径）。
// Base 币名优先取 Seed 登记值，未登记时由交易对推导。
func (p *Portfolio) feeInBaseLocked(symbol, feeCcy string) bool {
	if feeCcy == "" {
		return false
	}
	return feeCcy == p.baseOfLocked(symbol)
}

// ApplyFill 应用一笔增量成交，并自动消费对应订单的冻结。
// 手续费按币种分账：FeeCcy=Base 时从持仓数量扣（买入实收 = Qty - Fee，卖出同样扣 Base，
// 现金按全额名义结算）；FeeCcy=Quote 或为空时维持扣现金（空值按 quote 处理，兼容旧数据）。
func (p *Portfolio) ApplyFill(f exchange.Fill) FillResult {
	if f.Qty <= 0 || f.Price <= 0 {
		return FillResult{Applied: false}
	}
	return p.applyFill(f)
}

func (p *Portfolio) applyFill(f exchange.Fill) FillResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := FillResult{Applied: true}
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
	fee := abs(f.Fee)
	feeInBase := fee > 0 && p.feeInBaseLocked(f.Symbol, f.FeeCcy)
	res.FeeInBase = feeInBase
	switch f.Side {
	case exchange.Buy:
		recv := f.Qty
		if feeInBase {
			// Base 币手续费：买入实收 = Qty - Fee（手续费不得再重复扣现金）
			recv = f.Qty - fee
			if recv < 0 {
				res.clampf("买入 %s 手续费 %.10f 超过成交数量 %.10f，实收 clamp 为 0", f.Symbol, fee, f.Qty)
				recv = 0
			}
		}
		newQty := pos.Qty + recv
		if newQty > 0 {
			if pos.Qty >= 0 {
				pos.AvgPrice = (pos.AvgPrice*pos.Qty + notional) / newQty
			}
			// 买入的币即时可用：买单冻结占用的是现金而非币（卖侧冻结才扣 Available）
			pos.Available += recv
		}
		pos.Qty = newQty
		p.Cash -= notional
	case exchange.Sell:
		pos.Qty -= f.Qty
		if !frozen {
			pos.Available -= f.Qty
		}
		p.Cash += notional
		if feeInBase {
			// Base 币手续费：卖出后从剩余持仓/可用再扣手续费（现金按全额名义结算）
			pos.Qty -= fee
			pos.Available -= fee
			if pos.Available < 0 {
				res.clampf("卖出 %s 手续费 %.10f 扣穿可用，可用 clamp 为 0", f.Symbol, fee)
				pos.Available = 0
			}
		}
		if math.Abs(pos.Qty) < 1e-12 {
			pos.Qty = 0
			pos.AvgPrice = 0
		} else if feeInBase && pos.Qty < 0 {
			res.clampf("卖出 %s 手续费 %.10f 扣穿持仓，数量 clamp 为 0", f.Symbol, fee)
			pos.Qty = 0
			pos.AvgPrice = 0
		}
	}
	if fee > 0 && !feeInBase {
		// Quote 币手续费（含 FeeCcy 为空的兼容口径）：从现金扣
		if f.Fee < 0 {
			p.Cash += f.Fee
		} else {
			p.Cash -= f.Fee
		}
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
	return res
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
	// 买单冻结的现金仍是权益（挂单未成交期间不得出现权益假性塌陷，
	// 否则回测 MDD 虚高、实盘当日回撤风控会误触发 Kill Switch）
	for _, f := range p.freezes {
		if f.req.Side == exchange.Buy {
			e += f.req.Price * f.remaining
		}
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

// 对账差异类别。
const (
	DiffCash      = "cash"      // 计价币（现金）余额差异
	DiffPosition  = "position"  // 持仓数量差异
	DiffAvailable = "available" // 可用数量差异
)

// 对账容差：绝对 1e-6 与相对 0.1% 取大，容差内视为一致。
const (
	reconcileAbsTol = 1e-6
	reconcileRelTol = 0.001
)

// ReconcileDiff 单条结构化差异（Diff = Remote - Local）。
type ReconcileDiff struct {
	Kind   string  `json:"kind"` // "cash" | "position" | "available"
	Item   string  `json:"item"` // cash=计价币种；position/available=交易对或资产名
	Local  float64 `json:"local"`
	Remote float64 `json:"remote"`
	Diff   float64 `json:"diff"`
}

// ReconcileReport 结构化对账报告：远程按币种余额与本地账本（现金/持仓/可用）比对。
type ReconcileReport struct {
	Ts    time.Time       `json:"ts"`
	Ok    bool            `json:"ok"`
	Diffs []ReconcileDiff `json:"diffs"`
}

// withinTol 对账容差判断：|Remote - Local| <= max(1e-6, max(|Local|,|Remote|) * 0.1%)。
func withinTol(local, remote float64) bool {
	tol := max(reconcileAbsTol, max(abs(local), abs(remote))*reconcileRelTol)
	return abs(remote-local) <= tol
}

// ReconcileDetail 结构化对账（新代码请优先使用，旧 Reconcile 保留兼容）：
// 远程 balances 按币种映射后，比对 ①现金（本地可用 + Σ买单冻结 vs Quote 币 Total，
// 缺失按 0 计）②每个本地持仓的 Qty/Available ③本地无持仓但远程余额超容差的 Base 币。
func (p *Portfolio) ReconcileDetail(balances []exchange.Balance) ReconcileReport {
	p.mu.RLock()
	defer p.mu.RUnlock()
	report := ReconcileReport{Ts: time.Now().UTC(), Ok: true}
	remote := make(map[string]exchange.Balance, len(balances))
	for _, b := range balances {
		remote[b.Asset] = b
	}
	quote := p.quoteOfLocked()
	// ① 现金：本地 Cash 是已扣买单冻结的"可用"口径，而远程 Total 含在途买单冻结——
	// 比对口径必须对齐：本地可用 + Σ买单冻结 ≈ 交易所 Total。否则挂一张 100 USDT
	// 买单即报 diff 100，网格常态被 RECONCILE_BLOCK 误拦。
	localCash := p.Cash
	for _, f := range p.freezes {
		if f.req.Side == exchange.Buy {
			localCash += f.req.Price * f.remaining
		}
	}
	qb := remote[quote]
	if !withinTol(localCash, qb.Total) {
		report.Diffs = append(report.Diffs, ReconcileDiff{Kind: DiffCash, Item: quote, Local: localCash, Remote: qb.Total, Diff: qb.Total - localCash})
	}
	// ② 每个本地持仓：数量与可用双比对（远程按该持仓的 Base 币余额）
	seenBase := make(map[string]bool, len(p.Positions))
	for _, pos := range p.Positions {
		base := p.baseOfLocked(pos.Symbol)
		seenBase[base] = true
		b := remote[base]
		if !withinTol(pos.Qty, b.Total) {
			report.Diffs = append(report.Diffs, ReconcileDiff{Kind: DiffPosition, Item: pos.Symbol, Local: pos.Qty, Remote: b.Total, Diff: b.Total - pos.Qty})
		}
		if !withinTol(pos.Available, b.Available) {
			report.Diffs = append(report.Diffs, ReconcileDiff{Kind: DiffAvailable, Item: pos.Symbol, Local: pos.Available, Remote: b.Available, Diff: b.Available - pos.Available})
		}
	}
	// ③ 本地无持仓的 Base 币：远程余额超容差即报差异（本地视为 0）
	for asset, b := range remote {
		if asset == quote || seenBase[asset] {
			continue
		}
		if !withinTol(0, b.Total) {
			report.Diffs = append(report.Diffs, ReconcileDiff{Kind: DiffPosition, Item: asset, Local: 0, Remote: b.Total, Diff: b.Total})
		}
	}
	report.Ok = len(report.Diffs) == 0
	return report
}

// baseOfLocked 取某交易对的 Base 币种：Seed 登记的主交易对用登记值，其余按符号推导。
func (p *Portfolio) baseOfLocked(symbol string) string {
	if p.base != "" && (p.symbol == "" || p.symbol == symbol) {
		return p.base
	}
	return baseOfSymbol(symbol)
}

// quoteOfLocked 取计价币种：Seed 登记值优先，未登记时按主交易对推导，再回退 USDT（兼容旧口径）。
func (p *Portfolio) quoteOfLocked() string {
	if p.quote != "" {
		return p.quote
	}
	if p.symbol != "" {
		return quoteOfSymbol(p.symbol)
	}
	return "USDT"
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
