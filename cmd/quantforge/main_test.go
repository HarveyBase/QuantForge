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
