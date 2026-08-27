// Package grid 网格策略（docs/06）：震荡市吃波动，成交即在对面一格挂反向单。
// 两种死法都有对策：下界打穿停止补格告警（不满仓接刀）、上界卖飞通知（可选止盈离场）。
package grid

import (
	"fmt"
	"math"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

var _ strategy.Strategy = (*Grid)(nil)

// Params 网格参数（从 config.GridConfig 映射）。
type Params struct {
	Lower       float64
	Upper       float64
	Grids       int
	QtyPerGrid  float64
	Spacing     string // arith | geo
	StopOnBreak bool   // 价格跌破下界后停止补买
}

// Grid 现货网格（Base-Quote 计价，首期以 BTC-USDT 口径实现）。
type Grid struct {
	params    Params
	levels    []float64 // 网格价格线
	baseQty   float64   // 当前持仓（Base）
	quoteCash float64   // 当前现金（Quote）
	lastIdx   int       // 上次所处的格序号
	broke     bool      // 已跌破下界（打穿）
	started   bool
	rounds    int       // 完成的网格轮数（低买高卖一对算一轮）
	realized  float64   // 已实现利润
}

func New(p Params) (*Grid, error) {
	if p.Lower <= 0 || p.Upper <= p.Lower || p.Grids < 2 || p.QtyPerGrid <= 0 {
		return nil, fmt.Errorf("grid: 参数非法 lower=%v upper=%v grids=%d qty=%v", p.Lower, p.Upper, p.Grids, p.QtyPerGrid)
	}
	if p.Spacing != "arith" && p.Spacing != "geo" {
		p.Spacing = "geo"
	}
	g := &Grid{params: p}
	g.levels = make([]float64, p.Grids+1)
	for i := 0; i <= p.Grids; i++ {
		switch p.Spacing {
		case "arith":
			g.levels[i] = p.Lower + (p.Upper-p.Lower)*float64(i)/float64(p.Grids)
		case "geo":
			ratio := math.Pow(p.Upper/p.Lower, 1/float64(p.Grids))
			g.levels[i] = p.Lower * math.Pow(ratio, float64(i))
		}
	}
	return g, nil
}

func (g *Grid) Name() string  { return "grid" }
func (g *Grid) Warmup() int   { return 1 }

// Levels 网格线（展示用）。
func (g *Grid) Levels() []float64 { return g.levels }

// index 价格所处的格序号（0 = 下界之下）。
func (g *Grid) index(price float64) int {
	for i := len(g.levels) - 1; i >= 0; i-- {
		if price >= g.levels[i] {
			return i
		}
	}
	return 0
}

// OnCandle 每根收盘 K 线：跨格移动时产出逐格成交意图（经风控与执行器落地）。
// 向下穿格 → 在下一档挂买单；向上穿格 → 在上一档挂卖单；打穿下界后停止补格。
func (g *Grid) OnCandle(ctx *strategy.Context) []strategy.OrderIntent {
	if len(ctx.Candles) == 0 {
		return nil
	}
	price := ctx.Last().Close
	idx := g.index(price)

	if !g.started {
		g.started = true
		g.lastIdx = idx
		g.baseQty, g.quoteCash = ctx.Position, ctx.Cash
		// 首次进入：在当前格下方挂一张买单建仓（若不在下界）
		if idx > 0 && price >= g.levels[1] {
			return []strategy.OrderIntent{g.buyIntent(idx, "初始建仓")}
		}
		return nil
	}

	var out []strategy.OrderIntent
	// 向下跨格：每次跨一格买一格（跨多格逐格补，但打穿下界后停止）
	for i := g.lastIdx; i > idx && i >= 1; i-- {
		if g.broke && g.params.StopOnBreak {
			break
		}
		out = append(out, g.buyIntent(i, fmt.Sprintf("下穿第 %d 格", i)))
	}
	// 向上跨格：逐格卖出
	for i := g.lastIdx + 1; i <= idx && i <= len(g.levels)-1; i++ {
		out = append(out, g.sellIntent(i, fmt.Sprintf("上穿第 %d 格", i)))
	}
	g.lastIdx = idx

	// 打穿下界：停止补格并告警（不满仓接刀）
	if price < g.params.Lower {
		if !g.broke && g.params.StopOnBreak {
			g.broke = true
		}
	}
	// 收复下界则恢复网格
	if g.broke && price >= g.params.Lower {
		g.broke = false
	}
	return out
}

// ApplyFill 成交回报（回测与执行器驱动），用于统计轮数与已实现利润。
func (g *Grid) ApplyFill(side exchange.Side, qty, price float64) {
	switch side {
	case exchange.Buy:
		g.baseQty += qty
		g.quoteCash -= qty * price
	case exchange.Sell:
		g.baseQty -= qty
		profit := qty * (price - g.buyRef())
		g.realized += profit
		g.rounds++
		g.quoteCash += qty * price
	}
}

func (g *Grid) buyRef() float64 {
	// 卖出利润参考买入成本：用当前格下一档的价格近似（简化口径）
	if g.lastIdx > 0 && g.lastIdx < len(g.levels) {
		return g.levels[g.lastIdx-1]
	}
	return g.params.Lower
}

func (g *Grid) buyIntent(level int, note string) strategy.OrderIntent {
	px := g.levels[level-1] // 在下一档挂买单
	return strategy.OrderIntent{
		Kind: "grid_buy", Side: exchange.Buy, Type: exchange.OrderLimit,
		Price: px, Qty: g.params.QtyPerGrid, Note: note,
	}
}

func (g *Grid) sellIntent(level int, note string) strategy.OrderIntent {
	px := g.levels[level] // 在上一档挂卖单
	return strategy.OrderIntent{
		Kind: "grid_sell", Side: exchange.Sell, Type: exchange.OrderLimit,
		Price: px, Qty: g.params.QtyPerGrid, Note: note,
	}
}

// Stats 运行统计。
type Stats struct {
	Rounds   int     `json:"rounds"`    // 完成的低买高卖轮数
	Realized float64 `json:"realized"`  // 已实现利润（Quote）
	Broke    bool    `json:"broke"`     // 是否处于打穿下界状态
	Position float64 `json:"position"`  // 当前持仓
}

func (g *Grid) Stats() Stats {
	return Stats{Rounds: g.rounds, Realized: g.realized, Broke: g.broke, Position: g.baseQty}
}
