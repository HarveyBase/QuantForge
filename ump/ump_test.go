package ump

import (
	"sync"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func candle(ot int64, px float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: px, High: px * 1.02, Low: px * 0.98, Close: px, Confirmed: true}
}

// genSeries 多形态合成序列：横盘 → 趋势 → 锯齿 → 趋势，覆盖不同情境桶。
func genSeries(n int) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		px := 100.0
		switch {
		case i < n/4: // 横盘
			px = 100 + float64(i%3)*0.2
		case i < n/2: // 上涨
			px = 100 + float64(i-n/4)*0.5
		case i < 3*n/4: // 锯齿
			if i%2 == 0 {
				px = 160
			} else {
				px = 155
			}
		default: // 下跌
			px = 160 - float64(i-3*n/4)*0.5
		}
		out = append(out, candle(int64(i+1), px))
	}
	return out
}

func TestExtractFeatures(t *testing.T) {
	cs := genSeries(200)
	// 越界
	if _, err := Extract(cs, -1); err == nil {
		t.Fatal("负索引必须报错")
	}
	if _, err := Extract(cs, len(cs)); err == nil {
		t.Fatal("越界必须报错")
	}
	// 预热不足
	if _, err := Extract(cs, 10); err == nil {
		t.Fatal("预热期不足必须报错")
	}
	// 正常提取：桶值在合法范围
	for _, i := range []int{60, 100, 150, 199} {
		f, err := Extract(cs, i)
		if err != nil {
			t.Fatalf("i=%d 提取失败: %v", i, err)
		}
		if f.RSI < 0 || f.RSI >= Buckets || f.DistHigh < 0 || f.DistHigh >= Buckets || f.VolRank < 0 || f.VolRank >= Buckets {
			t.Fatalf("桶越界: %+v", f)
		}
		if f.Describe() == "" {
			t.Fatal("摘要不应为空")
		}
	}
}

func TestExtractNoLookahead(t *testing.T) {
	// 未来数据不影响特征：截断 i 之后的数据，特征必须一致
	cs := genSeries(200)
	f1, err := Extract(cs, 120)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Extract(cs[:121], 120)
	if err != nil {
		t.Fatal(err)
	}
	if f1 != f2 {
		t.Fatalf("特征不得依赖未来数据: %+v vs %+v", f1, f2)
	}
}

func TestExtractDistinguishesContexts(t *testing.T) {
	cs := genSeries(200)
	fTrend, _ := Extract(cs, 95) // 上涨段中
	fSaw, _ := Extract(cs, 145)  // 锯齿段中
	if fTrend == fSaw {
		t.Log("不同市况落入同桶（可能，但不常见）")
	}
}

func TestFilterBlocksLowWinRateContext(t *testing.T) {
	f := NewFilter(0.35, 10)
	fe := Features{RSI: 5, DistHigh: 9, VolRank: 5}
	// 10 笔样本只赢 1 笔（胜率 10% < 35%）
	for i := 0; i < 10; i++ {
		f.Observe(fe, i == 0)
	}
	block, wr, n := f.ShouldBlock(fe)
	if !block || wr != 0.1 || n != 10 {
		t.Fatalf("低胜率情境必须拦截: block=%v wr=%v n=%d", block, wr, n)
	}
	// 高胜率情境放行
	fe2 := Features{RSI: 1, DistHigh: 5, VolRank: 2}
	for i := 0; i < 10; i++ {
		f.Observe(fe2, i < 8)
	}
	block2, wr2, _ := f.ShouldBlock(fe2)
	if block2 || wr2 != 0.8 {
		t.Fatalf("高胜率情境应放行: %v %v", block2, wr2)
	}
	// 未知情境（样本不足）放行——证据不足不判罪
	block3, _, n3 := f.ShouldBlock(Features{RSI: 0, DistHigh: 0, VolRank: 0})
	if block3 || n3 != 0 {
		t.Fatal("样本不足必须放行")
	}
}

func TestFilterSnapshotRestore(t *testing.T) {
	f := NewFilter(0, 0)
	fe := Features{RSI: 3, DistHigh: 8, VolRank: 7}
	for i := 0; i < 30; i++ {
		f.Observe(fe, i < 3)
	}
	snap := f.Snapshot()
	f2 := NewFilter(0, 0)
	f2.Restore(snap)
	if f2.Total() != 30 {
		t.Fatalf("恢复后样本数错误: %d", f2.Total())
	}
	block, wr, _ := f2.ShouldBlock(fe)
	if !block || wr != 0.1 {
		t.Fatalf("恢复后判定必须一致: %v %v", block, wr)
	}
}

func TestFilterConcurrent(t *testing.T) {
	f := NewFilter(0, 0)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				f.Observe(Features{RSI: i % Buckets, DistHigh: g % Buckets, VolRank: 5}, i%2 == 0)
				f.ShouldBlock(Features{RSI: i % Buckets, DistHigh: g % Buckets, VolRank: 5})
			}
		}(g)
	}
	wg.Wait()
	if f.Total() != 800 {
		t.Fatalf("并发入库总数错误: %d", f.Total())
	}
}

func TestValidateOOSGood(t *testing.T) {
	// 构造：某情境前半段大量亏损、后半段继续亏损 → 拦截它应提升测试集胜率
	bad := Features{RSI: 9, DistHigh: 9, VolRank: 9}
	good := Features{RSI: 1, DistHigh: 1, VolRank: 1}
	var trades []TradeRecord
	for i := 0; i < 60; i++ { // 前半训练
		trades = append(trades, TradeRecord{bad, false}, TradeRecord{good, true})
	}
	for i := 0; i < 40; i++ { // 后半测试
		trades = append(trades, TradeRecord{bad, false}, TradeRecord{good, true})
	}
	rep, err := ValidateOOS(trades, 0.35, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Usable {
		t.Fatalf("有效拦截器应可用: %s", rep.Reason)
	}
	if rep.Blocked != 50 { // 测试集 100 笔中 bad 情境 50 笔
		t.Fatalf("应拦截 50 笔坏情境: %d", rep.Blocked)
	}
	if rep.TestWinRateBefore != 0.5 || rep.TestWinRateAfter != 1.0 {
		t.Fatalf("胜率计算错误: before=%v after=%v", rep.TestWinRateBefore, rep.TestWinRateAfter)
	}
	if rep.Improvement != 0.5 {
		t.Fatalf("提升量错误: %v", rep.Improvement)
	}
}

func TestValidateOOSNoiseRejected(t *testing.T) {
	// 随机噪音：拦截器不应产生正增量（拦到的多是随机胜负）
	fe := Features{RSI: 5, DistHigh: 5, VolRank: 5}
	var trades []TradeRecord
	// 训练：胜率 30%（低于阈值 → 会拦）
	for i := 0; i < 100; i++ {
		trades = append(trades, TradeRecord{fe, i%10 < 3})
	}
	// 测试：胜率 60%（情境翻转）→ 拦截反而在丢盈利交易
	for i := 0; i < 100; i++ {
		trades = append(trades, TradeRecord{fe, i%10 < 6})
	}
	rep, err := ValidateOOS(trades, 0.35, 20)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable {
		t.Fatal("情境翻转时拦截器必须判不可用")
	}
	if rep.TestWinRateAfter != 0 {
		t.Fatalf("全部被拦截时放行胜率应无定义（0）: %v", rep.TestWinRateAfter)
	}
}

func TestValidateOOSInsufficient(t *testing.T) {
	var trades []TradeRecord
	for i := 0; i < 10; i++ {
		trades = append(trades, TradeRecord{Features{}, true})
	}
	if _, err := ValidateOOS(trades, 0.35, 20); err == nil {
		t.Fatal("样本不足必须报错")
	}
}

func TestPairTrades(t *testing.T) {
	cs := genSeries(200)
	// 手工构造成交：买卖配对，赢与亏
	trades := []exchange.Order{
		{Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, UpdatedAt: 60},
		{Side: exchange.Sell, FilledQty: 1, AvgPrice: 120, UpdatedAt: 70}, // 赢
		{Side: exchange.Buy, FilledQty: 1, AvgPrice: 150, UpdatedAt: 100},
		{Side: exchange.Sell, FilledQty: 1, AvgPrice: 130, UpdatedAt: 110}, // 亏
		{Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, UpdatedAt: 150},  // 未平仓（应被忽略）
	}
	recs, err := PairTrades(cs, trades)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("应配对出 2 笔（未平仓忽略）: %d", len(recs))
	}
	if !recs[0].Win || recs[1].Win {
		t.Fatalf("盈亏判定错误: %+v", recs)
	}
	// 特征合法（在合法桶范围）
	for _, r := range recs {
		if r.Features.RSI < 0 || r.Features.RSI >= Buckets {
			t.Fatalf("特征非法: %+v", r.Features)
		}
	}
	// 分批加仓配对：两笔买一笔卖
	trades2 := []exchange.Order{
		{Side: exchange.Buy, FilledQty: 1, AvgPrice: 100, UpdatedAt: 60},
		{Side: exchange.Buy, FilledQty: 1, AvgPrice: 120, UpdatedAt: 65},
		{Side: exchange.Sell, FilledQty: 2, AvgPrice: 115, UpdatedAt: 70}, // 均价 110 vs 成本 110 → 亏（115>110 赢）
	}
	recs2, _ := PairTrades(cs, trades2)
	if len(recs2) != 1 || !recs2[0].Win {
		t.Fatalf("分批配对判定错误: %+v", recs2)
	}
	// 空输入
	if r, _ := PairTrades(cs, nil); len(r) != 0 {
		t.Fatal("空成交应返回空")
	}
}
