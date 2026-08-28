package regime

import (
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func c(ot int64, px float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: px, High: px * 1.01, Low: px * 0.99, Close: px, Confirmed: true}
}

// trendSeries 单边趋势（每根 +1%）。
func trendSeries(n int) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, c(int64(i+1), 100*math.Pow(1.01, float64(i))))
	}
	return out
}

// rangeSeries 锯齿震荡。
func rangeSeries(n int) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		px := 100.0
		if i%2 == 1 {
			px = 101
		}
		out = append(out, c(int64(i+1), px))
	}
	return out
}

func TestTrendDetectedWithDebounce(t *testing.T) {
	d := NewDetector(10, 3)
	cs := trendSeries(20)
	var last Reading
	for i := range cs {
		last = d.Update(cs[:i+1])
	}
	if last.Kind != Trending {
		t.Fatalf("单边趋势应识别为 trending: %s", last.Describe())
	}
	if last.ER < 0.9 {
		t.Fatalf("趋势 ER 应接近 1: %v", last.ER)
	}
}

func TestRangeDetected(t *testing.T) {
	d := NewDetector(10, 3)
	cs := rangeSeries(20)
	var last Reading
	for i := range cs {
		last = d.Update(cs[:i+1])
	}
	if last.Kind != Range {
		t.Fatalf("锯齿震荡应识别为 range: %s", last.Describe())
	}
}

func TestInsufficientDataKeepsMixed(t *testing.T) {
	d := NewDetector(10, 3)
	r := d.Update(trendSeries(5)) // 不足 lookback
	if r.Kind != Mixed || r.Candles != 5 {
		t.Fatalf("数据不足应维持 mixed: %+v", r)
	}
	if r.Describe() == "" {
		t.Fatal("摘要不应为空")
	}
}

func TestDebounceBlocksSingleFlip(t *testing.T) {
	d := NewDetector(10, 3)
	// 先进入震荡
	cs := rangeSeries(15)
	for i := range cs {
		d.Update(cs[:i+1])
	}
	if d.Current() != Range {
		t.Fatalf("前置失败: %s", d.Current())
	}
	// 单根趋势读数不足以翻转（防抖）
	d.Update(append(append([]exchange.Candle{}, cs...), c(16, 200), c(17, 202)))
	if d.Current() != Range {
		t.Fatalf("单次跳变不应立即切换: %s", d.Current())
	}
}

func TestMixedBandNoFlip(t *testing.T) {
	d := NewDetector(10, 2)
	// ER 落在 (0.20, 0.35) 过渡带：即使连续也不切换出 mixed 之外的确定态之外
	// 构造：温和趋势 + 噪声，ER ≈ 0.27
	var cs []exchange.Candle
	px := 100.0
	for i := 0; i < 20; i++ {
		px += 0.27
		if i%3 == 0 {
			px -= 0.18 // 回撤制造路径
		}
		cs = append(cs, c(int64(i+1), px))
	}
	r := d.Update(cs)
	if r.Raw != Mixed && r.ER > DefaultThresholdRange && r.ER < DefaultThresholdTrend {
		t.Fatalf("过渡带分类错误: %+v", r)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	d := NewDetector(10, 3)
	cs := rangeSeries(30)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 10; i <= 20; i++ {
				d.Update(cs[:i])
				_ = d.Current()
			}
		}(g)
	}
	wg.Wait()
}

func TestDescribeFormats(t *testing.T) {
	r := Reading{Kind: Trending, Raw: Mixed, ER: 0.4, Lookback: 24, Confirm: 3, Candles: 100}
	s := r.Describe()
	if s == "" || !contains(s, "trending") {
		t.Fatalf("摘要格式错误: %s", s)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestSegments(t *testing.T) {
	// 前 20 根锯齿 → 震荡段；后 20 根趋势 → 趋势段
	cs := append(rangeSeries(20), trendSeries(20)...)
	for i := 20; i < len(cs); i++ { // 时间戳接续
		cs[i].OpenTime = int64(i + 1)
	}
	segs := Segments(cs, 10, 3)
	if len(segs) < 2 {
		t.Fatalf("应至少切出 2 段: %+v", segs)
	}
	sawRange, sawTrend := false, false
	for _, s := range segs {
		if s.Kind == Range {
			sawRange = true
		}
		if s.Kind == Trending {
			sawTrend = true
		}
		if s.Bars <= 0 || s.To < s.From {
			t.Fatalf("区间非法: %+v", s)
		}
	}
	if !sawRange || !sawTrend {
		t.Fatalf("应同时识别震荡段与趋势段: %+v", segs)
	}
	// 段无缝覆盖
	total := 0
	for _, s := range segs {
		total += s.Bars
	}
	if total > len(cs) {
		t.Fatalf("段根数不得超总数: %d > %d", total, len(cs))
	}
}
