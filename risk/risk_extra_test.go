package risk

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
)

func TestCheckOrderInvalidParams(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	cases := []struct {
		req  exchange.OrderRequest
		rule string
	}{
		{exchange.OrderRequest{Side: exchange.Buy, Type: exchange.OrderLimit, Price: 1, Qty: 1}, "INVALID_SYMBOL"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Type: exchange.OrderLimit, Price: 1, Qty: 1}, "INVALID_SIDE"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Price: 1, Qty: 1}, "INVALID_TYPE"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 1, Qty: 0}, "INVALID_QTY"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 1, Qty: math.NaN()}, "INVALID_QTY"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: 0, Qty: 1}, "INVALID_PRICE"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: -1, Qty: 1}, "INVALID_PRICE"},
		{exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 1}, "INVALID_MARK"},
	}
	for i, c := range cases {
		// 最后一例：市价单标记价非法
		mark := 100.0
		if i == len(cases)-1 {
			mark = math.NaN()
		}
		err := m.CheckOrder(c.req, mark)
		if err == nil || !strings.Contains(err.Error(), c.rule) {
			t.Errorf("case %d 应被 %s 拒绝: %v", i, c.rule, err)
		}
	}
	if rs := m.Rejections(); len(rs) != len(cases) {
		t.Fatalf("拒单台账应全部留痕: %d", len(rs))
	}
	// 有效标记价的市价单合法（对照组）
	m2 := NewManager(testLimits(), portfolio.New(100000), "")
	if err := m2.CheckOrder(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 0.001}, 100); err != nil {
		t.Fatalf("有效标记价的市价单应放行: %v", err)
	}
}

func TestMarketOrderUsesMarkPrice(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	// 市价单按标记价计名义：qty 0.03 × mark 100 = 3，合法
	if err := m.CheckOrder(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderMarket, Qty: 0.03}, 100); err != nil {
		t.Fatalf("市价单应按标记价校验: %v", err)
	}
	if m.DailyNotionalUsed() != 3 {
		t.Fatalf("名义累计应按标记价: %v", m.DailyNotionalUsed())
	}
}

func TestCooldownBlocksSubsequent(t *testing.T) {
	l := testLimits()
	l.CooldownAfterRejectSec = 3600
	m := NewManager(l, portfolio.New(100000), "")
	// 触发一次拒单（敞口超限）并进入冷静期
	if err := m.CheckOrder(buyReq(1, 100000), 100000); err == nil {
		t.Fatal("预设拒单失败")
	}
	m.StartCooldown()
	// 冷静期内正常单也被拒
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err == nil || !strings.Contains(err.Error(), "COOLDOWN") {
		t.Fatalf("冷静期内必须拒单: %v", err)
	}
}

func TestCooldownDisabledWhenZero(t *testing.T) {
	l := testLimits()
	l.CooldownAfterRejectSec = 0
	m := NewManager(l, portfolio.New(100000), "")
	m.StartCooldown()
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err != nil {
		t.Fatalf("未配置冷静期时不应拦截: %v", err)
	}
}

func TestReconcileBlock(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	if blocked, _ := m.ReconcileBlocked(); blocked {
		t.Fatal("初始不应处于对账封锁")
	}
	m.BlockForReconcile("余额不一致")
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err == nil || !strings.Contains(err.Error(), "RECONCILE_BLOCKED") {
		t.Fatalf("对账异常必须禁单: %v", err)
	}
	if blocked, reason := m.ReconcileBlocked(); !blocked || reason != "余额不一致" {
		t.Fatalf("对账状态错误: %v %s", blocked, reason)
	}
	m.ClearReconcileBlock()
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err != nil {
		t.Fatalf("解除对账封锁后应放行: %v", err)
	}
}

func TestRejectionLedgerPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rejections.jsonl")
	m := NewManager(testLimits(), portfolio.New(100000), path)
	if err := m.CheckOrder(buyReq(1, 100000), 100000); err == nil {
		t.Fatal("预设拒单失败")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("拒单台账必须落盘: %v", err)
	}
	var r Rejection
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("台账格式必须为 JSONL: %v", err)
	}
	if r.RuleID != "MAX_ORDER_NOTIONAL" {
		t.Fatalf("台账规则 ID 错误: %s", r.RuleID)
	}
}

func TestInsufficientCashRejected(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(10), "")
	if err := m.CheckOrder(buyReq(1, 100), 100); err == nil || !strings.Contains(err.Error(), "INSUFFICIENT_CASH") {
		t.Fatalf("现金不足必须拒单: %v", err)
	}
}

func TestSellUnknownSymbolRejected(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	req := exchange.OrderRequest{Symbol: "ETH-USDT", Side: exchange.Sell, Type: exchange.OrderLimit, Price: 100, Qty: 1}
	if err := m.CheckOrder(req, 100); err == nil || !strings.Contains(err.Error(), "INSUFFICIENT_AVAILABLE") {
		t.Fatalf("无持仓卖出必须拒单: %v", err)
	}
}

func TestKillSwitchLifecycle(t *testing.T) {
	var k KillSwitch
	if k.Reason() != "" || k.Tripped() {
		t.Fatal("初始状态应为未触发")
	}
	var called []string
	var mu sync.Mutex
	k.OnTrip(func(r string) { mu.Lock(); called = append(called, r); mu.Unlock() })
	k.OnTrip(nil) // nil 监听器应被忽略
	k.Trip("")    // 空 reason 补默认文案
	if !k.Tripped() || k.Reason() != "未说明原因" {
		t.Fatalf("空 reason 应补默认: %q", k.Reason())
	}
	mu.Lock()
	if len(called) != 1 || called[0] != "未说明原因" {
		t.Fatalf("监听器应恰好回调一次: %v", called)
	}
	mu.Unlock()
	k.Trip("again") // 重复触发不再回调
	mu.Lock()
	if len(called) != 1 {
		t.Fatalf("重复 Trip 不应重复回调: %v", called)
	}
	mu.Unlock()
	// Restore 恢复历史状态（重启场景）
	k.Restore(true, "重启恢复")
	if !k.Tripped() || k.Reason() != "重启恢复" {
		t.Fatal("Restore 应恢复触发状态")
	}
	// Restore 后再 Trip 不回调（已是触发态）
	k.Trip("x")
	k.Reset()
	if k.Tripped() || k.Reason() != "" {
		t.Fatal("Reset 应清空状态")
	}
}

func TestSetDayStartEquity(t *testing.T) {
	pf := portfolio.New(1000)
	m := NewManager(testLimits(), pf, "")
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, Fee: 0})
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Sell, FilledQty: 1, AvgPrice: 40, Fee: 0})
	// 当前权益 940；把起始权益重置为当前值 → 未回撤，不触发
	m.SetDayStartEquity(940)
	if err := m.CheckOrder(buyReq(0.001, 50), 50); err != nil {
		t.Fatalf("起始权益对齐当前值应放行: %v", err)
	}
	m.SetDayStartEquity(1000) // 恢复日初基准 → 940 < 950 超过 5% 回撤
	if err := m.CheckOrder(buyReq(0.001, 50), 50); err == nil || !strings.Contains(err.Error(), "MAX_DAILY_LOSS") {
		t.Fatalf("回撤超限必须停机: %v", err)
	}
}

func TestDayStartZeroSkipsLossCheck(t *testing.T) {
	pf := portfolio.New(100)
	m := NewManager(testLimits(), pf, "")
	m.SetDayStartEquity(0) // 起始权益 0：跳过回撤检查（除零保护）
	if err := m.CheckOrder(buyReq(0.001, 1), 1); err != nil {
		t.Fatalf("起始权益为 0 时跳过回撤检查: %v", err)
	}
}

func TestCheckOrderConcurrent(t *testing.T) {
	m := NewManager(Limits{
		MaxOrderNotionalUSD: 1e12, MaxDailyNotionalUSD: 1e12, MaxPositionNotionalUSD: 1e12,
		MaxOrdersPerMinute: 1 << 30, MaxDailyLossPct: 100,
	}, portfolio.New(1e12), "")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.CheckOrder(buyReq(0.001, 100), 100)
			_ = m.DailyNotionalUsed()
			_ = m.Rejections()
		}()
	}
	wg.Wait()
}

func TestRateWindowSlides(t *testing.T) {
	l := testLimits()
	l.MaxOrderNotionalUSD = 1e9
	l.MaxDailyNotionalUSD = 1e9
	l.MaxPositionNotionalUSD = 1e9
	m := &Manager{Limits: l, Pf: portfolio.New(1e9), dayStartEq: 1e9, orderTimes: []time.Time{time.Now().Add(-2 * time.Minute)}}
	m.rollDayIfNeeded()
	// 2 分钟前的记录应滑出窗口
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err != nil {
		t.Fatalf("过期频率记录应滑出: %v", err)
	}
}

func TestRejectedOrderNotCountedDailyNotional(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	_ = m.CheckOrder(buyReq(1, 100000), 100000) // 被拒
	if m.DailyNotionalUsed() != 0 {
		t.Fatalf("拒单不应计入当日名义: %v", m.DailyNotionalUsed())
	}
}

// 随机种子防回归：随机参数下单，引擎不应 panic。
func TestCheckOrderFuzzNoPanic(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(1000), "")
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		req := exchange.OrderRequest{
			Symbol: "BTC-USDT",
			Side:   exchange.Side([]string{"buy", "sell", "junk"}[r.Intn(3)]),
			Type:   exchange.OrderType([]string{"limit", "market", "stop"}[r.Intn(3)]),
			Price:  r.Float64() * 1e6,
			Qty:    (r.Float64() - 0.5) * 10,
		}
		_ = m.CheckOrder(req, r.Float64()*1e5)
	}
}

func TestRejectionsCapped(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	for i := 0; i < maxRejections+500; i++ {
		_ = m.CheckOrder(buyReq(1, 100000), 100000) // 必然拒单（敞口超限）
	}
	if rs := m.Rejections(); len(rs) != maxRejections {
		t.Fatalf("内存台账应封顶 %d: %d", maxRejections, len(rs))
	}
}
