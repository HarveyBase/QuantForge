// Package strategy 组合策略：grid 与 trend 并行、按权重分配资金。
// 纪律：regime 自动路由默认关闭（docs/10 §6：互补性在真实样本上不成立，
// 无 OOS 证据不开自动切换）；开启后仅在防抖确认的市况下路由给对应策略。
package strategy

import (
	"github.com/HarveyBase/QuantForge/exchange"
)

// Composite 双策略组合：子策略各自看到"自己的资金份额"（Equity×权重），
// 信号合并产出；ApplyFill 回调广播给全部子策略（各自维护状态）。
type Composite struct {
	subs []subStrategy
}

type subStrategy struct {
	name    string
	weight  float64 // 资金权重 ∈ (0,1]，子策略的 Equity = 总权益 × weight
	s       Strategy
	enabled bool // regime 路由下本根是否激活（无路由时恒 true）
}

// NewComposite 构造组合（weights 与 subs 一一对应，权重和允许 <1：剩余为现金缓冲——永不满仓）。
func NewComposite(names []string, subs []Strategy, weights []float64) *Composite {
	c := &Composite{}
	for i, s := range subs {
		w := 0.5
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		c.subs = append(c.subs, subStrategy{name: names[i], weight: w, s: s, enabled: true})
	}
	return c
}

func (c *Composite) Name() string { return "composite" }
func (c *Composite) Warmup() int {
	w := 1
	for _, sub := range c.subs {
		if sub.s.Warmup() > w {
			w = sub.s.Warmup()
		}
	}
	return w
}

// SetActive 按（可选的）regime 路由结果启停子策略；未启用的子策略本根不产信号。
func (c *Composite) SetActive(name string, active bool) {
	for i := range c.subs {
		if c.subs[i].name == name {
			c.subs[i].enabled = active
		}
	}
}

// OnCandle 子策略各自以份额权益产出信号，合并返回。
func (c *Composite) OnCandle(ctx *Context) []OrderIntent {
	var out []OrderIntent
	for _, sub := range c.subs {
		if !sub.enabled {
			continue
		}
		subCtx := *ctx // 浅拷贝：子策略只拿到自己的资金视图
		subCtx.Equity = ctx.Equity * sub.weight
		subCtx.Cash = ctx.Cash * sub.weight
		out = append(out, sub.s.OnCandle(&subCtx)...)
	}
	return out
}

// ApplyFill 广播给实现了回调的子策略（grid/trend 均实现）。
func (c *Composite) ApplyFill(side exchange.Side, qty, price float64) {
	for _, sub := range c.subs {
		if applier, ok := sub.s.(interface {
			ApplyFill(exchange.Side, float64, float64)
		}); ok {
			applier.ApplyFill(side, qty, price)
		}
	}
}

// Describe 组合描述（复盘留痕）。
func (c *Composite) Describe() string {
	out := "composite:"
	for i, sub := range c.subs {
		if i > 0 {
			out += "+"
		}
		state := ""
		if !sub.enabled {
			state = "(停)"
		}
		if d, ok := sub.s.(interface{ Describe() string }); ok {
			out += sub.name + state + "[" + d.Describe() + "]"
		} else {
			out += sub.name + state
		}
	}
	return out
}
