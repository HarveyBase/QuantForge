// state.go 可选状态持久化接口（生产实盘改造 T6：策略运行态跨进程重启延续）。
// 纪律：
//   - 只导运行态（游标/持仓标志/止损锚点等），不导配置——配置由 config.json 重建；
//   - 持仓真相在账本/交易所（AdoptOpenOrders + 对账重建），状态恢复不引入任何未来数据
//     （防前视：快照只反映"截至上一根已收盘 K 线"的运行态）；
//   - 不实现本接口的策略照常工作：调用方类型断言，断言失败即冷启动并留痕。
package strategy

import "encoding/json"

// Stateful 可选状态持久化接口：策略运行态跨进程重启延续。
// ExportState 导出当前运行态（JSON，自包含版本号）；ImportState 导入运行态并校验
// 与当前配置一致性——配置漂移（如格数/窗口变了）必须返回明确错误，由调用方决定
// 冷启动，绝不静默套用不匹配的旧状态。
type Stateful interface {
	ExportState() (json.RawMessage, error)
	ImportState(raw json.RawMessage) error
}
