package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsNilCollector(t *testing.T) {
	if _, err := New(t.TempDir(), time.Hour, nil); err == nil {
		t.Fatal("空采集函数必须报错")
	}
}

func TestFirstReviewEstablishesBaseline(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	r, _ := New(dir, time.Hour, func(from time.Time) Input {
		calls++
		return Input{Stage: "paper", Symbol: "BTC-USDT", Interval: "1H",
			Equity: 10000, Cash: 5000, PriceFirst: 100, PriceLast: 110, CandlesSeen: 60}
	})
	rec, err := r.ReviewOnce()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("采集应恰好调用一次")
	}
	// 首次复盘无窗口基准
	if rec.WindowRetPct != 0 || rec.EquityStart != 10000 {
		t.Fatalf("首次复盘只建基准: %+v", rec)
	}
	if rec.PriceChgPct < 9.9 || rec.PriceChgPct > 10.1 {
		t.Fatalf("价格变动计算错误: %v", rec.PriceChgPct)
	}
	if rec.Stage != "paper" {
		t.Fatal("复盘必须标注阶段（表述红线）")
	}
	// 归档 + ledger 双写
	if _, err := os.Stat(filepath.Join(dir, "reviews", rec.Ts.Format("20060102_150405.000000000")+".json")); err != nil {
		t.Fatalf("复盘归档必须落盘: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "reviews", "ledger.jsonl"))
	if err != nil || !strings.Contains(string(b), `"stage":"paper"`) {
		t.Fatalf("ledger 索引必须留痕: %v %s", err, b)
	}
}

func TestSecondReviewComputesWindowReturn(t *testing.T) {
	dir := t.TempDir()
	equities := []float64{10000, 10500}
	i := 0
	r, _ := New(dir, time.Hour, func(from time.Time) Input {
		eq := equities[i]
		i++
		return Input{Stage: "paper", Equity: eq, PriceFirst: 100, PriceLast: 102}
	})
	r.ReviewOnce()
	rec, err := r.ReviewOnce()
	if err != nil {
		t.Fatal(err)
	}
	if rec.WindowRetPct < 4.9 || rec.WindowRetPct > 5.1 {
		t.Fatalf("窗口收益应为 5%%: %v", rec.WindowRetPct)
	}
	if rec.EquityStart != 10000 {
		t.Fatalf("窗口起始权益应为上次复盘值: %v", rec.EquityStart)
	}
}

func TestDiagnoseRules(t *testing.T) {
	// 跑赢买入持有
	rec := &Record{WindowRetPct: 3, PriceChgPct: 1, Stage: "paper", Interval: "1H"}
	notes := diagnose(rec, time.Hour)
	if !anyContains(notes, "跑赢") || !anyContains(notes, "不代表实盘") {
		t.Fatalf("收益对比诊断缺失: %v", notes)
	}
	// 跑输
	rec2 := &Record{WindowRetPct: -2, PriceChgPct: 1, Stage: "paper", Interval: "1H"}
	if !anyContains(diagnose(rec2, time.Hour), "跑输") {
		t.Fatal("跑输诊断缺失")
	}
	// 拒单归因
	rec3 := &Record{Stage: "paper", Interval: "1H",
		Rejections: []RejSummary{{RuleID: "INSUFFICIENT_CASH"}, {RuleID: "INSUFFICIENT_CASH"}}}
	if !anyContains(diagnose(rec3, time.Hour), "INSUFFICIENT_CASH×2") {
		t.Fatalf("拒单应按规则聚合计数: %v", diagnose(rec3, time.Hour))
	}
	// 交易过频警告
	rec4 := &Record{Stage: "paper", Interval: "1H",
		Fills: make([]FillSummary, maxFillsPerHour+1)}
	if !anyContains(diagnose(rec4, time.Hour), "交易过频") {
		t.Fatal("过频警告缺失")
	}
	// 频率护栏内
	rec5 := &Record{Stage: "paper", Interval: "1H", Fills: []FillSummary{{}}}
	if !anyContains(diagnose(rec5, time.Hour), "频率护栏内") {
		t.Fatal("护栏内提示缺失")
	}
	// Kill 状态
	rec6 := &Record{Stage: "paper", Interval: "1H", KillTripped: true, KillReason: "演练"}
	if !anyContains(diagnose(rec6, time.Hour), "Kill Switch") {
		t.Fatal("Kill 状态诊断缺失")
	}
	// 数据连续性异常（24h 窗口应见约 24 根 1H，只见了 10 根）
	rec7 := &Record{Stage: "paper", Interval: "1H", CandlesSeen: 10}
	if !anyContains(diagnose(rec7, 24*time.Hour), "数据连续性异常") {
		t.Fatalf("断流诊断缺失: %v", diagnose(rec7, 24*time.Hour))
	}
	// 静默窗口
	rec8 := &Record{Stage: "paper", Interval: "1H", CandlesSeen: 60}
	if !anyContains(diagnose(rec8, time.Hour), "市况平静") {
		t.Fatalf("无事件应有平静结论: %v", diagnose(rec8, time.Hour))
	}
}

func anyContains(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestPersistRoundtripAndRecent(t *testing.T) {
	dir := t.TempDir()
	eq := 10000.0
	r, _ := New(dir, time.Hour, func(from time.Time) Input {
		eq += 10
		return Input{Stage: "paper", Symbol: "BTC-USDT", Interval: "1H", Equity: eq, CandlesSeen: 60}
	})
	for i := 0; i < 3; i++ {
		if _, err := r.ReviewOnce(); err != nil {
			t.Fatal(err)
		}
	}
	recs := r.Recent(2)
	if len(recs) != 2 {
		t.Fatalf("Recent(2) 应返回 2 份: %d", len(recs))
	}
	// 倒序：最新在前
	if recs[0].Ts.Before(recs[1].Ts) {
		t.Fatal("复盘记录应按时间倒序")
	}
	if recs[0].Version != Version || recs[0].Symbol != "BTC-USDT" {
		t.Fatalf("往返字段丢失: %+v", recs[0])
	}
	if r.Recent(0) == nil || len(r.Recent(0)) == 0 {
		t.Log("Recent(0) 默认返回最近 10 份")
	}
	// 目录不存在时安全返回 nil
	r2, _ := New(t.TempDir(), time.Hour, func(time.Time) Input { return Input{} })
	if got := r2.Recent(5); got != nil {
		t.Fatalf("无归档目录应返回 nil: %v", got)
	}
}

func TestLedgerIsJSONL(t *testing.T) {
	dir := t.TempDir()
	r, _ := New(dir, time.Hour, func(time.Time) Input { return Input{Stage: "research", Equity: 1, CandlesSeen: 60} })
	if _, err := r.ReviewOnce(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "reviews", "ledger.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("应恰好一行: %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("ledger 行必须是 JSON: %v", err)
	}
	if m["stage"] != "research" {
		t.Fatal("ledger 应含阶段字段")
	}
}

func TestLoopStopsOnCancel(t *testing.T) {
	r, _ := New(t.TempDir(), 5*time.Millisecond, func(time.Time) Input {
		return Input{Stage: "paper", Equity: 1, CandlesSeen: 60}
	})
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.Loop(ctx, nil); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消必须终止复盘循环")
	}
}
