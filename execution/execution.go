// Package execution 下单执行：风控前置、幂等提交、回报同步和账本更新。
package execution

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
)

const (
	maxRetries     = 3
	retryBaseDelay = 500 * time.Millisecond
	reconcileEvery = 3 * time.Second

	rateLimitBackoffMax = 30 * time.Second // 限流退避上限
)

// rateLimitBackoff 限流退避基数（至少 5s，远长于普通指数退避：限流下重锤只会更糟）。
// 变量便于测试缩短，生产值 5s。
var rateLimitBackoff = 5 * time.Second

type Event struct {
	Ts         time.Time      `json:"ts"`
	Kind       string         `json:"kind"`
	Order      exchange.Order `json:"order"`
	Note       string         `json:"note,omitempty"`
	DeltaQty   float64        `json:"delta_qty,omitempty"`
	DeltaPrice float64        `json:"delta_price,omitempty"`
}

type Executor struct {
	Ex        exchange.Exchange
	Rk        *risk.Manager
	Pf        *portfolio.Portfolio
	mu        sync.Mutex
	orders    map[string]exchange.Order
	byClient  map[string]string
	claimed   map[string]bool
	inflight  map[string]bool
	events    []Event
	journal   *Journal // 事件溯源日志（可选；挂载后内存表变更逐点落盘）
	onEvent   func(Event)
	cancelCtx context.Context
	stopFn    context.CancelFunc
}

func New(ex exchange.Exchange, rk *risk.Manager, pf *portfolio.Portfolio, onEvent func(Event)) *Executor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Executor{Ex: ex, Rk: rk, Pf: pf, orders: map[string]exchange.Order{}, byClient: map[string]string{}, claimed: map[string]bool{}, inflight: map[string]bool{}, onEvent: onEvent, cancelCtx: ctx, stopFn: cancel}
}

// Submit 执行风控、原子幂等占位、先查后补重试，并同步成交账本。
func (e *Executor) Submit(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	if req.ClientOrderID == "" {
		req.ClientOrderID = fmt.Sprintf("qf-%d", time.Now().UnixNano())
	}
	e.mu.Lock()
	if e.claimed[req.ClientOrderID] || e.inflight[req.ClientOrderID] {
		old := e.lookupClientLocked(req.ClientOrderID)
		e.mu.Unlock()
		return old, fmt.Errorf("execution: clientOrderID %s 已提交或正在提交", req.ClientOrderID)
	}
	e.inflight[req.ClientOrderID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.inflight, req.ClientOrderID); e.mu.Unlock() }()
	mark := e.Pf.Mark(req.Symbol)
	if err := e.Rk.CheckOrder(req, mark); err != nil {
		e.Rk.StartCooldown()
		e.emit(Event{Ts: time.Now(), Kind: "rejected", Order: orderFromReq(e.Ex.Name(), req), Note: err.Error()})
		return exchange.Order{}, err
	}
	var lastErr error
	attempts := 0 // 实际重试次数（不可重试错误为 0）
	delay := time.Duration(0)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return exchange.Order{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		if e.Rk.Kill.Tripped() {
			return exchange.Order{}, fmt.Errorf("execution: Kill Switch 已触发，中止重试")
		}
		o, err := e.Ex.PlaceOrder(ctx, req)
		if err == nil {
			if o.ClientOrderID == "" {
				o.ClientOrderID = req.ClientOrderID
			}
			if o.Symbol == "" {
				o.Symbol = req.Symbol
			}
			if o.Status == exchange.StatusRejected {
				e.emit(Event{Ts: time.Now(), Kind: "rejected", Order: o, Note: "交易所返回拒单"})
				return exchange.Order{}, fmt.Errorf("execution: 交易所拒绝订单 %s", req.ClientOrderID)
			}
			if !e.register(o, req) {
				return exchange.Order{}, fmt.Errorf("execution: 订单登记失败 %s", req.ClientOrderID)
			}
			return o, nil
		}
		lastErr = err
		if exchange.IsRateLimitError(err) {
			attempts++
			delay = min(rateLimitBackoff<<(attempt+1), rateLimitBackoffMax) // 限流退避远长于普通退避
			continue
		}
		if retryable(err) {
			if attempt > 0 {
				attempts++
			}
			delay = retryBaseDelay << (attempt + 1)
			if found, qerr := e.Ex.GetOrderByClientID(ctx, req.Symbol, req.ClientOrderID); qerr == nil {
				if found.ClientOrderID == "" {
					found.ClientOrderID = req.ClientOrderID
				}
				if e.register(found, req) {
					return found, nil
				}
				return exchange.Order{}, fmt.Errorf("execution: 找到订单但登记失败 %s", req.ClientOrderID)
			}
			continue
		}
		// 不可重试但疑似"重复 clientOrderID"（交易所实际已收单，重启/重试窗口内常见）：
		// 仍查后补一次，防止误报失败导致上层重复下单。
		if duplicateOrderErr(err) {
			if found, qerr := e.Ex.GetOrderByClientID(ctx, req.Symbol, req.ClientOrderID); qerr == nil && found.OrderID != "" {
				if found.ClientOrderID == "" {
					found.ClientOrderID = req.ClientOrderID
				}
				if e.register(found, req) {
					return found, nil
				}
			}
		}
		break
	}
	return exchange.Order{}, fmt.Errorf("execution: 下单失败（重试 %d 次）: %w", attempts, lastErr)
}

func (e *Executor) register(o exchange.Order, req exchange.OrderRequest) bool {
	e.mu.Lock()
	if e.claimed[req.ClientOrderID] {
		e.mu.Unlock()
		return false
	}
	e.claimed[req.ClientOrderID] = true
	e.orders[o.OrderID] = o
	e.byClient[req.ClientOrderID] = o.OrderID
	e.journalEvent("register", o)
	e.mu.Unlock()
	if o.Status == exchange.StatusSubmitted || o.Status == exchange.StatusPartiallyFilled {
		if req.Type == exchange.OrderLimit && !e.Pf.Freeze(req) {
			e.removeOrder(o.OrderID)
			e.emit(Event{Ts: time.Now(), Kind: "rejected", Order: o, Note: "本地账本无法冻结订单"})
			return false
		}
	}
	e.emit(Event{Ts: time.Now(), Kind: "submitted", Order: o})
	if o.FilledQty > 0 {
		e.applyDelta(exchange.Order{}, o)
	}
	return true
}

func (e *Executor) removeOrder(id string) {
	e.mu.Lock()
	if o, ok := e.orders[id]; ok {
		delete(e.claimed, o.ClientOrderID)
		e.journalEvent("remove", o)
	}
	delete(e.orders, id)
	e.mu.Unlock()
}

func (e *Executor) Cancel(ctx context.Context, symbol, orderID string) error {
	if err := e.Ex.CancelOrder(ctx, symbol, orderID); err != nil {
		return err
	}
	e.mu.Lock()
	o := e.orders[orderID]
	delete(e.orders, orderID)
	e.journalEvent("cancel", o)
	e.mu.Unlock()
	e.releaseFreeze(o)
	e.emit(Event{Ts: time.Now(), Kind: "cancelled", Order: o})
	return nil
}
func (e *Executor) CancelAll(ctx context.Context, symbol string) int {
	open, err := e.Ex.GetOpenOrders(ctx, symbol)
	if err != nil {
		log.Printf("execution: 拉取挂单失败: %v", err)
		return 0
	}
	n := 0
	for _, o := range open {
		if e.Cancel(ctx, symbol, o.OrderID) == nil {
			n++
		}
	}
	return n
}
func (e *Executor) Stop() {
	if e.stopFn != nil {
		e.stopFn()
	}
}
func (e *Executor) ReconcileLoop() {
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.cancelCtx.Done():
			return
		case <-ticker.C:
			e.ReconcileOnce(e.cancelCtx)
		}
	}
}
func (e *Executor) ReconcileOnce(ctx context.Context) {
	e.mu.Lock()
	ids := make([]string, 0, len(e.orders))
	for id, o := range e.orders {
		if !o.Status.Terminal() {
			ids = append(ids, id)
		}
	}
	e.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		symbol := e.symbolOf(id)
		fresh, err := e.Ex.GetOrder(ctx, symbol, id)
		if err != nil {
			log.Printf("execution: 同步订单 %s 失败: %v", id, err)
			continue
		}
		e.applyUpdate(fresh)
	}
}
func (e *Executor) applyUpdate(fresh exchange.Order) {
	e.mu.Lock()
	old, ok := e.orders[fresh.OrderID]
	if !ok {
		e.mu.Unlock()
		return
	}
	e.orders[fresh.OrderID] = fresh
	e.journalEvent("update", fresh)
	e.mu.Unlock()
	e.applyDelta(old, fresh)
}
func (e *Executor) applyDelta(old, fresh exchange.Order) {
	inc := fresh.FilledQty - old.FilledQty
	if inc > 1e-12 {
		grossNew := fresh.AvgPrice * fresh.FilledQty
		grossOld := old.AvgPrice * old.FilledQty
		px := (grossNew - grossOld) / inc
		fee := fresh.Fee - old.Fee
		e.Pf.ApplyFill(exchange.Fill{
			Symbol: fresh.Symbol, ClientOrderID: fresh.ClientOrderID,
			Side: fresh.Side, Qty: inc, Price: px, Fee: fee,
			FeeCcy: fresh.FeeCcy, Ts: fresh.UpdatedAt,
		})
		e.emit(Event{Ts: time.Now(), Kind: string(fresh.Status), Order: fresh, DeltaQty: inc, DeltaPrice: px})
	}
	if fresh.Status.Terminal() {
		e.releaseFreeze(fresh)
		e.mu.Lock()
		delete(e.orders, fresh.OrderID)
		e.journalEvent("freeze_release", fresh) // 冻结释放审计留痕（重放不动账本）
		e.mu.Unlock()
	} else if inc <= 1e-12 && fresh.Status != old.Status {
		e.emit(Event{Ts: time.Now(), Kind: string(fresh.Status), Order: fresh})
	}
}
func (e *Executor) releaseFreeze(o exchange.Order) {
	if o.ClientOrderID != "" {
		e.Pf.ReleaseOrder(o.ClientOrderID)
	}
}
func (e *Executor) lookupClient(coid string) exchange.Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lookupClientLocked(coid)
}
func (e *Executor) lookupClientLocked(coid string) exchange.Order {
	if id, ok := e.byClient[coid]; ok {
		return e.orders[id]
	}
	return exchange.Order{}
}
func (e *Executor) symbolOf(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.orders[id].Symbol
}
func (e *Executor) OpenOrders() []exchange.Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]exchange.Order, 0, len(e.orders))
	for _, o := range e.orders {
		if !o.Status.Terminal() {
			out = append(out, o)
		}
	}
	return out
}
func (e *Executor) Events(limit int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if limit <= 0 || limit > len(e.events) {
		limit = len(e.events)
	}
	out := make([]Event, limit)
	copy(out, e.events[len(e.events)-limit:])
	return out
}
func (e *Executor) emit(ev Event) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	if len(e.events) > 1000 {
		e.events = e.events[len(e.events)-500:]
	}
	e.mu.Unlock()
	if e.onEvent != nil {
		e.onEvent(ev)
	}
}
func orderFromReq(exName string, req exchange.OrderRequest) exchange.Order {
	return exchange.Order{Exchange: exName, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID, Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty, Status: exchange.StatusNew}
}
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if exchange.IsRateLimitError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"timeout", "connection refused", "eof", "reset by peer", "context deadline", "unexpected"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// duplicateOrderErr 识别"clientOrderID 已存在"类错误（不可重试，但订单可能已在途，
// 必须查后补而不是报失败，防止上层重复下单）。匹配词保守：only 明确的重复语义。
func duplicateOrderErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"already exist", "already submitted", "duplicate", "duplicated", "重复"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
