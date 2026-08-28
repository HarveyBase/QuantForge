// QuantForge 入口：serve（行情+策略+后台）/ backtest（命令行回测）。
// 模式由 config.json 的 mode 决定：research / paper / live（live 需过环境变量门禁）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/dashboard"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/exchange/okx"
	"github.com/HarveyBase/QuantForge/execution"
	"github.com/HarveyBase/QuantForge/grid"
	"github.com/HarveyBase/QuantForge/market"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
	"github.com/HarveyBase/QuantForge/strategy"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(flagSet("serve"), os.Args[2:])
	case "backtest":
		err = cmdBacktest(flagSet("backtest"), os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatalf("quantforge: %v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "用法: quantforge <serve|backtest> [-config config.json]\n"+
		"  serve    启动行情+策略+管理后台（mode=research/paper/live）\n"+
		"  backtest 拉取/复用快照 K 线跑一次回测并输出指标\n")
	os.Exit(2)
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.String("config", "config.json", "配置文件路径")
	return fs
}

func cfgPath(fs *flag.FlagSet) string {
	return fs.Lookup("config").Value.String()
}

// app 共享装配。
type app struct {
	mu   sync.RWMutex
	btMu sync.Mutex
	cfg  *config.Config
	ex   exchange.Exchange
	pf   *portfolio.Portfolio
	rk   *risk.Manager
	exec *execution.Executor
	grid *grid.Grid
	pol  *market.Poller
	snap *market.SnapshotStore

	candles    []exchange.Candle // 最近已确认序列
	lastCandle int64             // 已处理的最新收盘 OpenTime（防重复驱动策略）
	trials     int               // 回测试验计数（防数据窥探）
}

func buildApp(cfg *config.Config) (*app, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.CheckLiveGate(); err != nil {
		return nil, err
	}
	// 交易所适配器：三级模式同一抽象，只换实例
	var ex exchange.Exchange
	switch cfg.Mode {
	case config.ModePaper:
		ex = okx.NewPaperWithURL(cfg.Exchange.RestURL, cfg.Exchange.TdMode, cfg.Exchange.Leverage)
	case config.ModeLive:
		ex = okx.NewLiveWithURL(cfg.Exchange.RestURL, cfg.Exchange.TdMode, cfg.Exchange.Leverage)
	default:
		ex = okx.NewPublicWithURL(cfg.Exchange.RestURL)
	}
	pf := portfolio.New(0)
	rkLimits := risk.Limits{
		MaxOrderNotionalUSD:    cfg.Risk.MaxOrderNotionalUSD,
		MaxDailyNotionalUSD:    cfg.Risk.MaxDailyNotionalUSD,
		MaxPositionNotionalUSD: cfg.Risk.MaxPositionNotionalUSD,
		MaxOrdersPerMinute:     cfg.Risk.MaxOrdersPerMinute,
		MaxDailyLossPct:        cfg.Risk.MaxDailyLossPct,
		CooldownAfterRejectSec: cfg.Risk.CooldownAfterRejectSec,
	}
	// 启动时先同步现货账户，失败则不进入交易流程。
	if cfg.Mode != config.ModeResearch {
		balances, err := ex.GetBalances(context.Background())
		if err != nil {
			return nil, fmt.Errorf("账户初始化失败: %w", err)
		}
		pf.Seed(balances, cfg.Exchange.InstID, strings.Split(cfg.Exchange.InstID, "-")[0], "USDT", 0)
	}
	rk := risk.NewManager(rkLimits, pf, filepath.Join(cfg.DataDir, "logs", "rejections.jsonl"))
	if cfg.Mode != config.ModeResearch {
		rk.SetDayStartEquity(pf.Equity())
	}
	g, err := grid.New(grid.Params{
		Lower: cfg.Strategy.Grid.Lower, Upper: cfg.Strategy.Grid.Upper,
		Grids: cfg.Strategy.Grid.Grids, QtyPerGrid: cfg.Strategy.Grid.QtyPerGrid,
		Spacing: cfg.Strategy.Grid.Spacing, StopOnBreak: cfg.Strategy.Grid.StopOnBreak,
	})
	if err != nil {
		return nil, err
	}
	a := &app{
		cfg: cfg, ex: ex, pf: pf, rk: rk, grid: g,
		snap: market.NewSnapshotStore(cfg.DataDir),
		pol:  &market.Poller{Ex: ex, Symbol: cfg.Exchange.InstID, Interval: cfg.Trading.Interval},
	}
	// paper/live 才有执行器；research 用空执行器（后台展示零订单）
	if cfg.Mode != config.ModeResearch {
		a.exec = execution.New(ex, rk, pf, func(ev execution.Event) {
			if ev.Order.FilledQty > 0 {
				a.grid.ApplyFill(ev.Order.Side, ev.Order.FilledQty, ev.Order.AvgPrice)
			}
		})
		rk.Kill.OnTrip(func(reason string) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				a.exec.CancelAll(ctx, cfg.Exchange.InstID)
			}()
		})
	}
	return a, nil
}

// onCandles 数据更新回调：更新标记价 + 驱动策略（只在新收盘根上）。
func (a *app) onCandles(candles []exchange.Candle) {
	a.mu.Lock()
	a.candles = append([]exchange.Candle(nil), candles...)
	a.mu.Unlock()
	if len(candles) == 0 {
		return
	}
	last := candles[len(candles)-1]
	a.mu.Lock()
	if last.OpenTime == a.lastCandle || len(candles) < a.grid.Warmup() {
		a.mu.Unlock()
		return
	}
	a.lastCandle = last.OpenTime
	a.mu.Unlock()
	a.pf.UpdateMark(a.cfg.Exchange.InstID, last.Close)
	// 快照留痕（每次新收盘根固化一次）
	if _, err := a.snap.Save(snapshotName(a.cfg), candles); err != nil {
		log.Printf("snapshot 保存失败: %v", err)
	}
	if a.cfg.Mode == config.ModeResearch || a.exec == nil {
		return // 研究模式不下单
	}
	cash, positions, _ := a.pf.Snapshot()
	posQty := 0.0
	for _, p := range positions {
		if p.Symbol == a.cfg.Exchange.InstID {
			posQty = p.Qty
		}
	}
	sctx := &strategy.Context{
		Symbol: a.cfg.Exchange.InstID, Interval: a.cfg.Trading.Interval,
		Candles: candles, Equity: a.pf.Equity(), Position: posQty, Cash: cash,
	}
	for intentIndex, intent := range a.grid.OnCandle(sctx) {
		req := exchange.OrderRequest{
			Symbol: a.cfg.Exchange.InstID, Side: intent.Side, Type: intent.Type,
			Price: intent.Price, Qty: intent.Qty,
			ClientOrderID: fmt.Sprintf("qf-%d-%s-%d", last.OpenTime, intent.Kind, intentIndex),
		}
		if _, err := a.exec.Submit(context.Background(), req); err != nil {
			log.Printf("下单失败 [%s]: %v", intent.Kind, err) // 拒单留痕，不静默
		}
	}
}

// loadFixedSample 从固定样本层加载（data/samples/<lower(symbol)>_<interval>.json）。
func (a *app) loadFixedSample() error {
	name := fmt.Sprintf("%s_%s.json",
		strings.ToLower(strings.ReplaceAll(a.cfg.Exchange.InstID, "-", "_")),
		strings.ToLower(a.cfg.Trading.Interval))
	path := filepath.Join(a.cfg.DataDir, "samples", name)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cs []exchange.Candle
	if err := json.Unmarshal(b, &cs); err != nil {
		return err
	}
	ms := market.IntervalMs(a.cfg.Trading.Interval)
	clean, err := market.Validate(cs, ms)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.candles = clean
	a.mu.Unlock()
	return nil
}

func snapshotName(cfg *config.Config) string {
	return fmt.Sprintf("%s_%s_%s", cfg.Exchange.Name, cfg.Exchange.InstID, cfg.Trading.Interval)
}

func cmdServe(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath(fs))
	if err != nil {
		return err
	}
	a, err := buildApp(cfg)
	if err != nil {
		return err
	}
	a.pol.OnUpdate = a.onCandles
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 首拉一次 + 周期轮询
	intervalMs := market.IntervalMs(cfg.Trading.Interval)
	if intervalMs == 0 {
		return fmt.Errorf("未知 K 线周期 %q", cfg.Trading.Interval)
	}
	if _, err := a.pol.FetchOnce(ctx, 300); err != nil {
		log.Printf("首次拉取行情失败（将重试）: %v", err)
	}
	pollEvery := time.Duration(intervalMs/4) * time.Millisecond
	if pollEvery < 15*time.Second {
		pollEvery = 15 * time.Second
	}
	go a.pol.Run(ctx, 300, pollEvery)

	if a.exec != nil {
		go a.exec.ReconcileLoop()
	}

	runBacktest := func(ctx context.Context) (*backtest.Result, error) {
		return a.runBacktest(ctx)
	}
	var orderSrc dashboard.OrderSource = dashboard.NoopExecutor{}
	if a.exec != nil {
		orderSrc = a.exec
	}
	srv := dashboard.New(cfg, a.pf, a.rk, orderSrc, a.grid, func() []exchange.Candle {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return append([]exchange.Candle(nil), a.candles...)
	}, runBacktest)

	if !cfg.Dashboard.Enabled {
		log.Printf("mode=%s symbol=%s（后台未启用）", cfg.Mode, cfg.Exchange.InstID)
		<-ctx.Done()
		return nil
	}
	httpSrv := &http.Server{Addr: cfg.Dashboard.Listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	log.Printf("QuantForge %s mode=%s symbol=%s 后台 http://%s",
		"v0.1.0", cfg.Mode, cfg.Exchange.InstID, cfg.Dashboard.Listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// runBacktest 用当前缓存的 K 线跑回测（试验计数累计，防数据窥探）。
// 数据降级顺序（docs/02 完整性优先）：缓存 → 实时拉取 → 快照 → 固定样本。
func (a *app) runBacktest(ctx context.Context) (*backtest.Result, error) {
	a.btMu.Lock()
	defer a.btMu.Unlock()
	a.mu.RLock()
	candles := append([]exchange.Candle(nil), a.candles...)
	a.mu.RUnlock()
	if len(candles) < 30 {
		cs, err := a.ex.GetCandles(ctx, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, 300)
		if err == nil {
			cs, err = market.Validate(cs, market.IntervalMs(a.cfg.Trading.Interval))
		}
		if err == nil {
			candles = cs
		} else {
			log.Printf("实时拉取失败，降级快照: %v", err)
		}
	}
	if len(candles) < 30 {
		if cs, err := a.snap.LoadLatest(snapshotName(a.cfg)); err == nil {
			candles = cs
		}
	}
	if len(candles) < 30 {
		if err := a.loadFixedSample(); err != nil {
			return nil, fmt.Errorf("行情不足且所有数据层失败: %w", err)
		}
		log.Printf("使用固定样本层 data/samples/（离线口径，结果仅用于演示）")
	}
	a.mu.RLock()
	if len(candles) < 30 {
		candles = append([]exchange.Candle(nil), a.candles...)
	}
	a.mu.RUnlock()
	if len(candles) < 30 {
		return nil, fmt.Errorf("K 线不足（%d 根，至少 30）", len(candles))
	}
	a.mu.Lock()
	a.trials++
	trials := a.trials
	a.mu.Unlock()
	g, err := grid.New(grid.Params{
		Lower: a.cfg.Strategy.Grid.Lower, Upper: a.cfg.Strategy.Grid.Upper,
		Grids: a.cfg.Strategy.Grid.Grids, QtyPerGrid: a.cfg.Strategy.Grid.QtyPerGrid,
		Spacing: a.cfg.Strategy.Grid.Spacing, StopOnBreak: a.cfg.Strategy.Grid.StopOnBreak,
	})
	if err != nil {
		return nil, err
	}
	eng := &backtest.Engine{
		Strategy: g,
		Cost:     backtest.CostModel{SlippageBps: a.cfg.Trading.SlippageBps, MakerFeeBps: 2, TakerFeeBps: 5},
		SeedCash: 10000,
	}
	res, err := eng.Run(candles, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, trials)
	if err != nil {
		return nil, err
	}
	a.saveBacktest(res)
	return res, nil
}

// saveBacktest 结果归档 + 试验台账（docs/01：全部试验含失败都要留痕）。
func (a *app) saveBacktest(res *backtest.Result) {
	dir := filepath.Join(a.cfg.DataDir, "backtests")
	os.MkdirAll(dir, 0o755)
	name := fmt.Sprintf("%s_%d.json", snapshotName(a.cfg), time.Now().Unix())
	b, err := json.MarshalIndent(res, "", " ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	ledger := map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "num_trials": res.NumTrials,
		"total_return_pct": res.Metrics.TotalReturnPct, "max_drawdown_pct": res.Metrics.MaxDrawdownPct,
		"trade_count": res.Metrics.TradeCount, "file": name,
	}
	if b, err := json.Marshal(ledger); err == nil {
		f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
}

func cmdBacktest(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath(fs))
	if err != nil {
		return err
	}
	a, err := buildApp(cfg)
	if err != nil {
		return err
	}
	res, err := a.runBacktest(context.Background())
	if err != nil {
		return err
	}
	m := res.Metrics
	fmt.Printf("样本区间: %s ~ %s（%d 根 %s K 线，试验第 %d 次）\n",
		time.UnixMilli(res.SampleFrom).Format("2006-01-02 15:04"), time.UnixMilli(res.SampleTo).Format("2006-01-02 15:04"),
		len(a.candles), cfg.Trading.Interval, res.NumTrials)
	fmt.Printf("策略收益: %.2f%%  基准(买入持有): %.2f%%  MDD: %.2f%%  Calmar: %.2f  Sharpe: %.2f\n",
		m.TotalReturnPct, m.BuyHoldPct, m.MaxDrawdownPct, m.Calmar, m.Sharpe)
	fmt.Printf("交易次数: %d  胜率: %.1f%%  手续费: %.2f  期末权益: %.2f\n",
		m.TradeCount, m.WinRate, m.TotalFees, m.FinalEquity)
	fmt.Printf("拒单: %d 笔  期末挂单: %d 张\n", len(res.RiskRejections), len(res.PendingOrders))
	fmt.Println("（回测输出不代表实盘收益；口径与限制见 docs/02、docs/08）")
	return nil
}
