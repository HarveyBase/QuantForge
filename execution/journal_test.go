package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
	paperex "github.com/HarveyBase/QuantForge/exchange/paper"
)

// newJournaledExecutor 带事件日志的执行器（限流退避缩短以保测试速度）。
func newJournaledExecutor(t *testing.T, seedCash float64) (*Executor, *paperex.Exchange, string) {
	t.Helper()
	pex := paperex.New(100, seedCash, paperex.FillModel{FeeBps: 0})
	pf := portfolio.New(seedCash)
	rk := risk.NewManager(risk.Limits{
		MaxOrderNotionalUSD: 100000, MaxDailyNotionalUSD: 1000000,
		MaxPositionNotionalUSD: 1000000, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50,
	}, pf, "")
	e := New(pex, rk, pf, nil)
	j, err := NewJournal(filepath.Join(t.TempDir(), "orders.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e.AttachJournal(j)
	return e, pex, j.path
}

// TestJournalWriteReplayConsistency 写→重放→三张表一致。
func TestJournalWriteReplayConsistency(t *testing.T) {
	e, pex, path := newJournaledExecutor(t, 10000)
	// 挂单一张（submitted），成交一张（终态离场但 claimed 保留）
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "j-open",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 105, Qty: 1, ClientOrderID: "j-fill",
	}); err != nil {
		t.Fatal(err)
	}
	pex.UpdatePrice(106)
	e.ReconcileOnce(context.Background())

	// 新执行器重放同一 journal
	pf2 := portfolio.New(10000)
	rk2 := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf2, "")
	e2 := New(pex, rk2, pf2, nil)
	events, skipped, err := LoadJournal(path)
	if err != nil || skipped != 0 {
		t.Fatalf("读取 journal 失败: %v skipped=%d", err, skipped)
	}
	e2.RestoreFromEvents(events)

	open1, open2 := e.OpenOrders(), e2.OpenOrders()
	if len(open1) != len(open2) {
		t.Fatalf("重放后挂单数不一致: %d vs %d", len(open1), len(open2))
	}
	for i := range open1 {
		if open1[i].OrderID != open2[i].OrderID || open1[i].Status != open2[i].Status {
			t.Fatalf("挂单状态不一致: %+v vs %+v", open1[i], open2[i])
		}
	}
	e.mu.Lock()
	c1 := len(e.claimed)
	e.mu.Unlock()
	e2.mu.Lock()
	c2 := len(e2.claimed)
	e2.mu.Unlock()
	if c1 != c2 || c2 != 2 {
		t.Fatalf("claimed 数不一致: %d vs %d（期望 2）", c1, c2)
	}
}

// TestClaimedSurvivesRestart 重启后（重放）相同 clientOrderID 仍被幂等拒绝。
func TestClaimedSurvivesRestart(t *testing.T) {
	e, _, path := newJournaledExecutor(t, 10000)
	if _, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "restart-dup",
	}); err != nil {
		t.Fatal(err)
	}
	pf2 := portfolio.New(10000)
	rk2 := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf2, "")
	pex2 := paperex.New(100, 10000, paperex.FillModel{FeeBps: 0})
	e2 := New(pex2, rk2, pf2, nil)
	events, _, _ := LoadJournal(path)
	e2.RestoreFromEvents(events)
	if _, err := e2.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "restart-dup",
	}); err == nil {
		t.Fatal("重放后相同 clientOrderID 必须仍被幂等拒绝（防重启后重复下单）")
	}
}

// TestJournalToleratesCorruptLines 损坏行/空行跳过，不阻断重放。
func TestJournalToleratesCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.jsonl")
	good := `{"ts":"2026-01-01T00:00:00Z","ev":"register","order":{"order_id":"o1","client_order_id":"c1","symbol":"BTC-USDT","status":"submitted"}}`
	if err := os.WriteFile(path, []byte("not-json\n\n"+good+"\n{\"broken\":[\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || skipped != 3 {
		t.Fatalf("期望 1 条有效事件 + 3 条跳过, got events=%d skipped=%d", len(events), skipped)
	}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(paperex.New(100, 10000, paperex.FillModel{FeeBps: 0}), rk, pf, nil)
	e.RestoreFromEvents(events)
	if len(e.OpenOrders()) != 1 {
		t.Fatalf("坏行不阻断重放：应恢复 1 张挂单, got %d", len(e.OpenOrders()))
	}
}

// TestJournalCompactKeepsClaimedAndOpen 压缩后：非终态挂单与 claimed 幂等键不丢。
func TestJournalCompactKeepsClaimedAndOpen(t *testing.T) {
	e, _, path := newJournaledExecutor(t, 10000)
	for i := 0; i < 5; i++ {
		if _, err := e.Submit(context.Background(), exchange.OrderRequest{
			Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
			Price: 95, Qty: 0.1, ClientOrderID: fmt.Sprintf("cmp-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.CompactJournal(); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := LoadJournal(path)
	if err != nil || skipped != 0 {
		t.Fatalf("压缩后 journal 必须干净: %v skipped=%d", err, skipped)
	}
	pf2 := portfolio.New(10000)
	rk2 := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf2, "")
	e2 := New(paperex.New(100, 10000, paperex.FillModel{FeeBps: 0}), rk2, pf2, nil)
	e2.RestoreFromEvents(events)
	if len(e2.OpenOrders()) != 5 {
		t.Fatalf("压缩后重放应保留 5 张挂单, got %d", len(e2.OpenOrders()))
	}
	for _, coid := range []string{"cmp-0", "cmp-4"} {
		if _, err := e2.Submit(context.Background(), exchange.OrderRequest{
			Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
			Price: 95, Qty: 0.1, ClientOrderID: coid,
		}); err == nil {
			t.Fatalf("压缩后 %s 的 claimed 防重发必须保留", coid)
		}
	}
}

// TestSubmitRateLimitBackoffRetries 限流错误必须退避重试而非立即失败。
func TestSubmitRateLimitBackoffRetries(t *testing.T) {
	old := rateLimitBackoff
	rateLimitBackoff = 5 * time.Millisecond
	defer func() { rateLimitBackoff = old }()

	f := &fakeEx{placeErrs: []error{fmt.Errorf("okx 50011 限流: %w", exchange.ErrRateLimited)}, byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{}}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(f, rk, pf, nil)
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "rl-1",
	})
	if err != nil {
		t.Fatalf("限流错误应退避后重试成功: %v", err)
	}
	if o.Status != exchange.StatusSubmitted || len(f.placed) != 2 {
		t.Fatalf("应重试第二次成功: placed=%d o=%+v", len(f.placed), o)
	}
}

// TestSubmitDuplicateClOrdIDQueryBackfill 不可重试的"重复 clientOrderID"错误也查后补。
func TestSubmitDuplicateClOrdIDQueryBackfill(t *testing.T) {
	existing := exchange.Order{
		Exchange: "fake", Symbol: "BTC-USDT", OrderID: "live-1", ClientOrderID: "dup-1",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 95, Qty: 1, Status: exchange.StatusSubmitted,
	}
	f := &fakeEx{
		placeErrs: []error{errors.New("okx code 51000: ClientOrderId already exists")},
		byClient:  map[string]exchange.Order{"dup-1": existing},
		orders:    map[string]exchange.Order{"live-1": existing},
	}
	pf := portfolio.New(10000)
	pf.Seed(nil, "BTC-USDT", "BTC", "USDT", 100) // 现金校验可用
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(f, rk, pf, nil)
	o, err := e.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "dup-1",
	})
	if err != nil {
		t.Fatalf("重复单错误应查后补成功（防双发）: %v", err)
	}
	if o.OrderID != "live-1" {
		t.Fatalf("应返回交易所已存在订单, got %+v", o)
	}
	if len(e.OpenOrders()) != 1 {
		t.Fatal("查后补订单应进入订单簿")
	}
}

// TestRestoreAdoptedTerminalNotInOrders BUG-2：adopted 事件重放语义与 recover.go 内存路径
// 一致——终态单一律不进挂单表（无论成交多少），只认领幂等键；重放后 Submit 同 coid 被拒。
func TestRestoreAdoptedTerminalNotInOrders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.jsonl")
	j, err := NewJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	// 在途列表含终态 0 成交单（交易所异常场景）
	term := exchange.Order{
		Exchange: "fake", Symbol: "BTC-USDT", OrderID: "ex-term", ClientOrderID: "adopt-term-1",
		Side: exchange.Buy, Type: exchange.OrderLimit, Price: 95, Qty: 1, Status: exchange.StatusCancelled,
	}
	f := &fakeEx{open: []exchange.Order{term}, byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{}}
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrderNotionalUSD: 1e6, MaxDailyNotionalUSD: 1e7, MaxPositionNotionalUSD: 1e7, MaxOrdersPerMinute: 100, MaxDailyLossPct: 50}, pf, "")
	e := New(f, rk, pf, nil)
	e.AttachJournal(j)
	if _, err := e.AdoptOpenOrders(context.Background(), "BTC-USDT"); err != nil {
		t.Fatal(err)
	}
	if len(e.OpenOrders()) != 0 {
		t.Fatalf("内存路径终态单不得进挂单表: %+v", e.OpenOrders())
	}

	// 重放侧：新执行器读同一 journal，语义必须一致
	events, skipped, err := LoadJournal(path)
	if err != nil || skipped != 0 {
		t.Fatalf("读取 journal 失败: %v skipped=%d", err, skipped)
	}
	if len(events) == 0 || events[0].Ev != "adopted" {
		t.Fatalf("journal 应有 adopted 事件: %+v", events)
	}
	e2 := newFakeExecutor(t, &fakeEx{byClient: map[string]exchange.Order{}, orders: map[string]exchange.Order{}}, 10000)
	e2.RestoreFromEvents(events)
	if opens := e2.OpenOrders(); len(opens) != 0 {
		t.Fatalf("重放后终态 0 成交 adopted 单不得进入挂单表: %+v", opens)
	}
	if _, err := e2.Submit(context.Background(), exchange.OrderRequest{
		Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: 95, Qty: 1, ClientOrderID: "adopt-term-1",
	}); err == nil {
		t.Fatal("重放后 adopted 幂等键必须仍拒绝同 coid 提交（防重发）")
	}
}

// TestJournalToleratesOverlongLine RISK-5：损坏超长行（>1MB）不得阻断恢复——
// 截断跳过计入 skipped，前后有效行照常读取。
func TestJournalToleratesOverlongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.jsonl")
	good1 := `{"ts":"2026-01-01T00:00:00Z","ev":"register","order":{"order_id":"o1","client_order_id":"c1","symbol":"BTC-USDT","status":"submitted"}}`
	good2 := `{"ts":"2026-01-01T00:00:01Z","ev":"register","order":{"order_id":"o2","client_order_id":"c2","symbol":"BTC-USDT","status":"submitted"}}`
	blob := strings.Repeat("x", 3*journalMaxLine) // 3MB 无换行损坏行（多轮缓冲满）
	if err := os.WriteFile(path, []byte(good1+"\n"+blob+"\n"+good2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("超长行不得阻断恢复: %v", err)
	}
	if len(events) != 2 || events[0].Order.OrderID != "o1" || events[1].Order.OrderID != "o2" {
		t.Fatalf("超长行前后的有效事件必须保留: %+v", events)
	}
	if skipped != 1 {
		t.Fatalf("超长行应计 1 条 skipped, got %d", skipped)
	}
}

// TestJournalWriteFailureCounting RISK-1：journal 连续写失败累加计数、成功清零；
// WriteFailures 为 atomic 只读快照，供上层监控接线。
func TestJournalWriteFailureCounting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.jsonl")
	j, err := NewJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newFakeExecutor(t, &fakeEx{}, 10000)
	e.AttachJournal(j)
	o := exchange.Order{OrderID: "wf-1", ClientOrderID: "wf", Symbol: "BTC-USDT", Status: exchange.StatusSubmitted}

	e.journalEvent("register", o) // 正常写入
	if n := j.WriteFailures(); n != 0 {
		t.Fatalf("正常写入失败计数应为 0, got %d", n)
	}
	// 破坏写入：把日志路径变成目录 → Append 打开失败
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	e.journalEvent("update", o)
	e.journalEvent("cancel", o)
	if n := j.WriteFailures(); n != 2 {
		t.Fatalf("连续写失败应累加计数: got %d", n)
	}
	// 恢复文件 → 写成功清零
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.journalEvent("update", o)
	if n := j.WriteFailures(); n != 0 {
		t.Fatalf("写成功后连续失败计数应清零, got %d", n)
	}
}

// TestFailLogMilestone RISK-1：失败日志限流——只在第 1、10、100…次打印。
func TestFailLogMilestone(t *testing.T) {
	yes := []int64{1, 10, 100, 1000, 1000000}
	no := []int64{0, 2, 5, 9, 11, 20, 55, 101, 250, -3}
	for _, n := range yes {
		if !failLogMilestone(n) {
			t.Errorf("第 %d 次应打日志", n)
		}
	}
	for _, n := range no {
		if failLogMilestone(n) {
			t.Errorf("第 %d 次不应打日志", n)
		}
	}
}

// TestCancelUnknownOrderSkipsJournal NIT：撤本地未知订单（交易所撤单成功）时
// 跳过 journal 落盘，不写零值 cancel 事件。
func TestCancelUnknownOrderSkipsJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.jsonl")
	j, err := NewJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newFakeExecutor(t, &fakeEx{}, 10000) // fakeEx.CancelOrder 对任意 id 都成功
	e.AttachJournal(j)
	if err := e.Cancel(context.Background(), "BTC-USDT", "not-local"); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := LoadJournal(path)
	if err != nil || skipped != 0 {
		t.Fatalf("读取 journal 失败: %v skipped=%d", err, skipped)
	}
	if len(events) != 0 {
		t.Fatalf("未知订单撤单不得写零值 cancel 事件: %+v", events)
	}
}
