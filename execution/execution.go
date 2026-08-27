// Package execution 下单执行：风控前置 → 下单 → 轮询回报 → 同步订单簿。
// 纪律（docs/08）：clientOrderID 幂等去重；重试有上限+退避；回报延迟先查后补（禁止盲目补发整单）；
// Kill Switch 检查点贯穿提交与回报同步两处；拒单绝不静默。
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
)

// Event 执行层事件（dashboard 推送与审计用）。
type Event struct {
	Ts    time.Time       `json:"ts"`
	Kind  string          `json:"kind"` // submitted / filled / partially_filled / cancelled / rejected / reconciled
	Order exchange.Order  `json:"order"`
	Note  string          `json:"note,omitempty"`
}

// Executor 执行器：包装交易所适配器，所有订单必须经 Submit 走风控。
type Executor struct {
	Ex  exchange.Exchange
	Rk  *risk.Manager
	Pf  *portfolio.Portfolio

	mu        sync.Mutex
	orders    map[string]exchange.Order // orderID → 最新状态
	byClient  map[string]string         // clientOrderID → orderID（幂等）
	submitted map[string]bool           // clientOrderID 已提交过（防重复提交）
	events    []Event
	onEvent   func(Event)

	cancelCtx context.Context
	stopFn    context.CancelFunc
}

func New(ex exchange.Exchange, rk *risk.Manager, pf *portfolio.Portfolio, onEvent func(Event)) *Executor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Executor{
		Ex: ex, Rk: rk, Pf: pf,
		orders: map[string]exchange.Order{}, byClient: map[string]string{},
		submitted: map[string]bool{}, onEvent: onEvent,
		cancelCtx: ctx, stopFn: cancel,
	}
}

// Submit 提交订单：风控 → 幂等检查 → 下单（重试上限+退避）→ 登记订单簿。
func (e *Executor) Submit(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	if req.ClientOrderID == "" {
		req.ClientOrderID = fmt.Sprintf("qf-%d", time.Now().UnixNano())
	}
	e.mu.Lock()
	if e.submitted[req.ClientOrderID] {
		e.mu.Unlock()
		return e.lookupClient(req.ClientOrderID), fmt.Errorf("execution: clientOrderID %s 已提交过（幂等拒绝重复下单）", req.ClientOrderID)
	}
	e.mu.Unlock()

	// 风控前置门禁（Kill Switch 检查点①）
	mark := e.Pf.Mark(req.Symbol)
	if err := e.Rk.CheckOrder(req, mark); err != nil {
		e.Rk.StartCooldown()
		e.emit(Event{Ts: time.Now(), Kind: "rejected", Order: orderFromReq(e.Ex.Name(), req), Note: err.Error()})
		return exchange.Order{}, err
	}

	// 下单（有限重试 + 指数退避）
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return exchange.Order{}, ctx.Err()
			case <-time.After(retryBaseDelay << attempt):
			}
		}
		if e.Rk.Kill.Tripped() { // 重试前再查一次（检查点②）
			return exchange.Order{}, fmt.Errorf("execution: Kill Switch 已触发，中止重试")
		}
		o, err := e.Ex.PlaceOrder(ctx, req)
		if err == nil {
			e.mu.Lock()
			e.submitted[req.ClientOrderID] = true
			e.orders[o.OrderID] = o
			e.byClient[o.ClientOrderID] = o.OrderID
			e.mu.Unlock()
			e.Pf.Freeze(req) // 挂单冻结资金/可卖
			e.emit(Event{Ts: time.Now(), Kind: "submitted", Order: o})
			// 适配器可能立即成交（市价单/可成交限价单）：直接驱动账本，避免等对账
			if o.FilledQty > 0 {
				e.Pf.ApplyTrade(o)
				if o.Status.Terminal() {
					e.releaseFreeze(o)
				}
				e.emit(Event{Ts: time.Now(), Kind: string(o.Status), Order: o})
			}
			return o, nil
		}
		lastErr = err
		// 网络类错误才重试；参数/权限类错误立即失败
		if !retryable(err) {
			break
		}
	}
	return exchange.Order{}, fmt.Errorf("execution: 下单失败（重试 %d 次）: %w", maxRetries, lastErr)
}

// Cancel 撤单并释放资金占用。
func (e *Executor) Cancel(ctx context.Context, symbol, orderID string) error {
	if err := e.Ex.CancelOrder(ctx, symbol, orderID); err != nil {
		return err
	}
	e.mu.Lock()
	o := e.orders[orderID]
	delete(e.orders, orderID)
	e.mu.Unlock()
	e.releaseFreeze(o)
	e.emit(Event{Ts: time.Now(), Kind: "cancelled", Order: o})
	return nil
}

// CancelAll 一键撤所有挂单（Kill Switch 处置动作）。
func (e *Executor) CancelAll(ctx context.Context, symbol string) int {
	open, err := e.Ex.GetOpenOrders(ctx, symbol)
	if err != nil {
		log.Printf("execution: 拉取挂单失败: %v", err)
		return 0
	}
	n := 0
	for _, o := range open {
		if err := e.Cancel(ctx, symbol, o.OrderID); err == nil {
			n++
		}
	}
	return n
}

// ReconcileLoop 后台回报同步：轮询更新订单簿并驱动组合账本（Kill Switch 检查点③）。
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

// ReconcileOnce 单轮回报同步：先查后补（回报延迟不盲目补发），成交驱动 ApplyTrade。
func (e *Executor) ReconcileOnce(ctx context.Context) {
	e.mu.Lock()
	ids := make([]string, 0, len(e.orders))
	for id := range e.orders {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		fresh, err := e.Ex.GetOrder(ctx, e.symbolOf(id), id)
		if err != nil {
			continue // 单个订单同步失败不阻塞其他订单
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
	e.mu.Unlock()

	newlyFilled := fresh.FilledQty - old.FilledQty
	if newlyFilled > 1e-12 {
		fillPart := fresh
		fillPart.FilledQty = newlyFilled
		e.Pf.ApplyTrade(fillPart)
		if fresh.Status.Terminal() {
			e.releaseFreeze(fresh)
		}
		e.emit(Event{Ts: time.Now(), Kind: string(fresh.Status), Order: fresh})
	} else if fresh.Status != old.Status {
		if fresh.Status.Terminal() {
			e.releaseFreeze(fresh)
		}
		e.emit(Event{Ts: time.Now(), Kind: string(fresh.Status), Order: fresh})
	}
}

func (e *Executor) releaseFreeze(o exchange.Order) {
	if o.OrderID == "" {
		return
	}
	e.Pf.Release(exchange.OrderRequest{
		Symbol: o.Symbol, Side: o.Side, Price: o.Price, Qty: o.Qty,
		ClientOrderID: o.ClientOrderID,
	})
}

func (e *Executor) lookupClient(coid string) exchange.Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := e.byClient[coid]; ok {
		return e.orders[id]
	}
	return exchange.Order{}
}

func (e *Executor) symbolOf(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o, ok := e.orders[id]; ok {
		return o.Symbol
	}
	return ""
}

// OpenOrders 本地订单簿快照（未终态）。
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

// Events 事件流只读副本。
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
	return exchange.Order{
		Exchange: exName, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID,
		Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty,
		Status: exchange.StatusNew,
	}
}

// retryable 判断错误是否值得重试（网络/超时类）。
func retryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"timeout", "connection refused", "eof", "reset by peer", "context deadline", "unexpected"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
