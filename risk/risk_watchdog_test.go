package risk

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
)

// 看门狗核心场景：策略完全不出单，权益暴跌也必须触发 Kill Switch。
func TestEvaluateDailyLossTripsKillWithoutOrder(t *testing.T) {
	pf := portfolio.New(1000)
	pf.UpdateMark("BTC-USDT", 100)
	m := NewManager(testLimits(), pf, "")
	// 模拟亏损（不经 CheckOrder）：100 买入 40 卖出，亏 60（>5%）
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, Fee: 0})
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Sell, FilledQty: 1, AvgPrice: 40, Fee: 0})
	tripped, reason := m.EvaluateDailyLoss()
	if !tripped || reason == "" {
		t.Fatalf("权益回撤超限必须触发看门狗: tripped=%v reason=%q", tripped, reason)
	}
	if !m.Kill.Tripped() || m.Kill.Reason() != reason {
		t.Fatalf("看门狗必须联动 Kill Switch 并留痕原因: %q", m.Kill.Reason())
	}
	// 看门狗只触发 Kill，不产生拒单台账（拒单留痕是 CheckOrder 的职责）
	if rs := m.Rejections(); len(rs) != 0 {
		t.Fatalf("看门狗不得写拒单台账: %+v", rs)
	}
	// Kill 触发后，后续任何下单被 KILL_SWITCH 拦截
	if err := m.CheckOrder(buyReq(0.001, 50), 50); err == nil || !strings.Contains(err.Error(), "KILL_SWITCH") {
		t.Fatalf("Kill 触发后必须拒单: %v", err)
	}
}

// 回撤在限额内不触发；重复评估幂等。
func TestEvaluateDailyLossWithinLimitNoTrip(t *testing.T) {
	pf := portfolio.New(1000)
	pf.UpdateMark("BTC-USDT", 100)
	m := NewManager(testLimits(), pf, "")
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, Fee: 0})
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Sell, FilledQty: 1, AvgPrice: 98, Fee: 0}) // 亏 2 < 5%
	for i := 0; i < 3; i++ {
		if tripped, reason := m.EvaluateDailyLoss(); tripped || reason != "" {
			t.Fatalf("回撤 0.2%% 不得触发看门狗: %v %q", tripped, reason)
		}
	}
	if m.Kill.Tripped() {
		t.Fatal("限额内不得触发 Kill Switch")
	}
}

// 基线为 0 时跳过回撤检查（除零保护，与 CheckOrder 口径一致）。
func TestEvaluateDailyLossZeroBaselineSkips(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100), "")
	m.SetDayStartEquity(0)
	if tripped, reason := m.EvaluateDailyLoss(); tripped || reason != "" {
		t.Fatalf("基线为 0 应跳过检查: %v %q", tripped, reason)
	}
}

// EquityBaseline 只读查询：返回日标签与日起始权益，不改动状态。
func TestEquityBaselineReadOnly(t *testing.T) {
	pf := portfolio.New(1000)
	m := NewManager(testLimits(), pf, "")
	day, eq := m.EquityBaseline()
	if day != time.Now().UTC().Format("2006-01-02") || eq != 1000 {
		t.Fatalf("基线应为当日与初始权益: %s %v", day, eq)
	}
	// 只读：重复查询结果一致
	if day2, eq2 := m.EquityBaseline(); day2 != day || eq2 != eq {
		t.Fatalf("只读查询不得漂移: %s %v", day2, eq2)
	}
	// 反映 SetDayStartEquity 的修正
	m.SetDayStartEquity(990)
	if _, eq3 := m.EquityBaseline(); eq3 != 990 {
		t.Fatalf("基线应跟随 SetDayStartEquity: %v", eq3)
	}
}

// 回归：抽出共享逻辑后，CheckOrder 的 MAX_DAILY_LOSS 拒单规则与留痕行为不变。
func TestCheckOrderDailyLossRuleUnchanged(t *testing.T) {
	pf := portfolio.New(1000)
	pf.UpdateMark("BTC-USDT", 100)
	m := NewManager(testLimits(), pf, "")
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, Fee: 0})
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Sell, FilledQty: 1, AvgPrice: 40, Fee: 0}) // 亏 60
	err := m.CheckOrder(buyReq(0.001, 50), 50)
	if err == nil || !strings.Contains(err.Error(), "MAX_DAILY_LOSS") {
		t.Fatalf("回撤超限必须由 MAX_DAILY_LOSS 拒单: %v", err)
	}
	rs := m.Rejections()
	if len(rs) != 1 || rs[0].RuleID != "MAX_DAILY_LOSS" {
		t.Fatalf("拒单台账规则 ID 必须留痕: %+v", rs)
	}
	if !m.Kill.Tripped() {
		t.Fatal("CheckOrder 回撤分支仍须联动 Kill Switch")
	}
}

// 并发压测：看门狗与 CheckOrder 并发调用（-race 下无数据竞争、无死锁）。
func TestWatchdogConcurrentWithCheckOrder(t *testing.T) {
	pf := portfolio.New(100000)
	pf.UpdateMark("BTC-USDT", 100)
	m := NewManager(testLimits(), pf, "")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = m.CheckOrder(buyReq(0.001, 100), 100)
				_, _ = m.EvaluateDailyLoss()
				_, _ = m.EquityBaseline()
			}
		}()
	}
	// 并发中制造权益暴跌：买 100 BTC@100 后标记价跌至 40，权益 94000 < 95000（-5%）
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 100, AvgPrice: 100, Fee: 0})
	pf.UpdateMark("BTC-USDT", 40)
	wg.Wait()
	if tripped, _ := m.EvaluateDailyLoss(); !tripped || !m.Kill.Tripped() {
		t.Fatal("权益暴跌后看门狗最终必须处于触发态")
	}
}
