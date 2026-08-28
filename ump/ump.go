// Package ump 统计版失败交易拦截器（借鉴 abu UMP 思路，不用机器学习）：
// 信号落地前提取情境特征（RSI 桶 / 距前高距离桶 / 波动率分位桶），
// 查历史相似情境的交易胜率——「样本充足且胜率低于阈值」才拦截。
// 纪律（docs/01）：证据不足不判罪（样本 < minSamples 放行）；
// 拦截器本身必须过样本外验证（ValidateOOS），否则它只是拟合了历史噪音。
package ump

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/indicators"
)

// 特征参数默认值。
const (
	DefaultRSIPeriod    = 14
	DefaultHighLookback = 20 // 距前高：前 20 根最高价
	DefaultVolLookback  = 50 // 波动率分位：过去 50 根 ATR% 分布
	Buckets             = 10 // 每维离散桶数
	// 拦截默认阈值：胜率 < 35% 且样本 ≥ 20 才拦
	DefaultMinWinRate = 0.35
	DefaultMinSamples = 20
)

// Features 信号时点情境特征（各维离散到 [0, Buckets)）。
type Features struct {
	RSI      int `json:"rsi_bucket"`       // RSI/10
	DistHigh int `json:"dist_high_bucket"` // (距前高%+100)/10，距前高∈[-100%,0]
	VolRank  int `json:"vol_rank_bucket"`  // 当前 ATR% 在历史分布中的分位
}

// Key 统计键。
func (f Features) Key() [3]int { return [3]int{f.RSI, f.DistHigh, f.VolRank} }

// Describe 人读摘要。
func (f Features) Describe() string {
	return fmt.Sprintf("rsi=%d0±, 距前高=%d0±%%, 波动分位=%d0±", f.RSI, f.DistHigh-10, f.VolRank)
}

// Extract 提取第 i 根（信号根）收盘时点的情境特征。
// 防前视：只使用 candles[:i+1]，距前高不含当前根。
func Extract(candles []exchange.Candle, i int) (Features, error) {
	n := len(candles)
	if i < 0 || i >= n {
		return Features{}, fmt.Errorf("ump: 索引越界 %d/%d", i, n)
	}
	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)
	for j, c := range candles {
		closes[j], highs[j], lows[j] = c.Close, c.High, c.Low
	}
	rsi := indicators.RSI(closes, DefaultRSIPeriod)[i]
	if math.IsNaN(rsi) {
		return Features{}, fmt.Errorf("ump: RSI 预热期不足（需要 %d 根）", DefaultRSIPeriod+1)
	}
	// 距前高：前 HighLookback 根最高（不含当前根）
	if i < DefaultHighLookback {
		return Features{}, fmt.Errorf("ump: 距前高窗口不足（需要 %d 根历史）", DefaultHighLookback)
	}
	priorHigh := math.Inf(-1)
	for _, h := range highs[i-DefaultHighLookback : i] {
		priorHigh = math.Max(priorHigh, h)
	}
	distHigh := closes[i]/priorHigh - 1 // ∈ (-1, 0]（突破时 ≈ 0）
	// ATR% 历史分布分位
	atrs := indicators.ATR(highs, lows, closes, 14)
	if i < DefaultVolLookback {
		return Features{}, fmt.Errorf("ump: 波动率窗口不足（需要 %d 根历史）", DefaultVolLookback)
	}
	var hist []float64
	for j := i - DefaultVolLookback + 1; j <= i; j++ {
		if !math.IsNaN(atrs[j]) && closes[j] > 0 {
			hist = append(hist, atrs[j]/closes[j])
		}
	}
	if len(hist) < 10 {
		return Features{}, fmt.Errorf("ump: 波动率样本不足")
	}
	cur := atrs[i] / closes[i]
	sort.Float64s(hist)
	rank := sort.SearchFloat64s(hist, cur) // ≤cur 的数量
	f := Features{
		RSI:      bucket(rsi, 0, 100),
		DistHigh: bucket(distHigh*100+100, 0, 200), // 距前高% ∈ (-100,0] → 平移到 (0,100]
		VolRank:  bucket(float64(rank)/float64(len(hist))*100, 0, 100),
	}
	return f, nil
}

func bucket(v, lo, hi float64) int {
	b := int((v - lo) / (hi - lo) * Buckets)
	if b < 0 {
		b = 0
	}
	if b >= Buckets {
		b = Buckets - 1
	}
	return b
}

// TradeRecord 历史交易结果（供统计的原子样本）。
type TradeRecord struct {
	Features Features `json:"features"`
	Win      bool     `json:"win"`
}

type cell struct{ wins, total int }

// Filter 统计拦截器（线程安全；可序列化重建——见 Snapshot/Restore）。
type Filter struct {
	mu         sync.Mutex
	minWinRate float64
	minSamples int
	cells      map[[3]int]cell
}

// NewFilter 构造拦截器。minWinRate/minSamples ≤0 用默认。
func NewFilter(minWinRate float64, minSamples int) *Filter {
	if minWinRate <= 0 {
		minWinRate = DefaultMinWinRate
	}
	if minSamples <= 0 {
		minSamples = DefaultMinSamples
	}
	return &Filter{minWinRate: minWinRate, minSamples: minSamples, cells: map[[3]int]cell{}}
}

// Observe 交易结束后入库（平仓才计盈亏，持仓中不算）。
func (f *Filter) Observe(fe Features, win bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cells[fe.Key()]
	c.total++
	if win {
		c.wins++
	}
	f.cells[fe.Key()] = c
}

// ShouldBlock 判定是否拦截该信号。
// 返回：block、该情境历史胜率、样本数。证据不足（样本 < minSamples）不拦。
func (f *Filter) ShouldBlock(fe Features) (block bool, winRate float64, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cells[fe.Key()]
	if c.total < f.minSamples {
		return false, 0, c.total
	}
	wr := float64(c.wins) / float64(c.total)
	return wr < f.minWinRate, wr, c.total
}

// Snapshot 导出统计（持久化/审计）。
func (f *Filter) Snapshot() map[[3]int][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[[3]int][2]int, len(f.cells))
	for k, c := range f.cells {
		out[k] = [2]int{c.wins, c.total}
	}
	return out
}

// Restore 从快照重建（serve 重启恢复）。
func (f *Filter) Restore(snap map[[3]int][2]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cells = make(map[[3]int]cell, len(snap))
	for k, v := range snap {
		f.cells[k] = cell{wins: v[0], total: v[1]}
	}
}

// Total 累计样本数。
func (f *Filter) Total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cells {
		n += c.total
	}
	return n
}

// OOSReport 拦截器自身的样本外验证结果（时间对半：前半训练、后半测试）。
type OOSReport struct {
	TrainSamples, TestSamples int
	Blocked                   int     // 测试集中被拦截的笔数
	TestWinRateBefore         float64 // 测试集全量胜率
	TestWinRateAfter          float64 // 测试集放行（未被拦截）胜率
	Improvement               float64 // after − before
	Usable                    bool    // 拦截器是否可用（提升 > 0 且拦截数 > 0）
	Reason                    string
}

// ValidateOOS 时间对半验证拦截器：train 建统计，test 上模拟"启用拦截"的效果。
// trades 必须按时间升序。测试集放行交易胜率应高于全量胜率，拦截器才算提供增量；
// 否则它只是噪音，不得启用（docs/01：拦截规则也要过样本外）。
func ValidateOOS(trades []TradeRecord, minWinRate float64, minSamples int) (*OOSReport, error) {
	if len(trades) < 2*minSamples {
		return nil, fmt.Errorf("ump: 样本 %d 不足（至少 %d）", len(trades), 2*minSamples)
	}
	half := len(trades) / 2
	train, test := trades[:half], trades[half:]
	f := NewFilter(minWinRate, minSamples)
	for _, tr := range train {
		f.Observe(tr.Features, tr.Win)
	}
	rep := &OOSReport{TrainSamples: len(train), TestSamples: len(test)}
	winsBefore, winsAfter, after := 0, 0, 0
	for _, tr := range test {
		if tr.Win {
			winsBefore++
		}
		if block, _, _ := f.ShouldBlock(tr.Features); block {
			rep.Blocked++
			continue
		}
		after++
		if tr.Win {
			winsAfter++
		}
	}
	rep.TestWinRateBefore = float64(winsBefore) / float64(len(test))
	if after > 0 {
		rep.TestWinRateAfter = float64(winsAfter) / float64(after)
	}
	rep.Improvement = rep.TestWinRateAfter - rep.TestWinRateBefore
	switch {
	case rep.Blocked == 0:
		rep.Reason = fmt.Sprintf("测试集零拦截（训练样本情境未复现或胜率达标），拦截器无增量")
		rep.Usable = false
	case rep.Improvement <= 0:
		rep.Reason = fmt.Sprintf("拦截后胜率 %.1f%% 未高于拦截前 %.1f%%——拦截器拟合了历史噪音，不得启用",
			rep.TestWinRateAfter*100, rep.TestWinRateBefore*100)
	default:
		rep.Usable = true
		rep.Reason = fmt.Sprintf("拦截 %d 笔后测试集胜率 %.1f%% → %.1f%%（+%.1fpp）",
			rep.Blocked, rep.TestWinRateBefore*100, rep.TestWinRateAfter*100, rep.Improvement*100)
	}
	return rep, nil
}

// PairTrades 把回测成交序列（buy/sell 交错）配对成带情境的交易样本。
// 特征取自买入成交根的前一根（决策时点可见数据，防前视）；
// 盈亏口径：卖出均价 > 买入均价（简化口径，未含费）。
// 未平仓的尾部持仓被忽略（盈亏未实现，不得入库）。
func PairTrades(candles []exchange.Candle, trades []exchange.Order) ([]TradeRecord, error) {
	indexOf := map[int64]int{}
	for i, c := range candles {
		indexOf[c.OpenTime] = i
	}
	var out []TradeRecord
	var buyQty, buyCost float64 // 持仓移动成本
	var entryFeat Features      // 当前配对的入场特征（RSI<0 表示无效/预热不足）
	for _, tr := range trades {
		switch tr.Side {
		case exchange.Buy:
			if tr.FilledQty <= 0 {
				continue
			}
			if buyQty == 0 {
				// 新一笔交易：特征取成交根前一根（决策时点可见数据）
				entryFeat = Features{-1, -1, -1}
				if i, ok := indexOf[tr.UpdatedAt]; ok && i >= 1 {
					if fe, err := Extract(candles, i-1); err == nil {
						entryFeat = fe
					}
				}
			}
			buyQty += tr.FilledQty
			buyCost += tr.FilledQty * tr.AvgPrice
		case exchange.Sell:
			if tr.FilledQty <= 0 || buyQty <= 0 {
				continue
			}
			avgCost := buyCost / buyQty
			sellQty := math.Min(tr.FilledQty, buyQty)
			win := tr.AvgPrice > avgCost
			buyQty -= sellQty
			buyCost -= sellQty * avgCost
			if buyQty <= 1e-12 {
				buyQty, buyCost = 0, 0
			}
			if entryFeat.RSI >= 0 {
				out = append(out, TradeRecord{Features: entryFeat, Win: win})
			}
		}
	}
	return out, nil
}
