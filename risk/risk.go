// Package risk 风控前置：限额、频率、敞口、当日回撤停机、Kill Switch、拒单台账。
package risk

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/portfolio"
)

type Limits struct {
	MaxOrderNotionalUSD    float64
	MaxDailyNotionalUSD    float64
	MaxPositionNotionalUSD float64
	MaxOrdersPerMinute     int
	MaxDailyLossPct        float64
	CooldownAfterRejectSec int
}

type Rejection struct {
	Ts     time.Time             `json:"ts"`
	RuleID string                `json:"rule_id"`
	Reason string                `json:"reason"`
	Order  exchange.OrderRequest `json:"order"`
}

type KillSwitch struct {
	tripped   atomic.Bool
	reason    atomic.Value
	mu        sync.Mutex
	listeners []func(string)
}

func (k *KillSwitch) Trip(reason string) {
	if reason == "" {
		reason = "未说明原因"
	}
	k.reason.Store(reason)
	first := k.tripped.CompareAndSwap(false, true)
	if !first {
		return
	}
	k.mu.Lock()
	listeners := append([]func(string){}, k.listeners...)
	k.mu.Unlock()
	for _, fn := range listeners {
		fn(reason)
	}
}
func (k *KillSwitch) Reset()        { k.tripped.Store(false); k.reason.Store("") }
func (k *KillSwitch) Tripped() bool { return k.tripped.Load() }
func (k *KillSwitch) Reason() string {
	if v, ok := k.reason.Load().(string); ok {
		return v
	}
	return ""
}
func (k *KillSwitch) OnTrip(fn func(string)) {
	if fn == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listeners = append(k.listeners, fn)
}
func (k *KillSwitch) Restore(tripped bool, reason string) {
	k.reason.Store(reason)
	k.tripped.Store(tripped)
}

type Manager struct {
	Limits           Limits
	Kill             KillSwitch
	Pf               *portfolio.Portfolio
	mu               sync.Mutex
	orderTimes       []time.Time
	dailyNotional    float64
	day              string
	dayStartEq       float64
	rejections       []Rejection
	ledgerPath       string
	cooldownUntil    time.Time
	reconcileBlocked bool
	reconcileReason  string
}

func NewManager(l Limits, pf *portfolio.Portfolio, ledgerPath string) *Manager {
	m := &Manager{Limits: l, Pf: pf, ledgerPath: ledgerPath, dayStartEq: pf.Equity()}
	m.rollDayIfNeeded()
	return m
}
func (m *Manager) rollDayIfNeeded() {
	today := time.Now().UTC().Format("2006-01-02")
	if m.day != today {
		m.day = today
		m.dailyNotional = 0
		m.dayStartEq = m.Pf.Equity()
	}
}

func (m *Manager) CheckOrder(req exchange.OrderRequest, markPrice float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDayIfNeeded()
	now := time.Now()
	if req.Symbol == "" {
		return m.reject(req, "INVALID_SYMBOL", "交易标的不能为空")
	}
	if req.Side != exchange.Buy && req.Side != exchange.Sell {
		return m.reject(req, "INVALID_SIDE", "订单方向非法 %q", req.Side)
	}
	if req.Type != exchange.OrderLimit && req.Type != exchange.OrderMarket {
		return m.reject(req, "INVALID_TYPE", "订单类型非法 %q", req.Type)
	}
	if req.Qty <= 0 || math.IsNaN(req.Qty) || math.IsInf(req.Qty, 0) {
		return m.reject(req, "INVALID_QTY", "订单数量必须为有限正数，当前 %v", req.Qty)
	}
	price := req.Price
	if req.Type == exchange.OrderLimit && (price <= 0 || math.IsNaN(price) || math.IsInf(price, 0)) {
		return m.reject(req, "INVALID_PRICE", "限价单价格必须为有限正数，当前 %v", price)
	}
	if req.Type == exchange.OrderMarket {
		price = markPrice
	}
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return m.reject(req, "INVALID_MARK", "市价单缺少有效标记价，当前 %v", price)
	}
	notional := price * req.Qty
	if m.Kill.Tripped() {
		return m.reject(req, "KILL_SWITCH", "Kill Switch 已触发（%s），人工复位前禁止一切新下单", m.Kill.Reason())
	}
	if m.reconcileBlocked {
		return m.reject(req, "RECONCILE_BLOCKED", "账户对账异常，禁止新下单：%s", m.reconcileReason)
	}
	if now.Before(m.cooldownUntil) {
		return m.reject(req, "COOLDOWN", "拒单冷静期内（%s 前）", m.cooldownUntil.Format(time.RFC3339))
	}
	if notional > m.Limits.MaxOrderNotionalUSD {
		return m.reject(req, "MAX_ORDER_NOTIONAL", "单笔名义 %.2f 超限 %.2f", notional, m.Limits.MaxOrderNotionalUSD)
	}
	if m.dailyNotional+notional > m.Limits.MaxDailyNotionalUSD {
		return m.reject(req, "MAX_DAILY_NOTIONAL", "当日累计名义 %.2f + 本单 %.2f 超限 %.2f", m.dailyNotional, notional, m.Limits.MaxDailyNotionalUSD)
	}
	cutoff := now.Add(-time.Minute)
	kept := m.orderTimes[:0]
	for _, ts := range m.orderTimes {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	m.orderTimes = kept
	if len(m.orderTimes) >= m.Limits.MaxOrdersPerMinute {
		return m.reject(req, "ORDER_RATE", "1 分钟内下单 %d 次已达上限 %d", len(m.orderTimes), m.Limits.MaxOrdersPerMinute)
	}
	projected := m.Pf.PositionNotional(req.Symbol)
	if req.Side == exchange.Buy {
		projected += notional
	}
	if projected > m.Limits.MaxPositionNotionalUSD {
		return m.reject(req, "MAX_POSITION_NOTIONAL", "敞口 %.2f 超限 %.2f", projected, m.Limits.MaxPositionNotionalUSD)
	}
	cash, positions, _ := m.Pf.Snapshot()
	if req.Side == exchange.Buy && notional > cash+1e-12 {
		return m.reject(req, "INSUFFICIENT_CASH", "可用现金 %.2f 不足（委托名义 %.2f）", cash, notional)
	}
	if req.Side == exchange.Sell {
		available := 0.0
		for _, pos := range positions {
			if pos.Symbol == req.Symbol {
				available = pos.Available
				break
			}
		}
		if req.Qty > available+1e-12 {
			return m.reject(req, "INSUFFICIENT_AVAILABLE", "可卖 %.8f 不足（委托 %.8f）", available, req.Qty)
		}
	}
	if eq := m.Pf.Equity(); m.dayStartEq > 0 && eq < m.dayStartEq*(1-m.Limits.MaxDailyLossPct/100) {
		reason := fmt.Sprintf("当日回撤超 %.1f%%: %.2f -> %.2f", m.Limits.MaxDailyLossPct, m.dayStartEq, eq)
		m.Kill.Trip(reason)
		return m.reject(req, "MAX_DAILY_LOSS", "当日权益 %.2f 较起始 %.2f 回撤超限，自动停机", eq, m.dayStartEq)
	}
	m.orderTimes = append(m.orderTimes, now)
	m.dailyNotional += notional
	return nil
}

func (m *Manager) OnOrderAccepted(exchange.OrderRequest) {}
func (m *Manager) StartCooldown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Limits.CooldownAfterRejectSec > 0 {
		m.cooldownUntil = time.Now().Add(time.Duration(m.Limits.CooldownAfterRejectSec) * time.Second)
	}
}
func (m *Manager) BlockForReconcile(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileBlocked = true
	m.reconcileReason = reason
}
func (m *Manager) ClearReconcileBlock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileBlocked = false
	m.reconcileReason = ""
}
func (m *Manager) ReconcileBlocked() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcileBlocked, m.reconcileReason
}

// maxRejections 内存台账上限：超过后丢弃最旧（完整记录已落盘 ledger.jsonl，内存只留最近窗口）。
const maxRejections = 10000

func (m *Manager) reject(req exchange.OrderRequest, ruleID, format string, args ...any) error {
	r := Rejection{Ts: time.Now().UTC(), RuleID: ruleID, Reason: fmt.Sprintf(format, args...), Order: req}
	m.rejections = append(m.rejections, r)
	if len(m.rejections) > maxRejections {
		m.rejections = m.rejections[len(m.rejections)-maxRejections:]
	}
	if m.ledgerPath != "" {
		if b, err := json.Marshal(r); err == nil {
			if f, err := os.OpenFile(m.ledgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				_, _ = f.Write(append(b, '\n'))
				_ = f.Close()
			}
		}
	}
	return fmt.Errorf("risk: [%s] %s", ruleID, r.Reason)
}
func (m *Manager) Rejections() []Rejection {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Rejection, len(m.rejections))
	copy(out, m.rejections)
	return out
}
func (m *Manager) DailyNotionalUsed() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dailyNotional
}
func (m *Manager) SetDayStartEquity(eq float64) { m.mu.Lock(); defer m.mu.Unlock(); m.dayStartEq = eq }
