package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
)

// fakeEx 可编程交易所替身：模拟网络故障、先查后补、拒单、部分成交等场景。
type fakeEx struct {
	mu sync.Mutex
	// placeErrs 依次返回的下单错误（nil 表示成功）
	placeErrs []error
	placeSeq  int
	placed    []exchange.OrderRequest
	// byClient 先查后补返回的订单（非零 ClientOrderID 时优先）
	byClient map[string]exchange.Order
	// orders GetOrder 返回表
	orders map[string]exchange.Order
	// openErr GetOpenOrders 错误
	openErr error
	open    []exchange.Order
	// getOrderErr GetOrder 错误
	getOrderErr map[string]error
	cancelErr   error
	cancelled   []string
}

func (f *fakeEx) Name() string { return "fake" }
func (f *fakeEx) GetCandles(ctx context.Context, symbol, interval string, limit int) ([]exchange.Candle, error) {
	return nil, errors.New("not supported")
}
func (f *fakeEx) GetTicker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	return exchange.Ticker{Symbol: symbol, Last: 100, Bid: 99.9, Ask: 100.1}, nil
}
func (f *fakeEx) GetInstrument(ctx context.Context, instID string) (exchange.Instrument, error) {
	return exchange.Instrument{Exchange: "fake", InstID: instID}, nil
}
func (f *fakeEx) GetOrderBook(ctx context.Context, symbol string, depth int) (exchange.OrderBook, error) {
	return exchange.OrderBook{}, nil
}
func (f *fakeEx) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	return []exchange.Balance{{Asset: "USDT", Total: 1e6, Available: 1e6}}, nil
}
func (f *fakeEx) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.placed = append(f.placed, req)
	if f.placeSeq < len(f.placeErrs) {
		err := f.placeErrs[f.placeSeq]
		f.placeSeq++
		if err != nil {
			return exchange.Order{}, err
		}
	}
	o := exchange.Order{
		Exchange: "fake", Symbol: req.Symbol, OrderID: "ord-1", ClientOrderID: req.ClientOrderID,
		Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty,
		Status: exchange.StatusSubmitted,
	}
	f.orders[o.OrderID] = o
	return o, nil
}
func (f *fakeEx) CancelOrder(ctx context.Context, symbol, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, orderID)
	return nil
}
func (f *fakeEx) GetOrder(ctx context.Context, symbol, orderID string) (exchange.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.getOrderErr[orderID]; err != nil {
		return exchange.Order{}, err
	}
	o, ok := f.orders[orderID]
	if !ok {
		return exchange.Order{}, errors.New("order not found")
	}
	return o, nil
}
func (f *fakeEx) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (exchange.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.byClient[clientOrderID]; ok {
		return o, nil
	}
	return exchange.Order{}, errors.New("not found")
}
func (f *fakeEx) GetOpenOrders(ctx context.Context, symbol string) ([]exchange.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.open, nil
}

func newFakeExecutor(t *testing.T, fe *fakeEx, seedCash float64) *Executor {
	t.Helper()
	if fe.orders == nil {
		fe.orders = map[string]exchange.Order{}
	}
	if fe.byClient == nil {
		fe.byClient = map[string]exchange.Order{}
	}
	if fe.getOrderErr == nil {
		fe.getOrderErr = map[string]error{}
	}
	pf := portfolio.New(seedCash)
	rk := risk.NewManager(risk.Limits{
		MaxOrderNotionalUSD: 1e9, MaxDailyNotionalUSD: 1e9, MaxPositionNotionalUSD: 1e9,
		MaxOrdersPerMinute: 1 << 30, MaxDailyLossPct: 100,
	}, pf, "")
	return New(fe, rk, pf, nil)
}

func TestSubmitRetryThenSuccess(t *testing.T) {
	fe := &fakeEx{placeErrs: []error{errors.New("connection refused")}} // 第一次超时，重试成功
	e := newFakeExecutor(t, fe, 10000)
	start := time.Now()
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.OrderID != "ord-1" {
		t.Fatalf("重试成功应返回订单: %+v", o)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("重试应遵守退避延迟: %v", elapsed)
	}
	if len(fe.placed) != 2 {
		t.Fatalf("应有 2 次尝试: %d", len(fe.placed))
	}
}

func TestSubmitQueryCompensationAfterRetryable(t *testing.T) {
	// 每次下单都超时，但交易所其实已收到（先查后补找到订单）
	fe := &fakeEx{placeErrs: []error{
		errors.New("timeout"), errors.New("timeout"), errors.New("timeout"), errors.New("timeout"),
	}, byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{}, getOrderErr: map[string]error{}}
	fe.byClient["r2"] = exchange.Order{
		Exchange: "fake", Symbol: "BTC-USDT", OrderID: "found-1", ClientOrderID: "r2",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 100, Qty: 1, Status: exchange.StatusSubmitted,
	}
	e := newFakeExecutor(t, fe, 10000)
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "r2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.OrderID != "found-1" {
		t.Fatalf("应通过先查后补找回订单: %+v", o)
	}
}

func TestSubmitNonRetryableFailsFast(t *testing.T) {
	fe := &fakeEx{placeErrs: []error{errors.New("invalid signature")}} // 不可重试
	e := newFakeExecutor(t, fe, 10000)
	start := time.Now()
	_, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "r3",
	})
	if err == nil {
		t.Fatal("下单失败必须返回错误")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("不可重试错误必须快速失败")
	}
	if len(fe.placed) != 1 {
		t.Fatalf("不可重试错误只应尝试一次: %d", len(fe.placed))
	}
}

func TestSubmitRetryExhausted(t *testing.T) {
	// 4 次都超时且先查后补找不到 → 彻底失败
	fe := &fakeEx{placeErrs: []error{toooLateErr(), toooLateErr(), toooLateErr(), toooLateErr()}}
	e := newFakeExecutor(t, fe, 10000)
	_, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "r4",
	})
	if err == nil || !strings.Contains(err.Error(), "重试 3 次") {
		t.Fatalf("重试耗尽必须报错并注明次数: %v", err)
	}
}

func toooLateErr() error { return errors.New("reset by peer") }

func TestSubmitKillTrippedAbortsRetry(t *testing.T) {
	fe := &fakeEx{placeErrs: []error{errors.New("timeout"), errors.New("timeout"), errors.New("timeout"), errors.New("timeout")}}
	e := newFakeExecutor(t, fe, 10000)
	req := exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "r5",
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.Rk.Kill.Trip("演练")
	}()
	if _, err := e.Submit(context.Background(), req); err == nil {
		t.Fatal("Kill Switch 触发应中止重试")
	}
}

func TestSubmitExchangeRejected(t *testing.T) {
	// 交易所直接返回拒单状态
	fe2 := &rejectEx{}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e9, MaxDailyNotionalUSD: 1e9, MaxPositionNotionalUSD: 1e9, MaxOrdersPerMinute: 1 << 30, MaxDailyLossPct: 100}, pf, "")
	e2 := New(fe2, rk, pf, nil)
	if _, err := e2.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "rj",
	}); err == nil {
		t.Fatal("交易所拒单必须报错")
	}
	evs := e2.Events(0)
	if len(evs) == 0 || evs[len(evs)-1].Kind != "rejected" {
		t.Fatalf("拒单事件必须留痕: %+v", evs)
	}
}

type rejectEx struct{ fakeEx }

func (r *rejectEx) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	return exchange.Order{
		Exchange: "fake", Symbol: req.Symbol, OrderID: "x1", ClientOrderID: req.ClientOrderID,
		Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty, Status: exchange.StatusRejected,
	}, nil
}

func TestSubmitFillBackfillsLedger(t *testing.T) {
	// 下单立即全部成交（如市价单）：登记时应把已有成交补进组合账本
	fe := &filledEx{}
	pf := portfolio.New(10000)
	pf.UpdateMark("BTC-USDT", 100) // 市价单风控需要标记价
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e9, MaxDailyNotionalUSD: 1e9, MaxPositionNotionalUSD: 1e9, MaxOrdersPerMinute: 1 << 30, MaxDailyLossPct: 100}, pf, "")
	e := New(fe, rk, pf, nil)
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket,
		Price: 0, Qty: 2, ClientOrderID: "imm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.FilledQty != 2 {
		t.Fatalf("成交数量错误: %+v", o)
	}
	_, positions, _ := pf.Snapshot()
	found := false
	for _, p := range positions {
		if p.Symbol == "BTC-USDT" && p.Qty == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("立即成交的订单必须同步进组合账本")
	}
	if len(e.OpenOrders()) != 0 {
		t.Fatal("已成交订单不应留在挂单列表")
	}
}

type filledEx struct{ fakeEx }

func (f *filledEx) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	return exchange.Order{
		Exchange: "fake", Symbol: req.Symbol, OrderID: "f1", ClientOrderID: req.ClientOrderID,
		Side: req.Side, Type: req.Type, Qty: req.Qty, FilledQty: req.Qty, AvgPrice: 100,
		Status: exchange.StatusFilled,
	}, nil
}

func TestSubmitEventCallback(t *testing.T) {
	fe := &fakeEx{}
	var kinds []string
	var mu sync.Mutex
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e9, MaxDailyNotionalUSD: 1e9, MaxPositionNotionalUSD: 1e9, MaxOrdersPerMinute: 1 << 30, MaxDailyLossPct: 100}, pf, "")
	e := New(fe, rk, pf, func(ev Event) {
		mu.Lock()
		kinds = append(kinds, ev.Kind)
		mu.Unlock()
	})
	fe.orders = map[string]exchange.Order{}
	fe.byClient = map[string]exchange.Order{}
	fe.getOrderErr = map[string]error{}
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "cb1",
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) == 0 || kinds[0] != "submitted" {
		t.Fatalf("onEvent 回调必须收到 submitted: %v", kinds)
	}
}

func TestEventsLimit(t *testing.T) {
	e, _ := newExecutor(t, 10000)
	for i := 0; i < 5; i++ {
		if _, err := e.Submit(context.Background(), exchange.OrderRequest{
			Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
			Price: 90 - float64(i), Qty: 1, ClientOrderID: fmt.Sprintf("ev-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if evs := e.Events(3); len(evs) != 3 {
		t.Fatalf("limit=3 应只取最近 3 条: %d", len(evs))
	}
	if all := e.Events(0); len(all) != 5 {
		t.Fatalf("limit<=0 应返回全部: %d", len(all))
	}
	if huge := e.Events(100); len(huge) != 5 {
		t.Fatalf("limit 超过总数应返回全部: %d", len(huge))
	}
}

func TestCancelPropagatesError(t *testing.T) {
	fe := &fakeEx{}
	e := newFakeExecutor(t, fe, 10000)
	fe.cancelErr = errors.New("cancel rejected by exchange")
	if err := e.Cancel(context.Background(), "BTC-USDT", "whatever"); err == nil {
		t.Fatal("撤单失败必须传播错误")
	}
}

func TestCancelAll(t *testing.T) {
	fe := &fakeEx{open: []exchange.Order{{OrderID: "a"}, {OrderID: "b"}}}
	e := newFakeExecutor(t, fe, 10000)
	if n := e.CancelAll(context.Background(), "BTC-USDT"); n != 2 {
		t.Fatalf("应撤掉 2 张: %d", n)
	}
	fe.openErr = errors.New("fetch failed")
	if n := e.CancelAll(context.Background(), "BTC-USDT"); n != 0 {
		t.Fatalf("拉取失败应返回 0: %d", n)
	}
}

func TestReconcileOnceSkipsBrokenOrder(t *testing.T) {
	fe := &fakeEx{}
	e := newFakeExecutor(t, fe, 10000)
	// 手工注入一张挂单到执行器（通过正常 Submit）
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 1, ClientOrderID: "rc1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// GetOrder 报错 → 跳过且不崩溃
	fe.getOrderErr[o.OrderID] = errors.New("boom")
	e.ReconcileOnce(context.Background())
	if len(e.OpenOrders()) != 1 {
		t.Fatal("同步失败的订单应保留在挂单列表待下次重试")
	}
	// 恢复后部分成交 → 推进账本
	partial := o
	partial.Status = exchange.StatusPartiallyFilled
	partial.FilledQty = 0.5
	partial.AvgPrice = 100
	fe.getOrderErr[o.OrderID] = nil
	fe.orders[o.OrderID] = partial
	e.ReconcileOnce(context.Background())
	_, positions, _ := e.Pf.Snapshot()
	var qty float64
	for _, p := range positions {
		if p.Symbol == "BTC-USDT" {
			qty = p.Qty
		}
	}
	if qty != 0.5 {
		t.Fatalf("部分成交必须推进账本: %v", qty)
	}
	evs := e.Events(1)
	if len(evs) == 0 || evs[0].DeltaQty != 0.5 {
		t.Fatalf("成交事件应带增量: %+v", evs)
	}
}

func TestReconcileLoopStops(t *testing.T) {
	fe := &fakeEx{}
	e := newFakeExecutor(t, fe, 10000)
	done := make(chan struct{})
	go func() { e.ReconcileLoop(); close(done) }()
	e.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 必须终止对账循环")
	}
}

func TestLookupClientUnknown(t *testing.T) {
	fe := &fakeEx{}
	e := newFakeExecutor(t, fe, 10000)
	if o := e.lookupClient("nope"); o.OrderID != "" {
		t.Fatal("未知 clientOrderID 应返回零值")
	}
}

func TestRetryableClassification(t *testing.T) {
	retry := []error{
		errors.New("Request timeout"),
		errors.New("connection refused"),
		errors.New("unexpected EOF"),
		errors.New("connection reset by peer"),
		errors.New("context deadline exceeded"),
	}
	for _, err := range retry {
		if !retryable(err) {
			t.Errorf("应判定可重试: %v", err)
		}
	}
	if retryable(nil) {
		t.Fatal("nil 不可重试")
	}
	if retryable(errors.New("invalid api key")) {
		t.Fatal("业务错误不可重试")
	}
}

func TestRegisterFreezeFailureRollsBack(t *testing.T) {
	// CheckOrder 通过后、Freeze 前现金被并发抽走 → 登记失败必须回滚订单簿
	pf := portfolio.New(10000)
	pf.UpdateMark("BTC-USDT", 100)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e9, MaxDailyNotionalUSD: 1e9, MaxPositionNotionalUSD: 1e9, MaxOrdersPerMinute: 1 << 30, MaxDailyLossPct: 100}, pf, "")
	var e *Executor
	steal := &stealingEx{onPlace: func() {
		// PlaceOrder 成功返回瞬间抽干现金（模拟并发成交）
		pf.ApplyFill(exchange.Fill{Symbol: "ETH-USDT", Side: exchange.Buy, Qty: 9999, Price: 1, Fee: 0})
	}}
	e = New(steal, rk, pf, nil)
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 100, Qty: 50, ClientOrderID: "st1",
	}); err == nil {
		t.Fatal("冻结失败必须报错")
	}
	if len(e.OpenOrders()) != 0 {
		t.Fatal("冻结失败的订单必须从订单簿回滚")
	}
	evs := e.Events(0)
	if len(evs) == 0 || evs[len(evs)-1].Kind != "rejected" {
		t.Fatalf("应留下 rejected 事件: %+v", evs)
	}
}

type stealingEx struct {
	fakeEx
	onPlace func()
}

func (s *stealingEx) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	s.onPlace()
	return exchange.Order{
		Exchange: "fake", Symbol: req.Symbol, OrderID: "st-1", ClientOrderID: req.ClientOrderID,
		Side: req.Side, Type: req.Type, Price: req.Price, Qty: req.Qty, Status: exchange.StatusSubmitted,
	}, nil
}

func TestApplyUpdateUnknownOrderIgnored(t *testing.T) {
	e, _ := newExecutor(t, 10000)
	e.applyUpdate(exchange.Order{OrderID: "ghost", Status: exchange.StatusFilled}) // 未知订单应被忽略
	if len(e.OpenOrders()) != 0 {
		t.Fatal("未知订单不得进入订单簿")
	}
}
