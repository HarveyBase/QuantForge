package grid

import (
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

func mkGrid(t *testing.T, lower, upper float64, grids int, spacing string) *Grid {
	t.Helper()
	g, err := New(Params{Lower: lower, Upper: upper, Grids: grids, QtyPerGrid: 0.1, Spacing: spacing, StopOnBreak: true})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func ctxAt(price float64) *strategy.Context {
	c := exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		Open: price, High: price * 1.01, Low: price * 0.99, Close: price, Volume: 1, Confirmed: true}
	return &strategy.Context{Symbol: "BTC-USDT", Candles: []exchange.Candle{c}, Cash: 100000}
}

func TestParamsValidation(t *testing.T) {
	if _, err := New(Params{Lower: 100, Upper: 50, Grids: 10, QtyPerGrid: 1}); err == nil {
		t.Fatal("参数非法必须报错")
	}
}

func TestGeoSpacing(t *testing.T) {
	g := mkGrid(t, 100, 400, 2, "geo")
	// 等比：100, 200, 400
	if g.Levels()[1] != 200 {
		t.Fatalf("等比网格线错误: %v", g.Levels())
	}
}

func TestDownCrossBuysUpCrossSells(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith") // 100,125,150,175,200
	g.OnCandle(ctxAt(160))               // 初始建仓（idx=3）
	gots := g.OnCandle(ctxAt(140))       // 下穿到 idx=1（150→125 之间）
	if len(gots) != 1 || gots[0].Side != exchange.Buy {
		t.Fatalf("下穿应产生买单: %+v", gots)
	}
	if gots[0].Price != 125 { // 在下一档（idx=1 的下一档是 levels[0]? 不，buyIntent(level=2)→levels[1]=125
		t.Fatalf("买单价应为 125: %v", gots[0].Price)
	}
	up := ctxAt(160)
	up.Position = 0.1
	sells := g.OnCandle(up)
	if len(sells) == 0 || sells[0].Side != exchange.Sell {
		t.Fatalf("上穿应产生卖单: %+v", sells)
	}
}

func TestBreakLowerStopsBuying(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	g.OnCandle(ctxAt(150))
	gots := g.OnCandle(ctxAt(90)) // 打穿下界（跨两格：150→125→100）
	if len(gots) != 2 || gots[0].Side != exchange.Buy || gots[1].Side != exchange.Buy {
		t.Fatalf("跨两格应产生两张逐格买单: %+v", gots)
	}
	if gots[1].Price != 100 { // 最后一格在下界 100
		t.Fatalf("最后一档买单价应为下界 100: %v", gots[1].Price)
	}
	if !g.Stats().Broke {
		t.Fatal("应进入打穿状态")
	}
	more := g.OnCandle(ctxAt(85))
	if len(more) != 0 {
		t.Fatalf("打穿后必须停止补格（不满仓接刀）: %+v", more)
	}
	recovered := g.OnCandle(ctxAt(120))
	if g.Stats().Broke || len(recovered) != 0 {
		t.Fatal("收复下界应恢复网格状态（无跨格不产生订单）")
	}
}

func TestApplyFillAccounting(t *testing.T) {
	g := mkGrid(t, 100, 200, 4, "arith")
	g.OnCandle(ctxAt(150))
	g.ApplyFill(exchange.Buy, 0.1, 125)
	g.ApplyFill(exchange.Sell, 0.1, 150)
	s := g.Stats()
	if s.Position != 0 || s.Rounds != 1 {
		t.Fatalf("轮数统计错误: %+v", s)
	}
}
