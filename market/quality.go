// Package market 行情获取、K 线质量校验与快照存储。
// 数据纪律（docs/02）：唯一键去重、缺口切断窗口、未收盘 K 线只展示不入回测、快照双写留痕。
package market

import (
	"fmt"
	"sort"

	"github.com/HarveyBase/QuantForge/exchange"
)

// Validate 按三层放行矩阵校验并清洗 K 线序列，返回可直接用于指标/回测的序列。
// 规则：
//  1. 未收盘（Confirmed=false）剔除；
//  2. 唯一键 exchange|symbol|interval|openTime 完全重复去重留一条；
//  3. 同键不同 close 视为冲突，返回错误（上游数据源有问题，拒绝静默修复）；
//  4. 时间乱序排序；
//  5. 中间缺桶在窗口中间切断：只保留最长连续段（缺口处无法安全计算指标）；
//  6. OHLC 非法（≤0 或 High<Low 等）返回错误。
func Validate(candles []exchange.Candle, intervalMs int64) ([]exchange.Candle, error) {
	seen := make(map[string]exchange.Candle, len(candles))
	var uniq []exchange.Candle
	for _, c := range candles {
		if err := sanity(c); err != nil {
			return nil, fmt.Errorf("market: K 线数据非法 %s: %w", c.Key(), err)
		}
		key := c.Key()
		if prev, dup := seen[key]; dup {
			if prev.Close != c.Close {
				return nil, fmt.Errorf("market: 同键不同值冲突 %s: close %v vs %v（拒绝静默修复）", key, prev.Close, c.Close)
			}
			continue // 完全重复，去重留一条
		}
		seen[key] = c
		uniq = append(uniq, c)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].OpenTime < uniq[j].OpenTime })

	// 剔除未收盘 K 线（只可展示，禁入指标与回测）
	confirmed := make([]exchange.Candle, 0, len(uniq))
	for _, c := range uniq {
		if c.Confirmed {
			confirmed = append(confirmed, c)
		}
	}
	// 中间缺桶切断：只保留最长连续段（缺口处无法安全计算指标）
	longest := longestRun(confirmed, intervalMs)
	if len(longest) == 0 {
		return nil, fmt.Errorf("market: 无可用 K 线")
	}
	return longest, nil
}

func longestRun(cs []exchange.Candle, intervalMs int64) []exchange.Candle {
	if len(cs) == 0 {
		return nil
	}
	bestStart, bestLen := 0, 1
	curStart, curLen := 0, 1
	for i := 1; i < len(cs); i++ {
		if cs[i].OpenTime-cs[i-1].OpenTime == intervalMs {
			curLen++
		} else {
			curStart, curLen = i, 1
		}
		if curLen > bestLen {
			bestStart, bestLen = curStart, curLen
		}
	}
	return cs[bestStart : bestStart+bestLen]
}

func sanity(c exchange.Candle) error {
	if c.Open <= 0 || c.High <= 0 || c.Low <= 0 || c.Close <= 0 {
		return fmt.Errorf("价格非正 O=%v H=%v L=%v C=%v", c.Open, c.High, c.Low, c.Close)
	}
	if c.High < c.Low || c.High < c.Open || c.High < c.Close || c.Low > c.Open || c.Low > c.Close {
		return fmt.Errorf("OHLC 关系非法 H=%v L=%v O=%v C=%v", c.High, c.Low, c.Open, c.Close)
	}
	if c.OpenTime <= 0 {
		return fmt.Errorf("时间戳非法 %d", c.OpenTime)
	}
	return nil
}

// IntervalMs 周期字符串 → 毫秒。
func IntervalMs(interval string) int64 {
	if len(interval) < 2 {
		return 0
	}
	unit := interval[len(interval)-1]
	n := int64(0)
	for _, ch := range interval[:len(interval)-1] {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	switch unit {
	case 'm':
		return n * 60_000
	case 'H':
		return n * 3_600_000
	case 'D':
		return n * 86_400_000
	default:
		return 0
	}
}
