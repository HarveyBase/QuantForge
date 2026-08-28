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
