package risk

import (
	"testing"

	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
)

func testLimits() Limits {
	return Limits{
		MaxOrderNotionalUSD:    1000,
		MaxDailyNotionalUSD:    3000,
		MaxPositionNotionalUSD: 2000,
		MaxOrdersPerMinute:     5,
		MaxDailyLossPct:        5,
	}
}

func buyReq(qty, price float64) exchange.OrderRequest {
	return exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Buy, Type: exchange.OrderLimit, Price: price, Qty: qty}
}

func TestOrderNotionalLimit(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	if err := m.CheckOrder(buyReq(0.03, 50000), 50000); err == nil {
		t.Fatal("单笔名义 1500 超限 1000 必须拒单")
	}
	if rs := m.Rejections(); len(rs) != 1 || rs[0].RuleID != "MAX_ORDER_NOTIONAL" {
		t.Fatalf("拒单台账必须留痕: %+v", rs)
	}
}

func TestDailyNotionalAccumulates(t *testing.T) {
	l := testLimits()
	l.MaxOrdersPerMinute = 100 // 本测试聚焦单日限额，放开频率
	m := NewManager(l, portfolio.New(100000), "")
	for i := 0; i < 6; i++ { // 6 × 500 = 3000 = 单日限额
		if err := m.CheckOrder(buyReq(0.01, 50000), 50000); err != nil {
			t.Fatalf("第 %d 单不应被拒: %v", i+1, err)
		}
	}
	if err := m.CheckOrder(buyReq(0.01, 50000), 50000); err == nil {
		t.Fatal("当日累计 3000 达到上限后必须拒单")
	}
}

func TestOrderRateLimit(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	for i := 0; i < 5; i++ {
		if err := m.CheckOrder(buyReq(0.001, 100), 100); err != nil {
			t.Fatalf("第 %d 单不应被拒: %v", i+1, err)
		}
	}
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err == nil {
		t.Fatal("1 分钟第 6 单必须被频率风控拦截")
	}
}

func TestKillSwitchBlocksAll(t *testing.T) {
	m := NewManager(testLimits(), portfolio.New(100000), "")
	m.Kill.Trip("测试停机")
	if err := m.CheckOrder(buyReq(0.001, 100), 100); err == nil {
		t.Fatal("Kill Switch 触发后必须拦截一切新下单")
	}
}

func TestSellRequiresAvailable(t *testing.T) {
	pf := portfolio.New(100000)
	// 持有 0.1 但可用 0（全部被挂单冻结）
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 0.1, AvgPrice: 50000, Fee: 0})
	pf.Freeze(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Price: 51000, Qty: 0.1})
	m := NewManager(testLimits(), pf, "")
	if err := m.CheckOrder(exchange.OrderRequest{Symbol: "BTC-USDT", Side: exchange.Sell, Price: 51000, Qty: 0.05}, 51000); err == nil {
		t.Fatal("可卖不足必须拒单（卖 available 不卖 position）")
	}
}

func TestPositionNotionalCap(t *testing.T) {
	pf := portfolio.New(1_000_000)
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 0.03, AvgPrice: 50000, Fee: 0})
	pf.UpdateMark("BTC-USDT", 50000) // 已有敞口 1500
	m := NewManager(testLimits(), pf, "")
	if err := m.CheckOrder(buyReq(0.01, 50000), 50000); err != nil {
		t.Fatalf("1500+500=2000 恰好达上限边界应放行: %v", err)
	}
	if err := m.CheckOrder(buyReq(0.011, 50000), 50000); err == nil {
		t.Fatal("加仓后敞口 1500+550=2050 超限 2000 必须拒单")
	}
}

func TestDailyLossTripsKillSwitch(t *testing.T) {
	pf := portfolio.New(1000)
	pf.UpdateMark("BTC-USDT", 100)
	m := NewManager(testLimits(), pf, "")
	// 模拟亏损：100 买入 40 卖出，亏 60（>5%）
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, Fee: 0})
	pf.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Sell, FilledQty: 1, AvgPrice: 40, Fee: 0}) // 亏 60
	if err := m.CheckOrder(buyReq(0.001, 50), 50); err == nil {
		t.Fatal("当日亏损超 5% 应自动触发 Kill Switch 并拒单")
	}
	if !m.Kill.Tripped() {
		t.Fatal("回撤超限必须联动 Kill Switch")
	}
}

func TestLimitsFromConfig(t *testing.T) {
	c := config.Default()
	l := Limits{
		MaxOrderNotionalUSD: c.Risk.MaxOrderNotionalUSD,
		MaxDailyNotionalUSD: c.Risk.MaxDailyNotionalUSD,
	}
	if l.MaxOrderNotionalUSD != 1000 || l.MaxDailyNotionalUSD != 10000 {
		t.Fatalf("config → risk 限额映射错误: %+v", l)
	}
}
