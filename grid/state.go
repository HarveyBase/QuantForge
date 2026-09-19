// state.go 网格策略运行态持久化（生产实盘改造 T6）。
// 背景：started/lastIdx/broke 原为内存态，进程重启后网格按当前持仓自举，与交易所
// 遗留挂单（AdoptOpenOrders 已认领）错位，可能重复挂格。导出/导入消除该错位。
//
// 纪律：
//   - 只导运行态（started/lastIdx/broke/轮数/已实现利润/账本镜像），不导配置——
//     配置由 config.json 重建，Import 时校验配置指纹一致，漂移即报错（调用方冷启动）；
//   - 防幽灵卖单不靠内存映射而是每根从 ctx.Position 推导（OnCandle 内 availableQty），
//     无需导出——重启后 AdoptOpenOrders + 账本对账重建持仓，下一根收盘自然对齐；
//   - baseQty/quoteCash 只是 ApplyFill 驱动的统计镜像（Stats 展示与审计连续性），
//     持仓真相仍在交易所/账本，此镜像不影响交易语义（OnCandle 用 ctx.Position）。
package grid

import (
	"encoding/json"
	"fmt"

	"github.com/HarveyBase/QuantForge/strategy"
)

var _ strategy.Stateful = (*Grid)(nil)

const stateVersion = 1

// gridState 导出的运行态（version 字段留升级余地：不认识的老版本拒绝导入）。
// 浮点字段由 encoding/json 保证往返精确与数值合法（NaN/Inf 无法表示，越界数解析报错）。
type gridState struct {
	Version   int     `json:"version"`
	Config    Params  `json:"config"` // 配置指纹（防漂移校验用；恢复配置的唯一来源仍是 config.json）
	Started   bool    `json:"started"`
	LastIdx   int     `json:"last_idx"`
	Broke     bool    `json:"broke"`
	Rounds    int     `json:"rounds"`
	Realized  float64 `json:"realized"`   // 已实现利润（Quote，运行态统计）
	BaseQty   float64 `json:"base_qty"`   // 账本镜像：持仓（Base），仅统计展示
	QuoteCash float64 `json:"quote_cash"` // 账本镜像：现金（Quote），仅统计展示
}

// ExportState 导出网格运行态（JSON）。
func (g *Grid) ExportState() (json.RawMessage, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	st := gridState{
		Version: stateVersion, Config: g.params,
		Started: g.started, LastIdx: g.lastIdx, Broke: g.broke,
		Rounds: g.rounds, Realized: g.realized,
		BaseQty: g.baseQty, QuoteCash: g.quoteCash,
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("grid: 运行态序列化失败: %w", err)
	}
	return b, nil
}

// ImportState 导入网格运行态。校验：版本一致、配置指纹一致（格数/边界/间距/每格数量/
// 打穿开关任一变化即拒绝）、lastIdx 在网格线范围内、镜像非负。任一不过返回明确错误，
// 由调用方决定冷启动。
func (g *Grid) ImportState(raw json.RawMessage) error {
	var st gridState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("grid: 运行态解析失败: %w", err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("grid: 运行态版本不兼容（状态 %d，当前 %d）——请冷启动", st.Version, stateVersion)
	}
	if drift := configDrift(g.params, st.Config); drift != "" {
		return fmt.Errorf("grid: 配置漂移，拒绝导入运行态（%s）——请冷启动或改回原配置", drift)
	}
	if st.LastIdx < 0 || st.LastIdx > g.params.Grids {
		return fmt.Errorf("grid: 运行态 last_idx=%d 越界（0~%d）", st.LastIdx, g.params.Grids)
	}
	if st.Rounds < 0 || st.BaseQty < 0 {
		return fmt.Errorf("grid: 运行态非法（rounds=%d base_qty=%v 不得为负）", st.Rounds, st.BaseQty)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.started = st.Started
	g.lastIdx = st.LastIdx
	g.broke = st.Broke
	g.rounds = st.Rounds
	g.realized = st.Realized
	g.baseQty = st.BaseQty
	g.quoteCash = st.QuoteCash
	return nil
}

// configDrift 配置指纹比对：返回首个差异描述（空串 = 一致）。
// 比对用 New 规范化后的参数（非法 Spacing 归一为 geo，两侧构造口径一致）。
func configDrift(cur, saved Params) string {
	switch {
	case cur.Lower != saved.Lower:
		return fmt.Sprintf("lower %v → %v", saved.Lower, cur.Lower)
	case cur.Upper != saved.Upper:
		return fmt.Sprintf("upper %v → %v", saved.Upper, cur.Upper)
	case cur.Grids != saved.Grids:
		return fmt.Sprintf("grids %d → %d", saved.Grids, cur.Grids)
	case cur.QtyPerGrid != saved.QtyPerGrid:
		return fmt.Sprintf("qty_per_grid %v → %v", saved.QtyPerGrid, cur.QtyPerGrid)
	case cur.Spacing != saved.Spacing:
		return fmt.Sprintf("spacing %q → %q", saved.Spacing, cur.Spacing)
	case cur.StopOnBreak != saved.StopOnBreak:
		return fmt.Sprintf("stop_on_break %v → %v", saved.StopOnBreak, cur.StopOnBreak)
	}
	return ""
}
