package exchange

import "testing"

func TestOrderStatusTerminal(t *testing.T) {
	terminal := []OrderStatus{StatusFilled, StatusCancelled, StatusRejected, StatusExpired}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	nonTerminal := []OrderStatus{StatusNew, StatusSubmitted, StatusPartiallyFilled}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("%s 不应为终态", s)
		}
	}
}

func TestCandleKey(t *testing.T) {
	c := Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H", OpenTime: 1700000000000}
	want := "okx|BTC-USDT|1H|1700000000000"
	if c.Key() != want {
		t.Fatalf("唯一键格式错误: %s", c.Key())
	}
	// 唯一键四要素任一不同则不同
	c2 := c
	c2.OpenTime++
	if c.Key() == c2.Key() {
		t.Fatal("不同 OpenTime 的唯一键必须不同")
	}
}

func TestTickerMid(t *testing.T) {
	tk := Ticker{Last: 100, Bid: 98, Ask: 102}
	if m := tk.Mid(); m != 100 {
		t.Fatalf("中间价应 100: %v", m)
	}
	// 盘口缺失时退化用最新价
	tk2 := Ticker{Last: 55, Bid: 0, Ask: 0}
	if m := tk2.Mid(); m != 55 {
		t.Fatalf("盘口缺失应退化为 last: %v", m)
	}
}

func TestTickerSpreadPct(t *testing.T) {
	tk := Ticker{Bid: 99, Ask: 101}
	if sp := tk.SpreadPct(); sp < 1.99 || sp > 2.01 {
		t.Fatalf("价差应约 2%%: %v", sp)
	}
	for _, bad := range []Ticker{{Bid: 0, Ask: 101}, {Bid: 99, Ask: 0}} {
		if sp := bad.SpreadPct(); sp != 0 {
			t.Fatalf("盘口无效时价差应为 0: %v", sp)
		}
	}
}

func TestOrderNotional(t *testing.T) {
	o := Order{Price: 100, Qty: 2}
	if n := o.Notional(); n != 200 {
		t.Fatalf("限价单名义应按委托价: %v", n)
	}
	// 市价单无委托价，按成交均价
	m := Order{Price: 0, AvgPrice: 50, Qty: 3}
	if n := m.Notional(); n != 150 {
		t.Fatalf("市价单名义应按成交均价: %v", n)
	}
}
