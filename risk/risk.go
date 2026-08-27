// Package risk 风控前置：限额、频率、敞口、当日回撤停机、Kill Switch、拒单台账。
// 纪律（docs/01、docs/08）：拦截在下单之前，拒单留痕绝不静默；三阶段（回测/模拟/实盘）同一条路径。
package risk

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
)

// Limits 风控限额（从 config.RiskConfig 传入）。
type Limits struct {
	MaxOrderNotionalUSD    float64
	MaxDailyNotionalUSD    float64
	MaxPositionNotionalUSD float64
	MaxOrdersPerMinute     int
	MaxDailyLossPct        float64
	CooldownAfterRejectSec int
}

// Rejection 风控拒单记录（JSONL 台账）。
type Rejection struct {
	Ts      time.Time `json:"ts"`
	RuleID  string    `json:"rule_id"`
	Reason  string    `json:"reason"`
	Order   exchange.OrderRequest `json:"order"`
}

// KillSwitch 停机开关：触发后撤单+平仓+停机，人工复位才能恢复。
type KillSwitch struct {
	tripped atomic.Bool
	reason  atomic.Value // string
}

func (k *KillSwitch) Trip(reason string) { k.reason.Store(reason); k.tripped.Store(true) }
func (k *KillSwitch) Reset()             { k.tripped.Store(false) }
func (k *KillSwitch) Tripped() bool      { return k.tripped.Load() }
func (k *KillSwitch) Reason() string {
	if v, ok := k.reason.Load().(string); ok {
		return v
	}
	return ""
}

// Manager 风控管理器：CheckOrder 是唯一的下单前门禁入口。
type Manager struct {
	Limits    Limits
	Kill      KillSwitch
	Pf        *portfolio.Portfolio

	mu          sync.Mutex
	orderTimes  []time.Time   // 频率窗口
	dailyNotional float64     // 当日累计名义
	day         string        // 当日（UTC 日期，切换时清零）
	dayStartEq  float64       // 当日起始权益
	rejections  []Rejection
	ledgerPath  string        // 拒单台账 JSONL（空=不落盘）
	cooldownUntil time.Time
}

func NewManager(l Limits, pf *portfolio.Portfolio, ledgerPath string) *Manager {
	m := &Manager{Limits: l, Pf: pf, ledgerPath: ledgerPath, dayStartEq: pf.Equity()}
	m.rollDayIfNeeded()
	return m
}

// rollDayIfNeeded 跨日重置当日累计。
func (m *Manager) rollDayIfNeeded() {
	today := time.Now().UTC().Format("2006-01-02")
	if m.day != today {
		m.day = today
		m.dailyNotional = 0
		m.dayStartEq = m.Pf.Equity()
	}
}

// CheckOrder 下单前门禁：返回 nil 放行，否则拒单（附带 rule_id）。
// 网格等策略的每张单、市价单、撤单后重挂，都必须过这道门。
func (m *Manager) CheckOrder(req exchange.OrderRequest, markPrice float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDayIfNeeded()

	now := time.Now()
	price := req.Price
	if price == 0 { // 市价单用标记价估算名义
		price = markPrice
	}
	notional := price * req.Qty

	if m.Kill.Tripped() {
		return m.reject("KILL_SWITCH", "Kill Switch 已触发（%s），人工复位前禁止一切新下单", m.Kill.Reason())
	}
	if now.Before(m.cooldownUntil) {
		return m.reject("COOLDOWN", "拒单冷静期内（%s 前）", m.cooldownUntil.Format(time.RFC3339))
	}
	if notional > m.Limits.MaxOrderNotionalUSD {
		return m.reject("MAX_ORDER_NOTIONAL", "单笔名义 %.2f 超限 %.2f", notional, m.Limits.MaxOrderNotionalUSD)
	}
	if m.dailyNotional+notional > m.Limits.MaxDailyNotionalUSD {
		return m.reject("MAX_DAILY_NOTIONAL", "当日累计名义 %.2f + 本单 %.2f 超限 %.2f",
			m.dailyNotional, notional, m.Limits.MaxDailyNotionalUSD)
	}
	// 频率：滑动窗口内的下单次数
	cutoff := now.Add(-time.Minute)
	kept := m.orderTimes[:0]
	for _, ts := range m.orderTimes {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	m.orderTimes = append(kept, now)
	if len(m.orderTimes) > m.Limits.MaxOrdersPerMinute {
		return m.reject("ORDER_RATE", "1 分钟内下单 %d 次超限 %d", len(m.orderTimes), m.Limits.MaxOrdersPerMinute)
	}
	// 敞口：现有 + 本单增量（买入加仓才增加多头敞口）
	projected := m.Pf.PositionNotional(req.Symbol)
	if req.Side == exchange.Buy {
		projected += notional
	}
	if projected > m.Limits.MaxPositionNotionalUSD {
		return m.reject("MAX_POSITION_NOTIONAL", "敞口 %.2f 超限 %.2f", projected, m.Limits.MaxPositionNotionalUSD)
	}
	// 卖出校验可卖数量（现货纪律：卖 available 不卖 position）
	if req.Side == exchange.Sell {
		_, positions, _ := m.Pf.Snapshot()
		for _, pos := range positions {
			if pos.Symbol == req.Symbol && req.Qty > pos.Available+1e-12 {
				return m.reject("INSUFFICIENT_AVAILABLE", "可卖 %.8f 不足（委托 %.8f），禁止卖出未冻结/不可用持仓", pos.Available, req.Qty)
			}
		}
	}
	// 当日回撤停机：亏损超阈值自动触发 Kill Switch（如 5%）
	if eq := m.Pf.Equity(); m.dayStartEq > 0 && eq < m.dayStartEq*(1-m.Limits.MaxDailyLossPct/100) {
		m.Kill.Trip(fmt.Sprintf("当日回撤超 %.1f%%: %.2f -> %.2f", m.Limits.MaxDailyLossPct, m.dayStartEq, eq))
		return m.reject("MAX_DAILY_LOSS", "当日权益 %.2f 较起始 %.2f 回撤超限，自动停机", eq, m.dayStartEq)
	}

	m.dailyNotional += notional
	return nil
}

// OnOrderAccepted 订单通过风控并提交后登记（内部计数已含频率，此处留扩展点）。
func (m *Manager) OnOrderAccepted(req exchange.OrderRequest) {}

// StartCooldown 拒单后进入冷静期。
func (m *Manager) StartCooldown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Limits.CooldownAfterRejectSec > 0 {
		m.cooldownUntil = time.Now().Add(time.Duration(m.Limits.CooldownAfterRejectSec) * time.Second)
	}
}

// reject 记录拒单台账并返回 error。
func (m *Manager) reject(ruleID, format string, args ...any) error {
	r := Rejection{
		Ts: time.Now().UTC(), RuleID: ruleID, Reason: fmt.Sprintf(format, args...),
	}
	m.rejections = append(m.rejections, r)
	if m.ledgerPath != "" {
		if b, err := json.Marshal(r); err == nil {
			f, err := os.OpenFile(m.ledgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = f.Write(append(b, '\n'))
				_ = f.Close()
			}
		}
	}
	return fmt.Errorf("risk: [%s] %s", ruleID, r.Reason)
}

// Rejections 拒单台账只读副本。
func (m *Manager) Rejections() []Rejection {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Rejection, len(m.rejections))
	copy(out, m.rejections)
	return out
}

// DailyNotionalUsed 当日已用名义额度。
func (m *Manager) DailyNotionalUsed() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dailyNotional
}
