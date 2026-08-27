package indicators

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSMA(t *testing.T) {
	out := SMA([]float64{1, 2, 3, 4, 5}, 3)
	if !math.IsNaN(out[1]) || !approx(out[2], 2) || !approx(out[4], 4) {
		t.Fatalf("SMA 错误: %v", out)
	}
}

func TestEMASeedWithSMA(t *testing.T) {
	out := EMA([]float64{1, 2, 3, 4, 5}, 3)
	// seed = SMA(1,2,3) = 2; ema4 = 4*(0.5) + 2*0.5 = 3
	if !approx(out[2], 2) || !approx(out[3], 3) {
		t.Fatalf("EMA 错误: %v", out)
	}
}

func TestATRConstantRange(t *testing.T) {
	// 每根 H-L=2，无跳空 → ATR = 2
	h := []float64{12, 12, 12, 12, 12}
	l := []float64{10, 10, 10, 10, 10}
	c := []float64{11, 11, 11, 11, 11}
	out := ATR(h, l, c, 3)
	for i, v := range out {
		if i >= 2 && !approx(v, 2) {
			t.Fatalf("ATR 应恒为 2，i=%d v=%v", i, v)
		}
	}
}

func TestDonchianExcludesCurrent(t *testing.T) {
	h := []float64{10, 10, 10, 20}
	l := []float64{9, 9, 9, 1}
	upper, lower := Donchian(h, l, 3)
	if !math.IsNaN(upper[2]) {
		t.Fatal("数据不足应为 NaN")
	}
	// i=3：用前 3 根（不含当前），上轨 10 下轨 9 —— 当前 20/1 不参与，防自触发
	if upper[3] != 10 || lower[3] != 9 {
		t.Fatalf("唐奇安不应包含当前根: upper=%v lower=%v", upper[3], lower[3])
	}
}

func TestRealizedVol(t *testing.T) {
	// 恒定收益序列波动率应为 0
	out := RealizedVol([]float64{0.01, 0.01, 0.01, 0.01}, 3, 365)
	if out[2] != 0 {
		t.Fatalf("恒定收益波动率应为 0: %v", out[2])
	}
}

func TestCrossover(t *testing.T) {
	a := []float64{1, 1, 2}
	b := []float64{1, 1.5, 1.8}
	if !Crossover(a, b, 2) {
		t.Fatal("i=2 应判定上穿")
	}
	if Crossover(a, b, 1) {
		t.Fatal("i=1 未上穿")
	}
}
