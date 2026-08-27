// Package indicators 技术指标：纯函数、无状态，只接受"截至当前时点"的序列（防前视由调用方保证）。
package indicators

import "math"

// SMA 简单移动平均，返回与输入等长的序列，前 period-1 个为 NaN。
func SMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	sum := 0.0
	for i, v := range values {
		sum += v
		if i >= period {
			sum -= values[i-period]
		}
		if i >= period-1 {
			out[i] = sum / float64(period)
		} else {
			out[i] = math.NaN()
		}
	}
	return out
}

// EMA 指数移动平均，seed 用前 period 个的 SMA。
func EMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if len(values) == 0 {
		return out
	}
	k := 2.0 / (float64(period) + 1)
	var ema float64
	for i, v := range values {
		if i < period-1 {
			out[i] = math.NaN()
			continue
		}
		if i == period-1 {
			sum := 0.0
			for _, x := range values[:period] {
				sum += x
			}
			ema = sum / float64(period)
		} else {
			ema = v*k + ema*(1-k)
		}
		out[i] = ema
	}
	return out
}

// ATR 平均真实波幅（Wilder 平滑）。highs/lows/closes 等长。
func ATR(highs, lows, closes []float64, period int) []float64 {
	n := len(closes)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	trs := make([]float64, n)
	trs[0] = highs[0] - lows[0]
	for i := 1; i < n; i++ {
		trs[i] = math.Max(highs[i]-lows[i], math.Max(math.Abs(highs[i]-closes[i-1]), math.Abs(lows[i]-closes[i-1])))
	}
	var atr float64
	for i, tr := range trs {
		if i < period-1 {
			out[i] = math.NaN()
			continue
		}
		if i == period-1 {
			sum := 0.0
			for _, x := range trs[:period] {
				sum += x
			}
			atr = sum / float64(period)
		} else {
			atr = (atr*float64(period-1) + tr) / float64(period)
		}
		out[i] = atr
	}
	return out
}

// Donchian 唐奇安通道：上轨 = 前 period 根（不含当前）最高价，下轨 = 最低价。
// 海龟法则口径：突破上轨买入、跌破下轨卖出，当前根不参与自身轨道计算（防自触发）。
func Donchian(highs, lows []float64, period int) (upper, lower []float64) {
	n := len(highs)
	upper, lower = make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		if i < period {
			upper[i], lower[i] = math.NaN(), math.NaN()
			continue
		}
		hi, lo := math.Inf(-1), math.Inf(1)
		for _, h := range highs[i-period : i] {
			hi = math.Max(hi, h)
		}
		for _, l := range lows[i-period : i] {
			lo = math.Min(lo, l)
		}
		upper[i], lower[i] = hi, lo
	}
	return upper, lower
}

// RealizedVol 已实现波动率（年化）：returns 为逐期收益率，period 为采样窗口。
func RealizedVol(returns []float64, period int, periodsPerYear float64) []float64 {
	out := make([]float64, len(returns))
	for i := range returns {
		if i < period-1 {
			out[i] = math.NaN()
			continue
		}
		w := returns[i-period+1 : i+1]
		mean := 0.0
		for _, r := range w {
			mean += r
		}
		mean /= float64(len(w))
		var ss float64
		for _, r := range w {
			ss += (r - mean) * (r - mean)
		}
		out[i] = math.Sqrt(ss/float64(len(w)-1)) * math.Sqrt(periodsPerYear)
	}
	return out
}

// Crossover 上穿：a[i-1] <= b[i-1] 且 a[i] > b[i]。
func Crossover(a, b []float64, i int) bool {
	if i <= 0 || i >= len(a) || i >= len(b) {
		return false
	}
	if math.IsNaN(a[i]) || math.IsNaN(b[i]) || math.IsNaN(a[i-1]) || math.IsNaN(b[i-1]) {
		return false
	}
	return a[i-1] <= b[i-1] && a[i] > b[i]
}
