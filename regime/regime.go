// Package regime 市况识别（docs/07：策略与市况错配是最大风险源）。
// 用 Kaufman 效率比（ER）区分趋势市 / 震荡市，带确认防抖：
// 连续 confirmBars 根同分类才切换状态，避免临界区来回抖动。
// 用途：网格（震荡市）与趋势突破（趋势市）的切换依据；
// 当前定位为信息面（复盘留痕 + dashboard 展示），自动路由需回测证据后开启。
package regime

import (
	"fmt"
	"math"
	"sync"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/indicators"
)

// Kind 市况分类。
type Kind string

const (
	Trending Kind = "trending" // 趋势市：适合突破/趋势策略
	Range    Kind = "range"    // 震荡市：适合网格
	Mixed    Kind = "mixed"    // 过渡带：证据不足，默认谨慎（维持原状）
)

// String 便于 JSON 与日志。
func (k Kind) String() string { return string(k) }

// 经典阈值：ER>0.35 趋势、ER<0.20 震荡（Kaufman 原文经验值，中间为过渡带）。
const (
	DefaultThresholdTrend = 0.35
	DefaultThresholdRange = 0.20
	DefaultLookback       = 24 // 24 根 1H ≈ 一天
	DefaultConfirmBars    = 3  // 连续 3 根同分类才切换
)

// Reading 单次市况读数（每根收盘更新）。
type Reading struct {
	Kind     Kind    `json:"kind"` // 防抖后的当前市况
	Raw      Kind    `json:"raw"`  // 本根的原始分类（未防抖）
	ER       float64 `json:"er"`   // 效率比
	Lookback int     `json:"lookback"`
	Confirm  int     `json:"confirm"` // 防抖确认根数
	Candles  int     `json:"candles"` // 可用根数（不足 lookback 时为未知态）
}

// Detector 市况识别器（有状态，线程安全）。
type Detector struct {
	mu sync.Mutex

	lookback int
	confirm  int
	thTrend  float64
	thRange  float64

	current Kind
	votes   []Kind // 最近原始分类（confirm 容量）
}

// NewDetector 构造识别器。lookback ≤0 用默认；confirm ≤0 用默认。
func NewDetector(lookback, confirm int) *Detector {
	if lookback <= 0 {
		lookback = DefaultLookback
	}
	if confirm <= 0 {
		confirm = DefaultConfirmBars
	}
	return &Detector{
		lookback: lookback, confirm: confirm,
		thTrend: DefaultThresholdTrend, thRange: DefaultThresholdRange,
		current: Mixed,
	}
}

// Update 每根收盘 K 线后调用：算 ER → 原始分类 → 防抖投票 → 返回读数。
func (d *Detector) Update(candles []exchange.Candle) Reading {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := Reading{Kind: d.current, Lookback: d.lookback, Confirm: d.confirm, Candles: len(candles)}
	if len(candles) < d.lookback+1 {
		return r // 数据不足：维持现状（宁迟滞不瞎猜）
	}
	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}
	er := indicators.EfficiencyRatio(closes, d.lookback)[len(closes)-1]
	if math.IsNaN(er) {
		return r
	}
	r.ER = er
	switch {
	case er >= d.thTrend:
		r.Raw = Trending
	case er <= d.thRange:
		r.Raw = Range
	default:
		r.Raw = Mixed
	}
	// 防抖：最近 confirm 根原始分类全一致才切换
	d.votes = append(d.votes, r.Raw)
	if len(d.votes) > d.confirm {
		d.votes = d.votes[len(d.votes)-d.confirm:]
	}
	if len(d.votes) == d.confirm && allSame(d.votes) {
		d.current = d.votes[0]
	}
	r.Kind = d.current
	return r
}

// Current 当前防抖后市况。
func (d *Detector) Current() Kind {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

// Describe 人读摘要（复盘与日志留痕）。
func (r Reading) Describe() string {
	if r.Candles <= r.Lookback {
		return fmt.Sprintf("市况未知（数据 %d 根不足 lookback %d）", r.Candles, r.Lookback)
	}
	return fmt.Sprintf("市况 %s（ER %.2f，原始 %s，防抖 %d 根）", r.Kind, r.ER, r.Raw, r.Confirm)
}

func allSame(ks []Kind) bool {
	for _, k := range ks[1:] {
		if k != ks[0] {
			return false
		}
	}
	return true
}

// Segment 一段同市况的连续区间。
type Segment struct {
	Kind Kind  `json:"kind"`
	From int64 `json:"from"` // 起始根 OpenTime（含）
	To   int64 `json:"to"`   // 结束根 OpenTime（含）
	Bars int   `json:"bars"`
}

// Segments 把序列按防抖后市况切段（研究用：lab 对每段分别回测两策略，验证市况互补性）。
// 每根收盘喂给独立 Detector，状态变化即断段。
func Segments(candles []exchange.Candle, lookback, confirm int) []Segment {
	d := NewDetector(lookback, confirm)
	var out []Segment
	cur := Kind("")
	start := 0
	for i := range candles {
		k := d.Update(candles[:i+1]).Kind
		if k != cur {
			if cur != "" && i > start {
				out = append(out, Segment{Kind: cur, From: candles[start].OpenTime, To: candles[i-1].OpenTime, Bars: i - start})
			}
			cur = k
			start = i
		}
	}
	if cur != "" && len(candles) > start {
		out = append(out, Segment{Kind: cur, From: candles[start].OpenTime, To: candles[len(candles)-1].OpenTime, Bars: len(candles) - start})
	}
	return out
}
