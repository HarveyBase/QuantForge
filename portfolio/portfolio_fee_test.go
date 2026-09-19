package portfolio

import (
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func approx(a, b float64) bool { return abs(a-b) < 1e-9 }

// FeeCcy=Base 买入：手续费从持仓数量扣（实收 = Qty - Fee），现金只扣成交名义。
func TestApplyFillFeeInBaseBuyDeductsPosition(t *testing.T) {
	p := New(1000)
	res := p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100, Fee: -0.001, FeeCcy: "BTC"})
	if !res.Applied || !res.FeeInBase || res.Clamped {
		t.Fatalf("分账口径错误: %+v", res)
	}
	pos := p.Positions["BTC-USDT"]
	if !approx(pos.Qty, 0.999) || !approx(pos.Available, 0.999) {
		t.Fatalf("买入实收应为 Qty-Fee: %+v", pos)
	}
	// 现金不得重复扣手续费：1000 - 100 = 900
	if !approx(p.Cash, 900) {
		t.Fatalf("现金应只扣成交名义: %v", p.Cash)
	}
}

// FeeCcy=Base 卖出：卖出数量与手续费都从 Base 持仓扣，现金按全额名义入账。
func TestApplyFillFeeInBaseSellDeductsPosition(t *testing.T) {
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100})
	res := p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Sell, Qty: 0.5, Price: 150, Fee: -0.0005, FeeCcy: "BTC"})
	if !res.FeeInBase || res.Clamped {
		t.Fatalf("分账口径错误: %+v", res)
	}
	pos := p.Positions["BTC-USDT"]
	// 1 - 0.5 - 0.0005 = 0.4995
	if !approx(pos.Qty, 0.4995) || !approx(pos.Available, 0.4995) {
		t.Fatalf("卖出后持仓/可用应同步扣手续费: %+v", pos)
	}
	// 现金：900 + 0.5*150 = 975（不扣现金手续费）
	if !approx(p.Cash, 975) {
		t.Fatalf("现金应按全额名义入账: %v", p.Cash)
	}
}

// FeeCcy=Quote 或为空：维持旧口径从现金扣（空值按 quote 兼容）。
func TestApplyFillFeeQuoteOrEmptyDeductsCash(t *testing.T) {
	p := New(1000)
	res := p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100, Fee: -1, FeeCcy: "USDT"})
	if res.FeeInBase || res.Clamped {
		t.Fatalf("Quote 币手续费不得走 Base 分账: %+v", res)
	}
	if !approx(p.Cash, 899) {
		t.Fatalf("Quote 币手续费应扣现金: %v", p.Cash)
	}
	// FeeCcy 为空：按 quote 兼容处理
	p2 := New(1000)
	res2 := p2.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100, Fee: 1})
	if res2.FeeInBase || res2.Clamped {
		t.Fatalf("空 FeeCcy 不得走 Base 分账: %+v", res2)
	}
	if !approx(p2.Cash, 899) {
		t.Fatalf("空 FeeCcy 应按 quote 扣现金: %v", p2.Cash)
	}
}

// 异常数据：Base 手续费扣穿持仓/可用时 clamp 到 0 并在返回值留痕。
func TestApplyFillFeeInBaseClampNegative(t *testing.T) {
	// 清仓卖出 + Base 手续费：持仓与可用都扣穿
	p := New(1000)
	p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 1, Price: 100})
	res := p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Sell, Qty: 1, Price: 100, Fee: -0.001, FeeCcy: "BTC"})
	if !res.Clamped || res.Note == "" {
		t.Fatalf("扣穿必须留痕: %+v", res)
	}
	if !strings.Contains(res.Note, "持仓") || !strings.Contains(res.Note, "可用") {
		t.Fatalf("留痕应包含持仓与可用两处 clamp: %q", res.Note)
	}
	pos := p.Positions["BTC-USDT"]
	if pos.Qty != 0 || pos.Available != 0 || pos.AvgPrice != 0 {
		t.Fatalf("扣穿后应 clamp 为 0: %+v", pos)
	}
	if !approx(p.Cash, 1000) {
		t.Fatalf("现金按全额名义结算: %v", p.Cash)
	}
	// 买入侧扣穿：手续费超过成交数量
	p2 := New(1000)
	res2 := p2.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 0.001, Price: 100, Fee: -0.002, FeeCcy: "BTC"})
	if !res2.Clamped || res2.Note == "" {
		t.Fatalf("买入扣穿必须留痕: %+v", res2)
	}
	pos2 := p2.Positions["BTC-USDT"]
	if pos2.Qty != 0 || pos2.Available != 0 {
		t.Fatalf("买入扣穿后持仓应为 0: %+v", pos2)
	}
	if !approx(p2.Cash, 999.9) {
		t.Fatalf("现金仍按成交名义扣减: %v", p2.Cash)
	}
}

// Seed 登记的 base/quote 参与分账判断；ApplyTrade 的 FeeCcy 传递链保持有效。
func TestApplyFillSeededBaseAndApplyTradeChain(t *testing.T) {
	p := New(0)
	p.Seed([]exchange.Balance{
		{Asset: "USDT", Total: 1000, Available: 1000},
		{Asset: "BTC", Total: 0.5, Available: 0.5},
	}, "BTC-USDT", "BTC", "USDT", 0)
	// 买 0.1 BTC@100，手续费 0.01 BTC：实收 0.09
	res := p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 0.1, Price: 100, Fee: -0.01, FeeCcy: "BTC"})
	if !res.FeeInBase {
		t.Fatalf("Seed 登记的 Base 币手续费必须走持仓分账: %+v", res)
	}
	pos := p.Positions["BTC-USDT"]
	if !approx(pos.Qty, 0.59) || !approx(pos.Available, 0.59) {
		t.Fatalf("Seed 后买入分账错误: %+v", pos)
	}
	if !approx(p.Cash, 990) {
		t.Fatalf("现金只扣名义: %v", p.Cash)
	}
	// 卖出 0.1@120，手续费 0.5 USDT（Quote）：扣现金
	res2 := p.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Sell, Qty: 0.1, Price: 120, Fee: -0.5, FeeCcy: "USDT"})
	if res2.FeeInBase {
		t.Fatalf("Quote 币手续费不得走 Base 分账: %+v", res2)
	}
	if !approx(p.Cash, 1001.5) || !approx(pos.Qty, 0.49) || !approx(pos.Available, 0.49) {
		t.Fatalf("Quote 手续费卖出账目错误: cash=%v pos=%+v", p.Cash, pos)
	}
	// ApplyTrade 的 Order.FeeCcy 传递链：Base 币手续费同样分账
	p3 := New(1000)
	p3.ApplyTrade(exchange.Order{Symbol: "BTC-USDT", Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, Fee: -0.01, FeeCcy: "BTC"})
	if pos3 := p3.Positions["BTC-USDT"]; !approx(pos3.Qty, 0.99) {
		t.Fatalf("ApplyTrade 须透传 FeeCcy: %+v", pos3)
	}
	if !approx(p3.Cash, 900) {
		t.Fatalf("ApplyTrade Base 手续费不得扣现金: %v", p3.Cash)
	}
	// 非法成交返回 Applied=false，不改动账本
	if r := p3.ApplyFill(exchange.Fill{Symbol: "BTC-USDT", Side: exchange.Buy, Qty: 0, Price: 100}); r.Applied {
		t.Fatalf("非法成交不得入账: %+v", r)
	}
}
