package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/exchange/okx"
	"github.com/HarveyBase/QuantForge/ump"
)

// 测试进程内对 http.DefaultTransport 关闭证书校验：
// 配置强制 HTTPS，mock 只能是自签 TLS server；okx 客户端 Transport 为 nil
// 时走 DefaultTransport，buildApp 内部的请求因此信任 mock（仅测试二进制内生效）。
func init() {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
}

func mockOKX(t *testing.T) *config.Config {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/candles", func(w http.ResponseWriter, r *http.Request) {
		limit := 300
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		if limit <= 0 || limit > 300 {
			limit = 300
		}
		var rows [][]string
		for i := limit; i >= 1; i-- { // OKX 最新在前
			ot := int64(3600000 * i)
			px := 100 + float64(i%7) // 震荡价
			rows = append(rows, []string{
				fmt.Sprintf("%d", ot), fmt.Sprintf("%g", px),
				fmt.Sprintf("%g", px+1), fmt.Sprintf("%g", px-1), fmt.Sprintf("%g", px),
				"1", "0", "0", "1",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": rows})
	})
	mux.HandleFunc("/api/v5/account/balance", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]any{{
			"details": []map[string]string{
				{"ccy": "USDT", "availBal": "10000", "cashBal": "10000", "frozenBal": "0"},
				{"ccy": "BTC", "availBal": "0.05", "cashBal": "0.05", "frozenBal": "0"},
			},
		}}})
	})
	var mu sync.Mutex
	placed := 0
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		placed++
		id := placed
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"ordId": fmt.Sprintf("o%d", id), "clOrdID": "", "sCode": "0", "sMsg": ""},
		}})
	})
	mux.HandleFunc("/api/v5/trade/orders-pending", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]any{}})
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []map[string]string{
			{"instId": "BTC-USDT", "baseCcy": "BTC", "quoteCcy": "USDT", "lotSz": "0.00000001", "minSz": "0.00001", "tickSz": "0.1"},
		}})
	})
	mux.HandleFunc("/api/v5/market/history-candles", func(w http.ResponseWriter, r *http.Request) {
		var rows [][]string
		for i := 300; i >= 1; i-- {
			ot := int64(3600000 * i)
			px := 100 + float64(i%7)
			rows = append(rows, []string{
				fmt.Sprintf("%d", ot), fmt.Sprintf("%g", px),
				fmt.Sprintf("%g", px+1), fmt.Sprintf("%g", px-1), fmt.Sprintf("%g", px),
				"1", "0", "0", "1",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": rows})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Exchange.RestURL = srv.URL
	cfg.DataDir = t.TempDir()
	return cfg
}

func TestBuildAppResearch(t *testing.T) {
	cfg := mockOKX(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a.ex.Name() != "okx" || a.exec != nil {
		t.Fatalf("research 模式应使用公开行情客户端且无执行器: %v %v", a.ex.Name(), a.exec)
	}
	if a.grid == nil || a.pol == nil || a.snap == nil {
		t.Fatal("核心组件未装配")
	}
}

func TestBuildAppRejectsInvalidConfig(t *testing.T) {
	cfg := mockOKX(t)
	cfg.Exchange.Name = "unknown"
	if _, err := buildApp(cfg); err == nil {
		t.Fatal("非法配置必须被拒绝")
	}
}

func TestBuildAppPaperSeedsBalances(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a.exec == nil {
		t.Fatal("paper 模式必须装配执行器")
	}
	cash, positions, _ := a.pf.Snapshot()
	if cash != 10000 {
		t.Fatalf("余额应从交易所同步: %v", cash)
	}
	found := false
	for _, p := range positions {
		if p.Symbol == "BTC-USDT" && p.Available == 0.05 {
			found = true
		}
	}
	if !found {
		t.Fatalf("持仓应从交易所同步: %+v", positions)
	}
	a.exec.Stop()
}

func TestOnCandlesSnapshotAndNoTradeInResearch(t *testing.T) {
	cfg := mockOKX(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	candles := genCandles(50)
	a.onCandles(candles)
	if len(a.candles) != 50 || a.lastCandle != candles[49].OpenTime {
		t.Fatalf("缓存与游标未更新: len=%d last=%d", len(a.candles), a.lastCandle)
	}
	// 快照留痕
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "snapshots", snapshotName(cfg), "latest.json")); err != nil {
		t.Fatalf("每次新收盘根应固化快照: %v", err)
	}
	// 重复驱动同根不重复处理
	a.onCandles(candles)
	if a.lastCandle != candles[49].OpenTime {
		t.Fatal("同根重复驱动必须幂等")
	}
}

func TestOnCandlesPaperSubmitsOrders(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	// 网格范围覆盖当前价，触发初始建仓买单
	candles := genCandles(50)
	price := candles[49].Close
	cfg.Strategy.Grid.Lower = price * 0.5
	cfg.Strategy.Grid.Upper = price * 1.5
	cfg.Strategy.Grid.Grids = 10
	cfg.Risk.MaxOrderNotionalUSD = 100000
	cfg.Risk.MaxDailyNotionalUSD = 1000000
	cfg.Risk.MaxPositionNotionalUSD = 1000000
	cfg.Risk.MaxOrdersPerMinute = 100
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	a.onCandles(candles)
	if len(a.exec.OpenOrders()) == 0 {
		t.Fatal("paper 模式初始建仓应产生挂单")
	}
	// cash 应被冻结
	cash, _, _ := a.pf.Snapshot()
	if cash >= 10000 {
		t.Fatalf("挂单应冻结资金: %v", cash)
	}
}

func TestLoadFixedSample(t *testing.T) {
	cfg := mockOKX(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sample := genCandles(40)
	b, _ := json.Marshal(sample)
	name := "btc_usdt_1h.json"
	os.MkdirAll(filepath.Join(cfg.DataDir, "samples"), 0o755)
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", name), b, 0o644)
	if err := a.loadFixedSample(); err != nil {
		t.Fatal(err)
	}
	if len(a.candles) != 40 {
		t.Fatalf("固定样本应加载: %d", len(a.candles))
	}
	// 损坏样本必须报错
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", name), []byte("xxx"), 0o644)
	a.candles = nil
	if err := a.loadFixedSample(); err == nil {
		t.Fatal("损坏样本必须报错")
	}
}

func TestRunBacktestUsesFixedSample(t *testing.T) {
	cfg := mockOKX(t)
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sample := genCandles(60)
	b, _ := json.Marshal(sample)
	os.MkdirAll(filepath.Join(cfg.DataDir, "samples"), 0o755)
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", "btc_usdt_1h.json"), b, 0o644)
	res, err := a.runBacktest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.NumTrials != 1 {
		t.Fatalf("试验计数必须累计: %d", res.NumTrials)
	}
	if res.Metrics.FinalEquity <= 0 || len(res.EquityCurve) < 30 {
		t.Fatalf("回测结果异常: %+v", res.Metrics)
	}
	res2, _ := a.runBacktest(context.Background())
	if res2.NumTrials != 2 {
		t.Fatalf("第二次回测试验数应为 2: %d", res2.NumTrials)
	}
	// 台账留痕
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "backtests", "ledger.jsonl")); err != nil {
		t.Fatalf("试验台账必须留痕: %v", err)
	}
}

func TestRunBacktestInsufficientData(t *testing.T) {
	cfg := mockOKX(t)
	// 换一个无样本无快照的目录 + 不提供行情的 URL
	cfg.DataDir = t.TempDir()
	badSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "1", "msg": "bad"})
	}))
	defer badSrv.Close()
	cfg.Exchange.RestURL = badSrv.URL
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.runBacktest(context.Background()); err == nil {
		t.Fatal("行情不足且所有数据层失败必须报错")
	}
}

func TestCmdBacktestRuns(t *testing.T) {
	cfg := mockOKX(t) // 行情拉取因自签证书失败 → 降级固定样本层，仍可回测
	sample := genCandles(60)
	b, _ := json.Marshal(sample)
	os.MkdirAll(filepath.Join(cfg.DataDir, "samples"), 0o755)
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", "btc_usdt_1h.json"), b, 0o644)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cb, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, cb, 0o644)
	fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
	fs.String("config", cfgPath, "")
	if err := cmdBacktest(fs, nil); err != nil {
		t.Fatalf("命令行回测应可运行: %v", err)
	}
}

func TestCmdBacktestConfigError(t *testing.T) {
	fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
	fs.String("config", filepath.Join(t.TempDir(), "nope.json"), "")
	if err := cmdBacktest(fs, nil); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("配置缺失必须报错: %v", err)
	}
}

func genCandles(n int) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	for i := 0; i < n; i++ {
		px := 100 + float64(i%7)
		out = append(out, exchange.Candle{
			Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
			OpenTime: int64(3600000 * (i + 1)),
			Open:     px, High: px + 1, Low: px - 1, Close: px,
			Volume: 1, Confirmed: true,
		})
	}
	return out
}

// 编译期确认 backtest.Result 仍在使用（保持导入）。
var _ = (*backtest.Result)(nil)
var _ = time.Second

func TestBuildAppPaperNoCredsFails(t *testing.T) {
	os.Unsetenv("OKX_API_KEY")
	os.Unsetenv("OKX_SECRET")
	os.Unsetenv("OKX_PASSPHRASE")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	if _, err := buildApp(cfg); err == nil {
		t.Fatal("paper 模式无凭据必须拒绝启动（不得静默跳过账户同步）")
	}
}

func TestRunBacktestFallsBackToSnapshot(t *testing.T) {
	cfg := mockOKX(t)
	// 实时层用坏服务（构建前替换 URL，令 a.ex 直连坏服务）
	badSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": "1", "msg": "bad"})
	}))
	defer badSrv.Close()
	cfg.Exchange.RestURL = badSrv.URL
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if oc, ok := a.ex.(*okx.Client); ok {
		oc.HTTP = badSrv.Client()
	}
	snapCandles := genCandles(40)
	if _, err := a.snap.Save(snapshotName(cfg), snapCandles); err != nil {
		t.Fatal(err)
	}
	res, err := a.runBacktest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.EquityCurve) != 40 {
		t.Fatalf("应使用快照层 40 根: %d", len(res.EquityCurve))
	}
}

func TestCmdWalkforwardRuns(t *testing.T) {
	cfg := mockOKX(t)
	// 固定样本 300 根：train 150 / test 50 → 3 折
	sample := genCandles(300)
	b, _ := json.Marshal(sample)
	os.MkdirAll(filepath.Join(cfg.DataDir, "samples"), 0o755)
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", "btc_usdt_1h.json"), b, 0o644)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cb, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, cb, 0o644)
	if err := cmdWalkforward([]string{"-config", cfgPath, "-train", "150", "-test", "50"}); err != nil {
		t.Fatalf("walkforward 子命令应可运行: %v", err)
	}
	// grid 策略也可验证
	if err := cmdWalkforward([]string{"-config", cfgPath, "-train", "150", "-test", "50", "-strategy", "grid"}); err != nil {
		t.Fatalf("grid walkforward 应可运行: %v", err)
	}
	// 未知策略报错
	if err := cmdWalkforward([]string{"-config", cfgPath, "-strategy", "yolo"}); err == nil {
		t.Fatal("未知策略必须报错")
	}
	// 样本不足报错
	if err := cmdWalkforward([]string{"-config", cfgPath, "-train", "250", "-test", "100"}); err == nil {
		t.Fatal("样本不足必须报错")
	}
}

func TestReviewLoopProducesRecords(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	candles := genCandles(50)
	price := candles[49].Close
	cfg.Strategy.Grid.Lower = price * 0.5
	cfg.Strategy.Grid.Upper = price * 1.5
	cfg.Strategy.Grid.Grids = 10
	cfg.Risk.MaxOrderNotionalUSD = 100000
	cfg.Risk.MaxDailyNotionalUSD = 1000000
	cfg.Risk.MaxPositionNotionalUSD = 1000000
	cfg.Risk.MaxOrdersPerMinute = 100
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if a.reviewer == nil {
		t.Fatal("serve 必须装配复盘器")
	}
	// 交易后复盘：首次建基准
	a.onCandles(candles)
	rec, err := a.reviewer.ReviewOnce()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Stage != "paper" || rec.Symbol != "BTC-USDT" {
		t.Fatalf("复盘记录字段错误: %+v", rec)
	}
	if len(rec.Fills) == 0 && rec.OpenOrders == 0 {
		t.Log("窗口内无成交（挂单在成交前）")
	}
	// 归档落盘
	dir := filepath.Join(cfg.DataDir, "reviews")
	ents, _ := os.ReadDir(dir)
	found := false
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != "ledger.jsonl" {
			found = true
		}
	}
	if !found {
		t.Fatal("复盘归档必须落盘")
	}
	if recs := a.reviewer.Recent(5); len(recs) != 1 {
		t.Fatalf("Recent 应能读回归档: %d", len(recs))
	}
}

func TestReviewingPausesTrading(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	candles := genCandles(50)
	price := candles[49].Close
	cfg.Strategy.Grid.Lower = price * 0.5
	cfg.Strategy.Grid.Upper = price * 1.5
	cfg.Strategy.Grid.Grids = 10
	cfg.Risk.MaxOrderNotionalUSD = 100000
	cfg.Risk.MaxDailyNotionalUSD = 1000000
	cfg.Risk.MaxPositionNotionalUSD = 1000000
	cfg.Risk.MaxOrdersPerMinute = 100
	a, _ := buildApp(cfg)
	defer a.exec.Stop()
	// 第一次驱动建仓
	a.onCandles(candles)
	nBefore := len(a.exec.OpenOrders())
	// 复盘静默期：新收盘信号不下单
	a.reviewing.Store(true)
	a.onCandles(candles[1:])
	a.reviewing.Store(false)
	if n := len(a.exec.OpenOrders()); n != nBefore {
		t.Fatalf("复盘期间不应新增订单: before=%d after=%d", nBefore, n)
	}
}

func TestCmdFetchCandles(t *testing.T) {
	cfg := mockOKX(t)
	// 返回 60 根历史（mock 的 candles 端点也服务 history-candles：直接补一个）
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cb, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, cb, 0o644)
	if err := cmdFetch([]string{"-config", cfgPath, "-bars", "60"}); err != nil {
		t.Fatalf("fetch 应可运行: %v", err)
	}
	// 校验固化产物可被 loadFixedSample 消费
	name := "btc_usdt_1h.json"
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "samples", name)); err != nil {
		t.Fatalf("固定样本应固化: %v", err)
	}
}

func TestStateRecoveredOnRestart(t *testing.T) {
	cfg := mockOKX(t)
	// 第一次启动：驱动一根收盘 + 触发 Kill，落盘 state
	a1, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a1.onCandles(genCandles(50))
	a1.rk.Kill.Trip("演练停机")
	a1.persistState()
	// 第二次启动（同 dataDir）：游标与 Kill 状态必须恢复
	a2, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a2.lastCandle == 0 {
		t.Fatal("重启后游标必须恢复（防重复驱动策略）")
	}
	if !a2.rk.Kill.Tripped() || a2.rk.Kill.Reason() != "演练停机" {
		t.Fatal("重启后 Kill Switch 状态必须恢复（停机不得因重启失效）")
	}
	// 恢复后同根驱动不重复处理
	candles := genCandles(50)
	a2.onCandles(candles)
	if a2.lastCandle != candles[49].OpenTime {
		t.Fatal("同根重复驱动必须幂等")
	}
}

func TestCmdUMPCheckRuns(t *testing.T) {
	cfg := mockOKX(t)
	// 造一份多轮"震荡+趋势"循环的 400 根样本，让 trend 产生多笔交易
	var sample []exchange.Candle
	px := 100.0
	for i := 0; i < 400; i++ {
		phase := (i / 10) % 4 // 10 根一段：震荡/上涨/震荡/下跌循环
		switch phase {
		case 0, 2: // 震荡
			px = px * (1 + 0.002*float64(i%5-2))
		case 1: // 上涨（单根 >1% 才能突破前根高点）
			px = px * 1.015
		case 3: // 下跌
			px = px * 0.99
		}
		sample = append(sample, exchange.Candle{
			Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
			OpenTime: int64(3600000 * (i + 1)),
			Open:     px, High: px * 1.01, Low: px * 0.99, Close: px,
			Volume: 1, Confirmed: true,
		})
	}
	b, _ := json.Marshal(sample)
	os.MkdirAll(filepath.Join(cfg.DataDir, "samples"), 0o755)
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", "btc_usdt_1h.json"), b, 0o644)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cb, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, cb, 0o644)
	if err := cmdUMPCheck([]string{"-config", cfgPath, "-min-samples", "3"}); err != nil {
		t.Fatalf("umpcheck 应可运行: %v", err)
	}
	// 样本不足时报错（换小样本）
	os.WriteFile(filepath.Join(cfg.DataDir, "samples", "btc_usdt_1h.json"), []byte("[]"), 0o644)
	if err := cmdUMPCheck([]string{"-config", cfgPath}); err == nil {
		t.Fatal("样本不足必须报错")
	}
}

func TestUMPServeBlocksLowWinRateBuy(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	candles := genCandles(60)
	price := candles[59].Close
	cfg.Strategy.Grid.Lower = price * 0.5
	cfg.Strategy.Grid.Upper = price * 1.5
	cfg.Strategy.Grid.Grids = 10
	cfg.Risk.MaxOrderNotionalUSD = 100000
	cfg.Risk.MaxDailyNotionalUSD = 1000000
	cfg.Risk.MaxPositionNotionalUSD = 1000000
	cfg.Risk.MaxOrdersPerMinute = 100
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if !a.umpOn || a.umpFilter == nil {
		t.Fatal("paper + ump.enabled 默认开启，应装配拦截器")
	}
	// 预置低胜率情境：把当前信号根的特征桶灌成低胜率
	fe, err := umpExtractFor(a, candles)
	if err != nil {
		t.Skipf("特征提取失败（预热不足）: %v", err)
	}
	for i := 0; i < ump.DefaultMinSamples; i++ {
		a.umpFilter.Observe(fe, false) // 全亏
	}
	// 驱动收盘：初始建仓买信号应被 UMP 拦截
	a.onCandles(candles)
	if n := a.umpBlocked.Load(); n != 1 {
		t.Fatalf("低胜率情境买入信号应被拦截 1 次: %d", n)
	}
	if len(a.exec.OpenOrders()) != 0 {
		t.Fatal("被拦截的信号不得进入执行器")
	}
	// 卖出信号不被拦截（离场自由）：预置多头 → 初始建仓买单被拦后清零 → 上穿触发卖单不拦
	a2, _ := buildApp(cfg)
	defer a2.exec.Stop()
	fe2, _ := umpExtractFor(a2, candles)
	for i := 0; i < ump.DefaultMinSamples; i++ {
		a2.umpFilter.Observe(fe2, false)
	}
	// a1 段的 onCandles 已把游标写进 state，a2 恢复后会对相同序列幂等跳过——重置后驱动
	a2.mu.Lock()
	a2.lastCandle = 0
	a2.mu.Unlock()
	a2.pf.Seed([]exchange.Balance{{Asset: "BTC", Total: 1, Available: 1}}, "BTC-USDT", "BTC", "USDT", price)
	base := genCandles(60)
	a2.onCandles(base) // 初始建仓买信号被拦（预期）
	if n := a2.umpBlocked.Swap(0); n != 1 {
		t.Fatalf("前置失败：初始买单应被拦: %d", n)
	}
	// 驱动后续根（价格连续微涨 → 跨格上穿产生卖单）
	up := genCandles(70)
	a2.onCandles(up)
	if n := a2.umpBlocked.Load(); n != 0 {
		t.Fatalf("卖出信号不得被 UMP 拦截: %d", n)
	}
}

func TestUMPDisabledByConfig(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	cfg.Ump.Enabled = false
	a, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.exec.Stop()
	if a.umpOn || a.umpFilter != nil {
		t.Fatal("ump.enabled=false 不得装配拦截器")
	}
}

func TestUMPStatePersistRestore(t *testing.T) {
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
	cfg := mockOKX(t)
	cfg.Mode = config.ModePaper
	a1, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a1.exec.Stop()
	fe := ump.Features{RSI: 5, DistHigh: 8, VolRank: 3}
	for i := 0; i < 30; i++ {
		a1.umpFilter.Observe(fe, i < 3) // 胜率 10%
	}
	a1.persistState()
	// 重启恢复
	a2, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a2.exec.Stop()
	if a2.umpFilter == nil || a2.umpFilter.Total() < 30 {
		t.Fatalf("重启后 UMP 统计必须恢复: %v", a2.umpFilter)
	}
	block, wr, _ := a2.umpFilter.ShouldBlock(fe)
	if !block || wr != 0.1 {
		t.Fatalf("恢复后判定必须一致: %v %v", block, wr)
	}
}

// umpExtractFor 提取当前最新已收盘根的情境特征（测试辅助）。
func umpExtractFor(a *app, candles []exchange.Candle) (ump.Features, error) {
	return ump.Extract(candles, len(candles)-1)
}
