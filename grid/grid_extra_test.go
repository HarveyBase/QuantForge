package grid

import (
	"sync"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

func TestArithSpacing(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith") // 100,125,150,175,200
	levels := g.Levels()
	want := []float64{100, 125, 150, 175, 200}
	for i, w := range want {
		if levels[i] != w {
			t.Fatalf("等差网格线 %d 应为 %v: %v", i, w, levels)
		}
	}
}

func TestInvalidSpacingFallsBackGeo(t *testing.T) {
	g := mkGrid(t, 100, 400, 2, "whatever")
	if g.Levels()[1] != 200 {
		t.Fatalf("非法 spacing 应回退等比: %v", g.Levels())
	}
}

func TestParamsValidationBranches(t *testing.T) {
	bad := []Params{
		{Lower: 0, Upper: 100, Grids: 4, QtyPerGrid: 1},
		{Lower: -1, Upper: 100, Grids: 4, QtyPerGrid: 1},
		{Lower: 100, Upper: 100, Grids: 4, QtyPerGrid: 1},
		{Lower: 100, Upper: 200, Grids: 1, QtyPerGrid: 1},
		{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0},
	}
	for i, p := range bad {
		if _, err := New(p); err == nil {
			t.Errorf("case %d 参数非法必须报错", i)
		}
	}
}

func TestEmptyCandlesNoOp(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	if out := g.OnCandle(&strategy.Context{Symbol: "BTC-USDT"}); out != nil {
		t.Fatalf("空 K 线不应产生订单: %+v", out)
	}
}

func TestFirstCandleBelowLowerNoBuy(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	out := g.OnCandle(ctxAt(50)) // idx=0，下界之下
	if len(out) != 0 {
		t.Fatalf("下界之下初始不应建仓: %+v", out)
	}
}

func TestFirstCandleAtUpperBuysOneBelow(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	// idx=4（顶格）：初始建仓在下一档 175 挂买单
	out := g.OnCandle(ctxAt(200))
	if len(out) != 1 || out[0].Side != exchange.Buy || out[0].Price != 175 {
		t.Fatalf("顶格初始应在下一档挂买单: %+v", out)
	}
}

func TestUpCrossLimitedByPosition(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith") // 100,125,150,175,200
	g.OnCandle(ctxAt(125))               // idx=1
	// 从 idx1 跳到 idx4（跨 3 格），但持仓只够卖 1 格 → 只 1 张卖单（幽灵卖单防护）
	up := ctxAt(200)
	up.Position = 0.1
	sells := g.OnCandle(up)
	if len(sells) != 1 || sells[0].Side != exchange.Sell {
		t.Fatalf("持仓不足时应截断卖单数: %+v", sells)
	}
	// 持仓充足时逐格全卖
	g2 := mkGrid(t, 100, 200, 4, "arith")
	g2.OnCandle(ctxAt(125))
	up2 := ctxAt(200)
	up2.Position = 1.0
	sells2 := g2.OnCandle(up2)
	if len(sells2) != 3 {
		t.Fatalf("跨 3 格应逐格卖出 3 张: %+v", sells2)
	}
	for i, s := range sells2 {
		if s.Side != exchange.Sell {
			t.Fatalf("全部应为卖单: %+v", s)
		}
		_ = i
	}
	// 无持仓时不产生卖单
	g3 := mkGrid(t, 100, 200, 4, "arith")
	g3.OnCandle(ctxAt(125))
	sells3 := g3.OnCandle(ctxAt(200))
	if len(sells3) != 0 {
		t.Fatalf("无持仓上穿不应产生幽灵卖单: %+v", sells3)
	}
}

func TestBuyRefEdge(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	g.OnCandle(ctxAt(150))
	g.ApplyFill(exchange.Sell, 0.1, 150)
	s := g.Stats()
	// lastIdx=2 → buyRef = levels[1] = 125；卖出 150 成本 125 → 利润 2.5
	if s.Realized != 0.1*(150-125) {
		t.Fatalf("已实现利润按下一档成本核算: %+v", s)
	}
	// lastIdx=0 时 buyRef 退化为下界
	g2 := mkGrid(t, 100, 200, 4, "arith")
	g2.OnCandle(ctxAt(90)) // idx=0（低于下界）
	g2.ApplyFill(exchange.Sell, 0.1, 150)
	if g2.Stats().Realized != 0.1*(150-100) {
		t.Fatalf("lastIdx=0 时成本应退化为下界: %+v", g2.Stats())
	}
}

func TestStatsInitial(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "geo")
	s := g.Stats()
	if s.Rounds != 0 || s.Realized != 0 || s.Broke || s.Position != 0 {
		t.Fatalf("初始统计应为零值: %+v", s)
	}
	if g.Name() != "grid" || g.Warmup() != 1 {
		t.Fatal("策略元数据错误")
	}
}

func TestApplyFillBuy(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	g.OnCandle(ctxAt(150))
	g.ApplyFill(exchange.Buy, 0.5, 125)
	s := g.Stats()
	if s.Position != 0.5 {
		t.Fatalf("买入应累加持仓: %+v", s)
	}
}

func TestLevelsDefensiveCopy(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	l1 := g.Levels()
	l1[0] = 999
	if g.Levels()[0] != 100 {
		t.Fatal("Levels 必须返回防御性拷贝")
	}
}

func TestConcurrentOnCandle(t *testing.T) {
	g := mkGrid(t, 100, 200, 20, "geo")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			px := 120 + float64(i%60)
			c := exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
				Open: px, High: px * 1.01, Low: px * 0.99, Close: px, Confirmed: true}
			g.OnCandle(&strategy.Context{Symbol: "BTC-USDT", Candles: []exchange.Candle{c}, Position: 1, Cash: 1e6})
			g.ApplyFill(exchange.Buy, 0.01, px)
			_ = g.Stats()
			_ = g.Levels()
		}(i)
	}
	wg.Wait()
}
