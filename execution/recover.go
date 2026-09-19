// 启动恢复：进程重启后把交易所在途订单合并回本地订单簿，消除孤儿单。
package execution

import (
	"context"
	"fmt"
	"time"
)

// AdoptOpenOrders 启动合并：拉取交易所在途订单——本地 orders 没有的采纳（孤儿单认领，
// 写入 orders/byClient 并标记 claimed，emit kind="adopted"，journal 落 adopted 事件）；
// 本地已有的用交易所最新状态 applyUpdate 对齐。
// ClientOrderID 无论是否 qf- 前缀都标记 claimed：qf- 是本系统单必须防重发；
// 非本系统单（手工单/他系统单）同样占用幂等键，标记 claimed 防止本地复用同一 clientOrderID。
// 注意：采纳的订单不做本地资金 Freeze——重启后 Freeze 状态由对账流程重建，
// Executor 不管资金冻结重建。
func (e *Executor) AdoptOpenOrders(ctx context.Context, symbol string) (int, error) {
	open, err := e.Ex.GetOpenOrders(ctx, symbol)
	if err != nil {
		return 0, fmt.Errorf("execution: 拉取在途订单失败: %w", err)
	}
	adopted := 0
	for _, o := range open {
		if o.OrderID == "" {
			continue
		}
		if o.Symbol == "" {
			o.Symbol = symbol
		}
		e.mu.Lock()
		_, local := e.orders[o.OrderID]
		e.mu.Unlock()
		if local {
			e.applyUpdate(o) // 本地已有：以交易所状态推进（成交增量照常进账本）
			continue
		}
		e.mu.Lock()
		if _, dup := e.orders[o.OrderID]; dup { // 双重检查：并发注册/采纳竞争
			e.mu.Unlock()
			continue
		}
		if o.ClientOrderID != "" {
			e.byClient[o.ClientOrderID] = o.OrderID
			e.claimed[o.ClientOrderID] = true
		}
		if !o.Status.Terminal() { // 防御：在途列表不应含终态单，含则只认领幂等键不进挂单表
			e.orders[o.OrderID] = o
		}
		e.journalEvent("adopted", o)
		e.mu.Unlock()
		adopted++
		e.emit(Event{Ts: time.Now(), Kind: "adopted", Order: o})
	}
	return adopted, nil
}
