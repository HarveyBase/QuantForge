// lifecycle_test.go 生产实盘改造 T5 的生命周期测试：
// 启动恢复编排 / 强制对账拦截 / 日内权益基线跨重启 / 优雅退出 / 断流告警。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/execution"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/state"
)

// lifeMock 可变状态的 OKX mock：余额/挂单/最新价可在线调整，撤单计数可观测。
type lifeMock struct {
	mu         sync.Mutex
	usdtTotal  float64
	usdtAvail  float64
	btcTotal   float64
	btcAvail   float64
	tickerLast float64
	pending    []map[string]string // okx orders-pending 原始行
	cancels    int
	placed     int
	autoHold   bool // 下单自动进 pending（模拟真实挂单簿）
	// 测试编排辅助
	hits            []string // 端点命中顺序（RISK-2 启动顺序断言用）
	balanceCalls    int      // balance 端点调用计数
	balanceFailFrom int      // >0 且调用序号 ≥ 该值时 balance 返回错误（0=不失败）
	cancelFail      bool     // cancel-order 返回错误（测撤单完整性告警）
}

func newLifeMock() *lifeMock {
	return &lifeMock{
		usdtTotal: 10000, usdtAvail: 10000,
		btcTotal: 0.05, btcAvail: 0.05,
		tickerLast: 100,
		autoHold:   true,
	}
}

func (m *lifeMock) setBalances(usdtTotal, usdtAvail, btcTotal, btcAvail float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usdtTotal, m.usdtAvail = usdtTotal, usdtAvail
	m.btcTotal, m.btcAvail = btcTotal, btcAvail
}

func (m *lifeMock) setTicker(last float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tickerLast = last
}

func (m *lifeMock) addPending(row map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, row)
}

func (m *lifeMock) cancelCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancels
}

func (m *lifeMock) placeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.placed
}

// setBalanceFailFrom 第 n 次及之后的 balance 调用返回错误（0=恢复成功）。
func (m *lifeMock) setBalanceFailFrom(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balanceFailFrom = n
}

// setCancelFail 控制 cancel-order 是否返回错误。
func (m *lifeMock) setCancelFail(b bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelFail = b
}

// firstHit 端点首次命中的序号（-1 = 未命中）。
func (m *lifeMock) firstHit(ep string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, h := range m.hits {
		if h == ep {
			return i
		}
	}
	return -1
}

// newLifeServer 启动 mock OKX 服务并返回绑定它的 paper 配置。
func newLifeServer(t *testing.T) (*lifeMock, *config.Config) {
	t.Helper()
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	m := newLifeMock()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/time", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"ts": fmt.Sprintf("%d", time.Now().UnixMilli())},
		}})
	})
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		last := m.tickerLast
		m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"instId": "BTC-USDT", "last": fmt.Sprintf("%g", last), "bidPx": fmt.Sprintf("%g", last-0.5),
				"askPx": fmt.Sprintf("%g", last+0.5), "ts": fmt.Sprintf("%d", time.Now().UnixMilli())},
		}})
	})
	mux.HandleFunc("/api/v5/market/candles", func(w http.ResponseWriter, r *http.Request) {
		var rows [][]string
		for i := 300; i >= 1; i-- {
			ot := int64(3600000 * i)
			px := 100 + float64(i%7)
			rows = append(rows, []string{
				fmt.Sprintf("%d", ot), fmt.Sprintf("%g", px),
				fmt.Sprintf("%g", px+1), fmt.Sprintf("%g", px-1), fmt.Sprintf("%g", px),
				"1", "0", "0", "1",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": rows})
	})
	mux.HandleFunc("/api/v5/account/balance", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.balanceCalls++
		fail := m.balanceFailFrom > 0 && m.balanceCalls >= m.balanceFailFrom
		usdt, usdtA, btc, btcA := m.usdtTotal, m.usdtAvail, m.btcTotal, m.btcAvail
		m.hits = append(m.hits, "balance")
		m.mu.Unlock()
		if fail {
			json.NewEncoder(w).Encode(map[string]any{"code": "1", "msg": "mock balance fail"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]any{{
			"details": []map[string]string{
				{"ccy": "USDT", "availBal": fmt.Sprintf("%g", usdtA), "cashBal": fmt.Sprintf("%g", usdt), "frozenBal": fmt.Sprintf("%g", usdt-usdtA)},
				{"ccy": "BTC", "availBal": fmt.Sprintf("%g", btcA), "cashBal": fmt.Sprintf("%g", btc), "frozenBal": fmt.Sprintf("%g", btc-btcA)},
			},
		}}})
	})
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.placed++
		id := fmt.Sprintf("o%d", m.placed)
		if m.autoHold {
			m.pending = append(m.pending, map[string]string{
				"instId": body["instId"], "ordId": id, "clOrdID": body["clOrdID"],
				"state": "live", "side": body["side"], "ordType": body["ordType"],
				"px": body["px"], "sz": body["sz"], "accFillSz": "0", "avgPx": "",
				"cTime": fmt.Sprintf("%d", time.Now().UnixMilli()-60000),
				"uTime": fmt.Sprintf("%d", time.Now().UnixMilli()),
			})
		}
		m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"ordId": id, "clOrdID": body["clOrdID"], "sCode": "0", "sMsg": ""},
		}})
	})
	mux.HandleFunc("/api/v5/trade/orders-pending", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits = append(m.hits, "orders-pending")
		rows := append([]map[string]string(nil), m.pending...)
		m.mu.Unlock()
		if rows == nil {
			rows = []map[string]string{}
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": rows})
	})
	mux.HandleFunc("/api/v5/trade/cancel-order", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		fail := m.cancelFail
		if !fail {
			m.cancels++
			kept := m.pending[:0]
			for _, row := range m.pending {
				if row["ordId"] != body["ordId"] {
					kept = append(kept, row)
				}
			}
			m.pending = kept
		}
		m.mu.Unlock()
		if fail {
			json.NewEncoder(w).Encode(map[string]any{"code": "1", "msg": "mock cancel fail"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"ordId": body["ordId"], "sCode": "0", "sMsg": ""},
		}})
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"instId": "BTC-USDT", "baseCcy": "BTC", "quoteCcy": "USDT", "lotSz": "0.00000001", "minSz": "0.00001", "tickSz": "0.1"},
		}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Mode = config.ModePaper
	cfg.Exchange.RestURL = srv.URL
	cfg.DataDir = t.TempDir()
	cfg.Ops.RestRateLimitPerSec = 50 // 测试加速（校验范围 [1,50]）
	return m, cfg
}

// pendingRow 造一条 okx 在途单原始行。
func pendingRow(ordID, clOrdID string) map[string]string {
	return map[string]string{
		"instId": "BTC-USDT", "ordId": ordID, "clOrdID": clOrdID,
		"state": "live", "side": "buy", "ordType": "limit",
		"px": "90", "sz": "0.01", "accFillSz": "0", "avgPx": "",
		"cTime": "1700000000000", "uTime": "1700000000000",
	}
}

// TestStartupRecoveryRestoresJournalAndAdoptsOrphans 启动恢复编排：
// journal 残留挂单 + 交易所在途单 → 恢复后本地订单簿与交易所一致（孤儿单被认领）。
func TestStartupRecoveryRestoresJournalAndAdoptsOrphans(t *testing.T) {
	m, cfg := newLifeServer(t)
	// journal 残留：上次运行留下的在途单 ex-known
	jpath := filepath.Join(cfg.DataDir, "state", "orders.jsonl")
	j, err := execution.NewJournal(jpath)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(execution.JournalEvent{Ts: time.Now(), Ev: "register", Order: exchange.Order{
		Exchange: "okx", Symbol: "BTC-USDT", OrderID: "ex-known", ClientOrderID: "qf-known",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 90, Qty: 0.01, Status: exchange.StatusSubmitted,
	}}); err != nil {
		t.Fatal(err)
	}
	// 交易所在途：ex-known（本地已知，对齐）+ ex-orphan（孤儿单，认领）
	m.addPending(pendingRow("ex-known", "qf-known"))
	m.addPending(pendingRow("ex-orphan", "manual-1"))

	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	open := a.exec.OpenOrders()
	got := map[string]bool{}
	for _, o := range open {
		got[o.OrderID] = true
	}
	if !got["ex-known"] || !got["ex-orphan"] {
		t.Fatalf("启动恢复后订单簿应含 journal 残留单与交易所孤儿单: %+v", open)
	}
	// 幂等键被认领：重启后不得重复使用同一 clientOrderID（防重复下单）
	if _, err := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 90, Qty: 0.01, ClientOrderID: "qf-known",
	}); err == nil {
		t.Fatal("journal 恢复的 clientOrderID 必须标记 claimed 防重发")
	}
	// 采纳事件留痕（journal 落 adopted，审计可查）
	events, skipped, err := execution.LoadJournal(jpath)
	if err != nil || skipped != 0 {
		t.Fatalf("journal 重读失败: %v skipped=%d", err, skipped)
	}
	adopted := 0
	for _, ev := range events {
		if ev.Ev == "adopted" && ev.Order.OrderID == "ex-orphan" {
			adopted++
		}
	}
	if adopted != 1 {
		t.Fatalf("孤儿单认领必须在 journal 留 adopted 事件: %d", adopted)
	}
}

// TestForcedReconcileBlocksAndClears 强制对账：构造差异 → blocked + 下单被
// RECONCILE_BLOCK 拦截；恢复一致 → 拦截自动解除。
func TestForcedReconcileBlocksAndClears(t *testing.T) {
	m, cfg := newLifeServer(t)
	cfg.Risk.CooldownAfterRejectSec = 0 // 拒单默认带 30s 冷静期，会盖住"解除后可下单"断言
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if blocked, _ := a.rk.ReconcileBlocked(); blocked {
		t.Fatal("启动对账一致（mock 余额与 Seed 一致）不应拦截")
	}
	// 构造差异：远程 USDT 少 2000（20% >> 容差 0.5%）
	m.setBalances(8000, 8000, 0.05, 0.05)
	rep, ok, err := a.reconcileOnce(context.Background())
	if err != nil || ok {
		t.Fatalf("余额差异必须判不通过: ok=%v err=%v diffs=%+v", ok, err, rep.Diffs)
	}
	blocked, reason := a.rk.ReconcileBlocked()
	if !blocked || !strings.Contains(reason, "USDT") {
		t.Fatalf("差异必须触发对账拦截: blocked=%v reason=%q", blocked, reason)
	}
	// 下单被拦截（任何模式不得绕过风控）
	_, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.1, ClientOrderID: "t-reconcile-1",
	})
	if serr == nil || !strings.Contains(serr.Error(), "RECONCILE_BLOCK") {
		t.Fatalf("对账拦截期间下单必须被拒: %v", serr)
	}
	// 恢复一致 → 自动解除
	m.setBalances(10000, 10000, 0.05, 0.05)
	if _, ok, err := a.reconcileOnce(context.Background()); err != nil || !ok {
		t.Fatalf("恢复一致应判通过: ok=%v err=%v", ok, err)
	}
	if blocked, _ := a.rk.ReconcileBlocked(); blocked {
		t.Fatal("恢复一致后拦截必须解除")
	}
	if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.1, ClientOrderID: "t-reconcile-2",
	}); serr != nil {
		t.Fatalf("解除拦截后下单应恢复: %v", serr)
	}
}

// TestDayStartEquityContinuity 日内权益基线跨重启：同日用旧基线延续、跨日重置。
// mock ticker=100 → Seed 权益 = 10000 现金 + 0.05 BTC × 100 = 10005（mark 接线后
// 基线即刻含持仓市值）。
func TestDayStartEquityContinuity(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")

	// 第一次启动：基线落盘
	_, cfg1 := newLifeServer(t)
	a1, err := buildApp(cfg1)
	if err != nil {
		t.Fatal(err)
	}
	defer a1.exec.Stop()
	day, eq := a1.rk.EquityBaseline()
	if day != today || eq != 10005 {
		t.Fatalf("Seed 后基线应为今日 %.2f（mark=100 含持仓市值）: %s %.2f", 10005.0, day, eq)
	}
	// 第二次启动（同 dataDir、同日）：基线延续落盘值
	_, cfg2 := newLifeServer(t)
	cfg2.DataDir = cfg1.DataDir // 复用同一 state 目录
	tamper := state.Runtime{Version: 1, Day: today, DayStartEq: 7777}
	if err := state.New(cfg2.DataDir).Save(tamper); err != nil {
		t.Fatal(err)
	}
	a2, err := buildApp(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	defer a2.exec.Stop()
	if day, eq := a2.rk.EquityBaseline(); day != today || eq != 7777 {
		t.Fatalf("同日重启必须延续旧基线 7777: %s %.2f", day, eq)
	}
	// 第三次启动（state 里是昨日）：基线重置为 Seed 后真实权益
	m3, cfg3 := newLifeServer(t)
	cfg3.DataDir = cfg1.DataDir
	m3.setTicker(200) // 新价 → 新基线 10000 + 0.05×200 = 10010
	if err := state.New(cfg3.DataDir).Save(state.Runtime{Version: 1, Day: "2000-01-01", DayStartEq: 7777}); err != nil {
		t.Fatal(err)
	}
	a3, err := buildApp(cfg3)
	if err != nil {
		t.Fatal(err)
	}
	defer a3.exec.Stop()
	if day, eq := a3.rk.EquityBaseline(); day != today || eq != 10010 {
		t.Fatalf("跨日重启必须重置基线为当前权益 10010: %s %.2f", day, eq)
	}
}

// TestGracefulShutdownCancelsAndPersists 优雅退出：撤单被执行 + 最新态落盘。
func TestGracefulShutdownCancelsAndPersists(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
			Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
			Price: 50, Qty: 0.1, ClientOrderID: fmt.Sprintf("t-shutdown-%d", i),
		}); serr != nil {
			t.Fatalf("前置下单失败: %v", serr)
		}
	}
	if n := len(a.exec.OpenOrders()); n != 2 {
		t.Fatalf("前置失败：应有 2 张挂单: %d", n)
	}
	a.gracefulShutdown(nil)
	if c := m.cancelCount(); c != 2 {
		t.Fatalf("优雅退出应撤掉全部挂单: %d", c)
	}
	if n := len(a.exec.OpenOrders()); n != 0 {
		t.Fatalf("撤单后订单簿应清空: %d", n)
	}
	// state 落盘（含日内基线）
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Day != time.Now().UTC().Format("2006-01-02") || st.DayStartEq != 10005 {
		t.Fatalf("退出落盘应含日内基线: %+v", st)
	}
}

// TestGracefulShutdownKeepsOrdersWhenConfigured ShutdownCancelOrders=false：
// 挂单留守（重启后由 AdoptOpenOrders 认领），不撤单。
func TestGracefulShutdownKeepsOrdersWhenConfigured(t *testing.T) {
	m, cfg := newLifeServer(t)
	cfg.Ops.ShutdownCancelOrders = false
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.1, ClientOrderID: "t-keep-1",
	}); serr != nil {
		t.Fatalf("前置下单失败: %v", serr)
	}
	a.gracefulShutdown(nil)
	if c := m.cancelCount(); c != 0 {
		t.Fatalf("shutdown_cancel_orders=false 不得撤单: %d", c)
	}
	if n := len(a.exec.OpenOrders()); n != 1 {
		t.Fatalf("挂单应留守: %d", n)
	}
}

// captureNotifier 测试用告警捕获器。
type captureNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureNotifier) Send(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, text)
}
func (c *captureNotifier) Enabled() bool { return true }
func (c *captureNotifier) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

// TestFeedErrorAlerting 断流计数器：3 次失败告警（节流内不重复）、恢复清零。
func TestFeedErrorAlerting(t *testing.T) {
	cfg := mockOKX(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cap := &captureNotifier{}
	a.notifier = cap
	// 1-2 次失败：计数但未到阈值
	a.onFeedError(errors.New("timeout 1"))
	a.onFeedError(errors.New("timeout 2"))
	if got := cap.sent(); len(got) != 0 {
		t.Fatalf("未达 3 次不应告警: %v", got)
	}
	// 第 3 次：告警
	a.onFeedError(errors.New("timeout 3"))
	msgs := cap.sent()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "连续失败 3 次") {
		t.Fatalf("第 3 次失败应告警: %v", msgs)
	}
	// 第 4 次：仍在节流窗内，不重复告警（计数继续涨）
	a.onFeedError(errors.New("timeout 4"))
	if got := cap.sent(); len(got) != 1 {
		t.Fatalf("10 分钟节流窗内不应重复告警: %v", got)
	}
	if n := a.feedErrs.Load(); n != 4 {
		t.Fatalf("连续失败计数应为 4: %d", n)
	}
	// 恢复（下一次成功拉取）：计数清零
	a.onFeedUpdate(nil)
	if n := a.feedErrs.Load(); n != 0 {
		t.Fatalf("恢复后计数应清零: %d", n)
	}
	// 再失败 1 次：从头计数，不告警
	a.onFeedError(errors.New("timeout 5"))
	if got := cap.sent(); len(got) != 1 {
		t.Fatalf("恢复后单次失败不应告警: %v", got)
	}
}

// TestAlertThrottleWindow 节流器窗口语义单测。
func TestAlertThrottleWindow(t *testing.T) {
	th := alertThrottle{every: 30 * time.Millisecond}
	if !th.allow() {
		t.Fatal("首次应放行")
	}
	if th.allow() {
		t.Fatal("窗口内第二次应拒绝")
	}
	time.Sleep(40 * time.Millisecond)
	if !th.allow() {
		t.Fatal("窗口过后应再次放行")
	}
}

// TestReconcileWithinTolerance 对账容差判定层单测：内禀 Ok 直接过、粉尘豁免、
// 配置容差内放行、超容差拒绝。
func TestReconcileWithinTolerance(t *testing.T) {
	if !reconcileWithinTolerance(portfolio.ReconcileReport{Ok: true}, 0.5) {
		t.Fatal("Ok=true 应直接通过")
	}
	// 粉尘（0.3 USDT）：放行
	dust := portfolio.ReconcileReport{Diffs: []portfolio.ReconcileDiff{{Kind: "cash", Item: "USDT", Local: 10000, Remote: 10000.3, Diff: 0.3}}}
	if !reconcileWithinTolerance(dust, 0.5) {
		t.Fatal("粉尘级差异（≤0.5）应豁免")
	}
	// 容差内（0.4% < 0.5%）：放行
	small := portfolio.ReconcileReport{Diffs: []portfolio.ReconcileDiff{{Kind: "position", Item: "BTC-USDT", Local: 1, Remote: 1.004, Diff: 0.004}}}
	if !reconcileWithinTolerance(small, 0.5) {
		t.Fatal("配置容差内的差异应放行")
	}
	// 超容差（20%）：拒绝
	big := portfolio.ReconcileReport{Diffs: []portfolio.ReconcileDiff{{Kind: "cash", Item: "USDT", Local: 10000, Remote: 8000, Diff: -2000}}}
	if reconcileWithinTolerance(big, 0.5) {
		t.Fatal("超容差差异必须拒绝")
	}
	// 一项放行一项超限：整体拒绝
	mixed := portfolio.ReconcileReport{Diffs: []portfolio.ReconcileDiff{
		{Kind: "cash", Item: "USDT", Local: 10000, Remote: 10000.3, Diff: 0.3},
		{Kind: "cash", Item: "USDT", Local: 10000, Remote: 8000, Diff: -2000},
	}}
	if reconcileWithinTolerance(mixed, 0.5) {
		t.Fatal("任一差异超容差即整体拒绝")
	}
}

// TestStartupPersistsStateImmediately 交易模式启动即落盘（防启动后崩溃丢基线）。
func TestStartupPersistsStateImmediately(t *testing.T) {
	_, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "state", "runtime.json")); err != nil {
		t.Fatalf("paper 启动后 state 应立即落盘: %v", err)
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Day == "" || st.DayStartEq != 10005 {
		t.Fatalf("落盘应含日内基线: %+v", st)
	}
}

// TestStartupBalanceFetchAfterAdopt RISK-2 回归：余额拉取（T1）必须晚于在途单
// 认领（T0 拉挂单）——(T0,T1] 窗口内的成交已含在 T1 余额里，重建账本无丢失窗口。
// 修复前 balance 在 buildApp 顶部先拉（早于 adopt），断言 firstHit 顺序可捕获。
func TestStartupBalanceFetchAfterAdopt(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if i := m.firstHit("orders-pending"); i < 0 {
		t.Fatal("paper 启动必须拉取在途单（adopt）")
	}
	if b, p := m.firstHit("balance"), m.firstHit("orders-pending"); b < p {
		t.Fatalf("余额拉取必须晚于在途单认领（balance@%d < orders-pending@%d）——先拉余额会丢 (T0,T1] 窗口成交", b, p)
	}
}

// TestReconcileNeverSucceededBlocks RISK-3 回归：从未成功对账（lastRec 零值）+
// 余额拉取失败 → fail-closed 拦截下单（修复前 fail-open 照常交易）。
func TestReconcileNeverSucceededBlocks(t *testing.T) {
	m, cfg := newLifeServer(t)
	cfg.Risk.CooldownAfterRejectSec = 0 // 拒单默认带 30s 冷静期，会盖住后续断言
	// 第 1 次 balance（Seed 拉取）成功、第 2 次（启动强制对账）失败 → lastRec 零值
	m.setBalanceFailFrom(2)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	blocked, reason := a.rk.ReconcileBlocked()
	if !blocked || !strings.Contains(reason, "对账从未成功") {
		t.Fatalf("从未成功对账必须拦截下单: blocked=%v reason=%q", blocked, reason)
	}
	if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.01, ClientOrderID: "t-neverrec-1",
	}); serr == nil || !strings.Contains(serr.Error(), "RECONCILE_BLOCK") {
		t.Fatalf("对账从未成功期间下单必须被拒: %v", serr)
	}
	// 拉取恢复 + 对账一致 → 自动解除
	m.setBalanceFailFrom(0)
	if _, ok, err := a.reconcileOnce(context.Background()); err != nil || !ok {
		t.Fatalf("恢复后对账应通过: ok=%v err=%v", ok, err)
	}
	if blocked, _ := a.rk.ReconcileBlocked(); blocked {
		t.Fatal("对账成功后拦截必须解除")
	}
}

// TestReconcileConsecutiveFailuresBlock RISK-3：成功过一次后短暂拉取失败宽容
// （不拦截），连续 ≥3 次 → 拦截；恢复后解除。
func TestReconcileConsecutiveFailuresBlock(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if blocked, _ := a.rk.ReconcileBlocked(); blocked {
		t.Fatal("前置失败：启动对账成功不应拦截")
	}
	// 启动已用掉 2 次 balance 调用，从第 3 次开始失败
	m.setBalanceFailFrom(3)
	for i := 1; i <= 2; i++ {
		if _, _, rerr := a.reconcileOnce(context.Background()); rerr == nil {
			t.Fatalf("第 %d 次应拉取失败", i)
		}
		if blocked, _ := a.rk.ReconcileBlocked(); blocked {
			t.Fatalf("成功过一次后的短暂失败（%d 次）不应拦截", i)
		}
	}
	if _, _, _ = a.reconcileOnce(context.Background()); true {
		blocked, reason := a.rk.ReconcileBlocked()
		if !blocked || !strings.Contains(reason, "连续失败") {
			t.Fatalf("连续 3 次拉取失败必须拦截: blocked=%v reason=%q", blocked, reason)
		}
	}
	// 恢复 → 解除 + 计数清零（下一次单次失败不再立即拦截）
	m.setBalanceFailFrom(0)
	if _, ok, rerr := a.reconcileOnce(context.Background()); rerr != nil || !ok {
		t.Fatalf("恢复后对账应通过: ok=%v err=%v", ok, rerr)
	}
	if blocked, _ := a.rk.ReconcileBlocked(); blocked {
		t.Fatal("恢复后拦截必须解除")
	}
	m.setBalanceFailFrom(99)
	if _, _, _ = a.reconcileOnce(context.Background()); true {
		if blocked, _ := a.rk.ReconcileBlocked(); blocked {
			t.Fatal("计数清零后单次失败不应拦截")
		}
	}
}

// TestKillRestoreCancelsLeftoverOrders RISK-4 回归：Kill 态跨重启恢复时补撤上次
// 遗留挂单（进程被杀撤单链未执行完），不留孤儿单在场。
func TestKillRestoreCancelsLeftoverOrders(t *testing.T) {
	m, cfg := newLifeServer(t)
	// 预置：Kill 已触发 + 交易所遗留一张在途单（上次 Kill 撤单没执行完）
	if err := state.New(cfg.DataDir).Save(state.Runtime{Version: 1, KillTripped: true, KillReason: "演练停机"}); err != nil {
		t.Fatal(err)
	}
	m.addPending(pendingRow("ex-left", "manual-left"))
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if !a.rk.Kill.Tripped() || a.rk.Kill.Reason() != "演练停机" {
		t.Fatal("Kill 态必须恢复")
	}
	if c := m.cancelCount(); c != 1 {
		t.Fatalf("Kill 态恢复应补撤遗留挂单 1 张: %d", c)
	}
	if n := len(a.exec.OpenOrders()); n != 0 {
		t.Fatalf("补撤后本地挂单簿应清空: %d", n)
	}
}

// TestKillRestoreCancelAlerts RISK-4：补撤单必告警留痕；撤单失败（CancelAll 返回
// 0 且期望 >0）追加不完整告警，人工兜底。
func TestKillRestoreCancelAlerts(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	cap := &captureNotifier{}
	a.notifier = cap
	if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.01, ClientOrderID: "t-killrec-1",
	}); serr != nil {
		t.Fatalf("前置下单失败: %v", serr)
	}
	m.setCancelFail(true) // 撤单全失败 → CancelAll 实撤 0
	a.killRestoreCancel("演练停机")
	msgs := cap.sent()
	if len(msgs) != 2 {
		t.Fatalf("应有补撤告警 + 不完整告警各一条: %v", msgs)
	}
	if !strings.Contains(msgs[0], "Kill 态跨重启恢复") || !strings.Contains(msgs[0], "补撤") {
		t.Fatalf("首条应为补撤留痕告警: %q", msgs[0])
	}
	if !strings.Contains(msgs[1], "不完整") || !strings.Contains(msgs[1], "期望 1 实撤 0") {
		t.Fatalf("撤单不完整告警格式错误: %q", msgs[1])
	}
}

// TestGracefulShutdownAlertsOnIncompleteCancel RISK-7 回归：优雅退出撤单失败不得
// 静默——与撤单前本地挂单数比对，不完整即告警。
func TestGracefulShutdownAlertsOnIncompleteCancel(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cap := &captureNotifier{}
	a.notifier = cap
	if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.01, ClientOrderID: "t-shutdown-incomplete",
	}); serr != nil {
		t.Fatalf("前置下单失败: %v", serr)
	}
	m.setCancelFail(true)
	a.gracefulShutdown(nil)
	msgs := cap.sent()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "优雅退出撤单可能不完整") || !strings.Contains(msgs[0], "期望 1 实撤 0") {
		t.Fatalf("撤单不完整必须告警: %v", msgs)
	}
}

// TestKillTripCancelAlertsIncomplete RISK-7：Kill 触发链的撤单同样做完整性比对，
// 失败不静默。
func TestKillTripCancelAlertsIncomplete(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	cap := &captureNotifier{}
	a.notifier = cap
	if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 50, Qty: 0.01, ClientOrderID: "t-killtrip-1",
	}); serr != nil {
		t.Fatalf("前置下单失败: %v", serr)
	}
	m.setCancelFail(true)
	a.rk.Kill.Trip("演练停机")
	// OnTrip 撤单在后台 goroutine，轮询等告警落地
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range cap.sent() {
			if strings.Contains(msg, "Kill 触发撤单可能不完整") && strings.Contains(msg, "期望 1 实撤 0") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Kill 触发撤单不完整必须告警: %v", cap.sent())
}

// TestGracefulShutdownHTTPFirst RISK-6 回归：httpSrv.Shutdown 必须最先执行——
// in-flight handler 未返回（Shutdown 阻塞等待）期间不得开始撤单，drain 期间
// 手动下单等写入口先被关闭；退出后新请求被拒绝。
func TestGracefulShutdownHTTPFirst(t *testing.T) {
	m, cfg := newLifeServer(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	for i := 0; i < 2; i++ {
		if _, serr := a.exec.Submit(context.Background(), exchange.OrderRequest{
			Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
			Price: 50, Qty: 0.01, ClientOrderID: fmt.Sprintf("t-httpfirst-%d", i),
		}); serr != nil {
			t.Fatalf("前置下单失败: %v", serr)
		}
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(200)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: mux}
	go hs.Serve(ln)
	base := "http://" + ln.Addr().String()

	go func() {
		resp, cerr := http.Get(base + "/slow")
		if cerr == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("前置失败：in-flight 请求未启动")
	}

	done := make(chan struct{})
	go func() {
		a.gracefulShutdown(hs)
		close(done)
	}()
	// in-flight handler 阻塞期间：HTTP Shutdown 在等它 → 撤单尚未开始
	// （若实现是先撤单后关 HTTP，此处 cancelCount 会立刻变成 2）。
	time.Sleep(200 * time.Millisecond)
	if c := m.cancelCount(); c != 0 {
		t.Fatalf("HTTP 未关闭前不得开始撤单（in-flight handler 仍在处理）: cancels=%d", c)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gracefulShutdown 未在预算内完成")
	}
	if c := m.cancelCount(); c != 2 {
		t.Fatalf("HTTP 关闭后应完成撤单 2 张: %d", c)
	}
	// 退出后新请求被拒绝（监听已关）
	if resp, cerr := http.Get(base + "/api/status"); cerr == nil {
		resp.Body.Close()
		t.Fatal("drain 完成后不得再接受新请求")
	}
}
