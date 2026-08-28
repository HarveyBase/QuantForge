package dashboard

import (
	"context"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/execution"
)

// OrderSource 执行层数据视图：research 模式没有执行器时用 NoopExecutor。
type OrderSource interface {
	OpenOrders() []exchange.Order
	Events(limit int) []execution.Event
	CancelAll(ctx context.Context, symbol string) int
}

// NoopExecutor research 模式的空执行器（无订单、无事件、撤单无效）。
type NoopExecutor struct{}

func (NoopExecutor) OpenOrders() []exchange.Order          { return nil }
func (NoopExecutor) Events(int) []execution.Event          { return nil }
func (NoopExecutor) CancelAll(context.Context, string) int { return 0 }
