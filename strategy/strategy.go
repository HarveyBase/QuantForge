// Package strategy 策略接口与运行上下文。
// 防前视铁律：Context 只暴露当前及历史 K 线，策略拿不到任何未来数据（docs/01、docs/02）。
package strategy

import (
	"github.com/HarveyBase/QuantForge/exchange"
)

// Context 策略可见的全部世界：截至当前已收盘的 K 线 + 账户状态。
type Context struct {
	Symbol   string
	Interval string
	Candles  []exchange.Candle // 时间升序、全部已收盘，最后一根为当前根
	Equity   float64           // 当前总权益
	Position float64           // 当前净持仓（Base）
	Cash     float64
}

// Last 最新一根已收盘 K 线。
func (c *Context) Last() exchange.Candle {
	if len(c.Candles) == 0 {
		return exchange.Candle{}
	}
	return c.Candles[len(c.Candles)-1]
}

// OrderIntent 策略产出的订单意图（未过风控、未提交）。
type OrderIntent struct {
	Kind  string             `json:"kind"` // open / close / rebalance 等（策略自定义语义）
	Side  exchange.Side      `json:"side"`
	Type  exchange.OrderType `json:"type"`
	Price float64            `json:"price"`
	Qty   float64            `json:"qty"`
	Note  string             `json:"note,omitempty"`
}

// Strategy 策略接口。OnCandle 在每根 K 线收盘后调用一次，返回本根要下的订单意图。
// 意图经风控门禁与执行器落地——策略本身永远不直接碰交易所。
type Strategy interface {
	Name() string
	// Warmup 需要的最少 K 线数（指标预热期）。
	Warmup() int
	OnCandle(ctx *Context) []OrderIntent
}

// Registry 策略注册表（cmd 装配用）。
var Registry = map[string]func() Strategy{}

func Register(name string, ctor func() Strategy) {
	Registry[name] = ctor
}
