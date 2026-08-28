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

// EfficiencyRatio Kaufman 效率比（ER）= |净位移| / 路径总长，衡量趋势干净度。
// 趋势市：价格近似直行，ER → 1；震荡市：来回拉锯，ER → 0。
// 与 ATR 配合可做市况识别（regime）：ER 高开趋势策略、ER 低开网格。
func EfficiencyRatio(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	for i := range closes {
		if i < period {
			out[i] = math.NaN()
			continue
		}
		net := math.Abs(closes[i] - closes[i-period])
		path := 0.0
		for j := i - period + 1; j <= i; j++ {
			path += math.Abs(closes[j] - closes[j-1])
		}
		if path <= 0 {
			out[i] = 0
			continue
		}
		out[i] = net / path
	}
	return out
}

// RSI 相对强弱指数（Wilder 平滑），前 period 个为 NaN。
// >70 超买、<30 超卖（经典阈值；仅用截至当前数据，防前视由调用方保证）。
func RSI(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	if len(closes) <= period {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	var avgGain, avgLoss float64
	for i := 1; i <= period; i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			avgGain += d
		} else {
			avgLoss -= d
		}
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)
	out[period] = rsiFrom(avgGain, avgLoss)
	for i := 0; i < period; i++ {
		out[i] = math.NaN()
	}
	for i := period + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else {
			l = -d
		}
		avgGain = (avgGain*float64(period-1) + g) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + l) / float64(period)
		out[i] = rsiFrom(avgGain, avgLoss)
	}
	return out
}

func rsiFrom(avgGain, avgLoss float64) float64 {
	switch {
	case avgLoss == 0 && avgGain == 0:
		return 50
	case avgLoss == 0:
		return 100
	default:
		return 100 - 100/(1+avgGain/avgLoss)
	}
}
