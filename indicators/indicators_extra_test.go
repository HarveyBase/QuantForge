package indicators

import (
	"math"
	"testing"
)

func TestSMAEmpty(t *testing.T) {
	if out := SMA(nil, 3); len(out) != 0 {
		t.Fatal("空输入应返回空")
	}
}

func TestSMAPeriodOne(t *testing.T) {
	out := SMA([]float64{5, 6, 7}, 1)
	for i, v := range out {
		if v != float64(5+i) {
			t.Fatalf("period=1 应原样返回: %v", out)
		}
	}
}

func TestSMAPeriodExceedsLength(t *testing.T) {
	out := SMA([]float64{1, 2}, 5)
	if !math.IsNaN(out[0]) || !math.IsNaN(out[1]) {
		t.Fatalf("period 超长应全 NaN: %v", out)
	}
}

func TestEMAEmpty(t *testing.T) {
	if out := EMA(nil, 3); len(out) != 0 {
		t.Fatal("空输入应返回空")
	}
}

func TestEMAPeriodExceedsLength(t *testing.T) {
	out := EMA([]float64{1, 2}, 10)
	if !math.IsNaN(out[0]) || !math.IsNaN(out[1]) {
		t.Fatalf("period 超长应全 NaN: %v", out)
	}
}

func TestEMAKnownSequence(t *testing.T) {
	// 数据 2,4,6,8 period=2：seed=3, k=2/3 → ema[2]=6*(2/3)+3*(1/3)=5, ema[3]=8*(2/3)+5*(1/3)=7
	out := EMA([]float64{2, 4, 6, 8}, 2)
	if !approx(out[1], 3) || !approx(out[2], 5) || !approx(out[3], 7) {
		t.Fatalf("EMA 数值错误: %v", out)
	}
}

func TestATRWithGaps(t *testing.T) {
	// TR 取 H-L 与前收盘跳空的最大值
	h := []float64{12, 20, 12}
	l := []float64{10, 10, 10}
	c := []float64{11, 19, 11}
	out := ATR(h, l, c, 2)
	// trs: [2, max(10, 9, 0)=10, max(2, 7, 9)=9] → atr[1]=6, atr[2]=(6+9)/2=7.5
	if !approx(out[1], 6) || !approx(out[2], 7.5) {
		t.Fatalf("ATR 跳空处理错误: %v", out)
	}
}

func TestATREmpty(t *testing.T) {
	if out := ATR(nil, nil, nil, 3); len(out) != 0 {
		t.Fatal("空输入应返回空")
	}
}

func TestATRPeriodOne(t *testing.T) {
	h := []float64{11, 12, 13}
	l := []float64{9, 10, 11}
	c := []float64{10, 11, 12}
	out := ATR(h, l, c, 1)
	for i, v := range out {
		if !approx(v, h[i]-l[i]) {
			t.Fatalf("period=1 ATR 应等于 TR: %v", out)
		}
	}
}

func TestDonchianEmpty(t *testing.T) {
	u, l := Donchian(nil, nil, 3)
	if len(u) != 0 || len(l) != 0 {
		t.Fatal("空输入应返回空")
	}
}

func TestDonchianPeriodOne(t *testing.T) {
	u, l := Donchian([]float64{5, 3, 8}, []float64{4, 2, 7}, 1)
	// i>=1 用前一根
	if u[1] != 5 || l[1] != 4 || u[2] != 3 || l[2] != 2 {
		t.Fatalf("period=1 唐奇安错误: %v %v", u, l)
	}
}

func TestRealizedVolPeriodExceeds(t *testing.T) {
	out := RealizedVol([]float64{0.01}, 5, 365)
	if !math.IsNaN(out[0]) {
		t.Fatal("窗口不足应为 NaN")
	}
}

func TestRealizedVolScale(t *testing.T) {
	// 交替收益 ±0.01：样本方差（n-1）sd = 0.01×√(4/3)，年化 ×√365
	out := RealizedVol([]float64{0.01, -0.01, 0.01, -0.01}, 4, 365)
	if !approx(out[3], 0.01*math.Sqrt(4.0/3.0)*math.Sqrt(365)) {
		t.Fatalf("年化波动率错误: %v", out[3])
	}
}

func TestCrossoverBoundaries(t *testing.T) {
	a := []float64{1, 2}
	b := []float64{1, 2}
	if Crossover(a, b, 0) {
		t.Fatal("i=0 应拒绝")
	}
	if Crossover(a, b, 2) {
		t.Fatal("i 越界应拒绝")
	}
	if Crossover(a, []float64{1}, 1) {
		t.Fatal("b 长度不足应拒绝")
	}
}

func TestCrossoverNaN(t *testing.T) {
	a := []float64{math.NaN(), 2}
	b := []float64{1, 1.5}
	if Crossover(a, b, 1) {
		t.Fatal("NaN 输入应拒绝")
	}
}

func TestCrossoverEqualNotCross(t *testing.T) {
	// a[i-1] <= b[i-1] 且 a[i] > b[i] 才算上穿；相等不算
	a := []float64{1, 2}
	b := []float64{2, 2}
	if Crossover(a, b, 1) {
		t.Fatal("未超越不应判定上穿")
	}
}
