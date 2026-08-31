package strategy

import (
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestContextLast(t *testing.T) {
	var empty Context
	if (empty.Last() != exchange.Candle{}) {
		t.Fatal("空上下文 Last 应返回零值 K 线")
	}
	c := &Context{Candles: []exchange.Candle{
		{Close: 100, OpenTime: 1},
		{Close: 105, OpenTime: 2},
	}}
	if last := c.Last(); last.Close != 105 || last.OpenTime != 2 {
		t.Fatalf("Last 应返回最新一根: %+v", last)
	}
}

func TestRegistryRegister(t *testing.T) {
	ctor := func() Strategy { return nil }
	Register("test_strategy", ctor)
	if got, ok := Registry["test_strategy"]; !ok || got == nil {
		t.Fatal("注册表应能取回已注册构造器")
	}
	// 重复注册覆盖旧值
	ctor2 := func() Strategy { return &dummyStrategy{} }
	Register("test_strategy", ctor2)
	if s := Registry["test_strategy"](); s == nil {
		t.Fatal("覆盖注册后构造器应可调用")
	}
	delete(Registry, "test_strategy")
}

type dummyStrategy struct{}

func (d *dummyStrategy) Name() string                    { return "dummy" }
func (d *dummyStrategy) Warmup() int                     { return 1 }
func (d *dummyStrategy) OnCandle(*Context) []OrderIntent { return nil }

// dummySig 固定信号策略（composite 测试用）。
type dummySig struct {
	name  string
	buys  int
	fills int
}

func (d *dummySig) Name() string { return d.name }
func (d *dummySig) Warmup() int  { return 1 }
func (d *dummySig) OnCandle(ctx *Context) []OrderIntent {
	d.buys++
	return []OrderIntent{{Side: exchange.Buy, Type: exchange.OrderMarket, Qty: ctx.Equity / 100}}
}
func (d *dummySig) ApplyFill(exchange.Side, float64, float64) { d.fills++ }
func (d *dummySig) Describe() string                          { return d.name + "-desc" }

func TestCompositeWeightedSignals(t *testing.T) {
	g, tr := &dummySig{name: "grid"}, &dummySig{name: "trend"}
	c := NewComposite([]string{"grid", "trend"},
		[]Strategy{g, tr}, []float64{0.4, 0.6})
	if c.Name() != "composite" || c.Warmup() != 1 {
		t.Fatal("元数据错误")
	}
	ctx := &Context{Equity: 10000, Cash: 10000, Candles: []exchange.Candle{{Close: 100, Confirmed: true}}}
	out := c.OnCandle(ctx)
	if len(out) != 2 {
		t.Fatalf("双子策略应各出 1 信号: %d", len(out))
	}
	// 权重资金视图：grid qty = 4000/100 = 40，trend = 6000/100 = 60
	if out[0].Qty != 40 || out[1].Qty != 60 {
		t.Fatalf("权重分配错误: %+v", out)
	}
	// ApplyFill 广播
	c.ApplyFill(exchange.Buy, 1, 100)
	if g.fills != 1 || tr.fills != 1 {
		t.Fatal("回调必须广播")
	}
	// 路由停用
	c.SetActive("grid", false)
	out2 := c.OnCandle(ctx)
	if len(out2) != 1 {
		t.Fatalf("停用后应只剩 trend 信号: %d", len(out2))
	}
	if !containsStr(c.Describe(), "(停)") {
		t.Fatalf("描述应标注停用: %s", c.Describe())
	}
	// 重新启用
	c.SetActive("grid", true)
	if len(c.OnCandle(ctx)) != 2 {
		t.Fatal("重新启用应恢复")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
