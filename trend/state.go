// state.go 趋势策略运行态持久化（生产实盘改造 T6）。
// 防前视：导出的只有"截至上一根已收盘 K 线"的运行态——入场价、跟踪止损锚点
// （入场后最高收盘）、上次信号根 OpenTime（冷却判重）。唐奇安/ATR 每根从
// ctx.Candles 全量重算（策略不缓存窗口索引），无窗口态可泄未来数据。
// 恢复语义 = "从未重启、跑到当前 K 线"：冷启动本就等待下一根确认，恢复后行为
// 与连续运行一致（往返等价测试验证，见 state_test.go）。
//
// 不导出的项：
//   - sizer：注入型决策器不可序列化，重启后按 New 默认（ATR 波动率目标）重建；
//     serve 路径本就只用默认值（半凯利仅研究注入）。
//   - cooldown：New 固定为 2 的重试节奏常量，非运行态。
//   - 持仓本身：以账本为准（ctx.Position），重启后 Seed/对账重建，不从状态恢复。
package trend

import (
	"encoding/json"
	"fmt"

	"github.com/HarveyBase/QuantForge/strategy"
)

var _ strategy.Stateful = (*Donchian)(nil)

const stateVersion = 1

// donchState 导出的运行态（version 字段留升级余地：不认识的老版本拒绝导入）。
type donchState struct {
	Version   int     `json:"version"`
	Config    Params  `json:"config"`               // 参数指纹（防漂移校验用；恢复参数的唯一来源仍是 config.json）
	EntryPx   float64 `json:"entry_px,omitempty"`   // 最近一次入场成交价（0 = 空仓锚点已清）
	PeakClose float64 `json:"peak_close,omitempty"` // 入场后最高收盘（跟踪止损锚点）
	SignalBar int64   `json:"signal_bar"`           // 上次发信号根 OpenTime（-1 = 从未发信号）
}

// ExportState 导出趋势运行态（JSON）。
func (d *Donchian) ExportState() (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := donchState{
		Version: stateVersion, Config: d.p,
		EntryPx: d.entryPx, PeakClose: d.peakClose, SignalBar: d.signalBar,
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("trend: 运行态序列化失败: %w", err)
	}
	return b, nil
}

// ImportState 导入趋势运行态。校验：版本一致、参数指纹一致（入场/出场/ATR 窗口、
// 止损倍数、风险与仓位上限任一变化即拒绝）、锚点非负（负值只可能来自手改状态文件）。
// 任一不过返回明确错误，由调用方决定冷启动。
func (d *Donchian) ImportState(raw json.RawMessage) error {
	var st donchState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("trend: 运行态解析失败: %w", err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("trend: 运行态版本不兼容（状态 %d，当前 %d）——请冷启动", st.Version, stateVersion)
	}
	if drift := paramsDrift(d.p, st.Config); drift != "" {
		return fmt.Errorf("trend: 参数漂移，拒绝导入运行态（%s）——请冷启动或改回原参数", drift)
	}
	if st.EntryPx < 0 || st.PeakClose < 0 {
		return fmt.Errorf("trend: 运行态锚点非法（entry_px=%v peak_close=%v 不得为负）", st.EntryPx, st.PeakClose)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entryPx = st.EntryPx
	d.peakClose = st.PeakClose
	d.signalBar = st.SignalBar
	return nil
}

// paramsDrift 参数指纹比对：返回首个差异描述（空串 = 一致）。
func paramsDrift(cur, saved Params) string {
	switch {
	case cur.EntryN != saved.EntryN:
		return fmt.Sprintf("entry_n %d → %d", saved.EntryN, cur.EntryN)
	case cur.ExitN != saved.ExitN:
		return fmt.Sprintf("exit_n %d → %d", saved.ExitN, cur.ExitN)
	case cur.AtrN != saved.AtrN:
		return fmt.Sprintf("atr_n %d → %d", saved.AtrN, cur.AtrN)
	case cur.AtrMult != saved.AtrMult:
		return fmt.Sprintf("atr_mult %v → %v", saved.AtrMult, cur.AtrMult)
	case cur.RiskPct != saved.RiskPct:
		return fmt.Sprintf("risk_pct %v → %v", saved.RiskPct, cur.RiskPct)
	case cur.MaxPosPct != saved.MaxPosPct:
		return fmt.Sprintf("max_pos_pct %v → %v", saved.MaxPosPct, cur.MaxPosPct)
	}
	return ""
}
