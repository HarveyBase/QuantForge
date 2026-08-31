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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/candlestore"
	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/dashboard"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/exchange/okx"
	"github.com/HarveyBase/QuantForge/execution"
	"github.com/HarveyBase/QuantForge/grid"
	"github.com/HarveyBase/QuantForge/lab"
	"github.com/HarveyBase/QuantForge/market"
	"github.com/HarveyBase/QuantForge/notify"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/regime"
	"github.com/HarveyBase/QuantForge/review"
	"github.com/HarveyBase/QuantForge/risk"
	"github.com/HarveyBase/QuantForge/state"
	"github.com/HarveyBase/QuantForge/strategy"
	"github.com/HarveyBase/QuantForge/trend"
	"github.com/HarveyBase/QuantForge/ump"
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
	case "walkforward":
		err = cmdWalkforward(os.Args[2:])
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "umpcheck":
		err = cmdUMPCheck(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatalf("quantforge: %v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "用法: quantforge <serve|backtest|walkforward> [-config config.json]\n"+
		"  serve       启动行情+策略+管理后台（mode=research/paper/live）\n"+
		"  backtest    拉取/复用快照 K 线跑一次回测并输出指标\n"+
		"  walkforward 走样前向滚动验证（OOS 样本外成绩，防过拟合门槛）\n"+
		"  fetch       分页拉取长历史 K 线固化到 data/samples/（研究样本层）\n")
	os.Exit(2)
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.String("config", "config.json", "配置文件路径")
	if name == "serve" {
		fs.Duration("review-every", time.Hour, "复盘周期（默认 1 小时；验证环境可调短）")
	}
	return fs
}

func cfgPath(fs *flag.FlagSet) string {
	return fs.Lookup("config").Value.String()
}

// app 共享装配。
type app struct {
	mu         sync.RWMutex
	btMu       sync.Mutex
	cfg        *config.Config
	ex         exchange.Exchange
	pf         *portfolio.Portfolio
	rk         *risk.Manager
	exec       *execution.Executor
	grid       *grid.Grid        // grid 策略实例（nil 当选 trend；dashboard 网格视图用）
	strat      strategy.Strategy // 当前活跃策略（grid/trend，页面可热切）
	pol        *market.Poller
	snap       *market.SnapshotStore
	reviewer   *review.Reviewer
	reviewing  atomic.Bool        // 复盘进行中：暂停新下单（行情照收，复盘完自动恢复）
	regimeDet  *regime.Detector   // 市况识别（信息面：震荡/趋势留痕，自动路由待回测证据）
	store      *state.Store       // 运行态持久化：重启恢复游标/试验数/Kill 状态
	candlesDB  *candlestore.Store // SQLite K 线缓存库（fetch 历史 + 实时收盘增量统一入库）
	notifier   notify.Notifier    // 告警通道（Telegram，env 缺省时仅日志）
	umpFilter  *ump.Filter        // grid 买信号拦截器（启动自举+运行累积；已过样本外验证 docs/10 §5B）
	umpOn      bool               // 拦截开关（config.ump.enabled，默认开：拦截只减少下单不增加风险）
	umpBlocked atomic.Int64       // 窗口内拦截计数（复盘留痕）
	activeMode atomic.Value       // 当前活跃环境（research/paper；live 仅当启动配置为 live）

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
		cfg: cfg, ex: ex, pf: pf, rk: rk, grid: g, strat: g,
		snap: market.NewSnapshotStore(cfg.DataDir),
		pol:  &market.Poller{Ex: ex, Symbol: cfg.Exchange.InstID, Interval: cfg.Trading.Interval},
	}
	if cfg.Strategy.Name == "trend" || cfg.Strategy.Name == "both" {
		tr, terr := trend.New(trend.Params{
			EntryN: cfg.Strategy.Trend.EntryN, ExitN: cfg.Strategy.Trend.ExitN,
			AtrN: cfg.Strategy.Trend.AtrN, AtrMult: cfg.Strategy.Trend.AtrMult,
			RiskPct: cfg.Strategy.Trend.RiskPct, MaxPosPct: cfg.Strategy.Trend.MaxPosPct,
		})
		if terr != nil {
			return nil, terr
		}
		if cfg.Strategy.Name == "trend" {
			a.strat = tr
		} else {
			// 组合：grid+trend 按权重分资金（regime 路由默认关——证据纪律）
			a.strat = strategy.NewComposite([]string{"grid", "trend"},
				[]strategy.Strategy{g, tr},
				[]float64{cfg.Strategy.Both.GridWeight, cfg.Strategy.Both.TrendWeight})
		}
	}
	// paper/live 才有执行器；research 用空执行器（后台展示零订单）
	if cfg.Mode != config.ModeResearch {
		a.exec = execution.New(ex, rk, pf, func(ev execution.Event) {
			if ev.Order.FilledQty > 0 {
				if g, ok := a.strat.(interface {
					ApplyFill(exchange.Side, float64, float64)
				}); ok {
					g.ApplyFill(ev.Order.Side, ev.Order.FilledQty, ev.Order.AvgPrice)
				}
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
	// UMP 拦截器：仅 paper/live（research 不下单）。grid 版已过样本外验证（docs/10 §5B）。
	a.umpOn = cfg.Ump.Enabled && cfg.Mode != config.ModeResearch
	if a.umpOn {
		a.umpFilter = ump.NewFilter(0, 0)
		go a.bootstrapUMP()
	}
	// 小时级复盘（全部模式：research 也复盘，只记录不下单）
	rev, err := review.New(cfg.DataDir, time.Hour, a.collectReviewInput)
	if err != nil {
		return nil, err
	}
	a.reviewer = rev
	a.regimeDet = regime.NewDetector(regime.DefaultLookback, regime.DefaultConfirmBars)
	if db, derr := candlestore.Open(cfg.DataDir); derr == nil {
		a.candlesDB = db
	} else {
		log.Printf("K 线库打开失败（图表将回退内存缓存）: %v", derr)
	}
	// 告警通道：Kill/断流/连续拒单/复盘严重项 → Telegram（无凭据自动退化为日志）
	a.notifier = notify.NewFromEnv(cfg.Mode, cfg.Exchange.InstID)
	rk.Kill.OnTrip(func(reason string) {
		a.notifier.Send(fmt.Sprintf("Kill Switch 触发：%s（已自动撤单，人工复位前禁止一切新下单）", reason))
	})
	// 运行态恢复：游标（防重启重复驱动策略）、试验计数、Kill Switch 状态
	a.store = state.New(cfg.DataDir)
	if st, err := a.store.Load(); err != nil {
		log.Printf("state 恢复失败（按全新状态启动）: %v", err)
	} else {
		a.mu.Lock()
		a.lastCandle = st.LastCandle
		a.trials = st.Trials
		a.mu.Unlock()
		if st.KillTripped {
			rk.Kill.Restore(true, st.KillReason)
			log.Printf("Kill Switch 处于触发状态（%s），继续停机", st.KillReason)
		}
		a.activeMode.Store(string(cfg.Mode)) // 活跃环境初始 = 启动配置（页面可降级；升级受门禁）
		if len(st.UMP) > 0 && a.umpFilter != nil {
			snap := make(map[[3]int][2]int, len(st.UMP))
			for _, c := range st.UMP {
				snap[c.Key] = [2]int{c.Wins, c.Total}
			}
			a.umpFilter.Restore(snap)
			log.Printf("UMP 拦截器统计已恢复（%d 个情境，%d 笔样本）", len(st.UMP), a.umpFilter.Total())
		}
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
	a.persistState()
	a.pf.UpdateMark(a.cfg.Exchange.InstID, last.Close)
	rd := a.regimeDet.Update(candles)
	if rd.Kind == regime.Trending {
		// 信息面留痕：趋势市对网格是逆风（只赚震荡的钱），日志可见
		log.Printf("%s", rd.Describe())
	}
	// regime 自动路由（默认关，config 显式开启才生效——证据纪律 docs/10 §6）
	if a.cfg.Strategy.Both.RegimeRoute {
		if cp, ok := a.strat.(*strategy.Composite); ok {
			cp.SetActive("grid", rd.Kind != regime.Trending)
			cp.SetActive("trend", rd.Kind != regime.Range)
		}
	}
	// 快照留痕（每次新收盘根固化一次）
	if _, err := a.snap.Save(snapshotName(a.cfg), candles); err != nil {
		log.Printf("snapshot 保存失败: %v", err)
	}
	// 已确认 K 线增量入库（SQLite 缓存库，图表/回测读全历史）
	if a.candlesDB != nil {
		if err := a.candlesDB.Upsert(candles); err != nil {
			log.Printf("K 线入库失败: %v", err)
		}
	}
	if a.ActiveMode() == config.ModeResearch || a.exec == nil {
		return // 活跃环境为研究模式（页面可切）：只看不下单
	}
	if a.reviewing.Load() {
		log.Printf("复盘进行中，本根收盘信号跳过下单（复盘完自动恢复）")
		return
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
	for intentIndex, intent := range a.strat.OnCandle(sctx) {
		// UMP 拦截：只拦买入（离场信号自由）；卖出是风险释放不该被拦
		if a.umpOn && a.umpFilter != nil && intent.Side == exchange.Buy {
			if fe, err := ump.Extract(candles, len(candles)-1); err == nil {
				if block, wr, n := a.umpFilter.ShouldBlock(fe); block {
					a.umpBlocked.Add(1)
					log.Printf("UMP 拦截买入信号 [%s]：历史同情境胜率 %.0f%%（%d 笔）低于阈值——%s",
						intent.Kind, wr*100, n, fe.Describe())
					continue
				}
			}
		}
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

// bootstrapUMP 启动自举拦截统计：当前可得样本 → grid 回测 → 成交配对 → 入库。
// 失败留痕不阻塞交易（拦截器空统计 = 全放行，安全退化）。
func (a *app) bootstrapUMP() {
	candles := a.fetchCandles(context.Background(), 300)
	if len(candles) < 100 {
		log.Printf("UMP 自举失败：样本不足（%d 根）——拦截器空统计放行（安全退化）", len(candles))
		return
	}
	g, err := grid.New(grid.Params{
		Lower: a.cfg.Strategy.Grid.Lower, Upper: a.cfg.Strategy.Grid.Upper,
		Grids: a.cfg.Strategy.Grid.Grids, QtyPerGrid: a.cfg.Strategy.Grid.QtyPerGrid,
		Spacing: a.cfg.Strategy.Grid.Spacing, StopOnBreak: a.cfg.Strategy.Grid.StopOnBreak,
	})
	if err != nil {
		log.Printf("UMP 自举失败：网格参数非法 %v", err)
		return
	}
	eng := &backtest.Engine{Strategy: g,
		Cost:     backtest.CostModel{SlippageBps: a.cfg.Trading.SlippageBps, MakerFeeBps: 2, TakerFeeBps: 5},
		SeedCash: 10000}
	res, err := eng.Run(candles, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, 1)
	if err != nil {
		log.Printf("UMP 自举失败：回测失败 %v", err)
		return
	}
	trades, err := ump.PairTrades(candles, res.Trades)
	if err != nil {
		log.Printf("UMP 自举失败：配对失败 %v", err)
		return
	}
	for _, tr := range trades {
		a.umpFilter.Observe(tr.Features, tr.Win)
	}
	a.mu.Lock()
	a.trials++
	a.mu.Unlock()
	log.Printf("UMP 自举完成：%d 笔交易样本入库（总样本 %d）", len(trades), a.umpFilter.Total())
}

// collectReviewInput 复盘数据采集：窗口内成交/拒单/权益/K 线连续性，全部来自实时账本。
func (a *app) collectReviewInput(from time.Time) review.Input {
	in := review.Input{
		Stage: string(a.cfg.Mode), Symbol: a.cfg.Exchange.InstID, Interval: a.cfg.Trading.Interval,
	}
	cash, _, _ := a.pf.Snapshot()
	in.Cash = cash
	in.Equity = a.pf.Equity()
	in.KillTripped = a.rk.Kill.Tripped()
	in.KillReason = a.rk.Kill.Reason()
	in.UMPBlocked = int(a.umpBlocked.Swap(0)) // 取窗口值并清零（下窗重新累计）
	if a.exec != nil {
		in.OpenOrders = len(a.exec.OpenOrders())
		for _, ev := range a.exec.Events(500) {
			if ev.Ts.Before(from) || ev.DeltaQty <= 0 {
				continue
			}
			in.Fills = append(in.Fills, review.FillSummary{
				Ts: ev.Ts, Side: string(ev.Order.Side), Qty: ev.DeltaQty, Price: ev.DeltaPrice, Note: ev.Kind,
			})
		}
	}
	for _, rej := range a.rk.Rejections() {
		if rej.Ts.Before(from) {
			continue
		}
		in.Rejections = append(in.Rejections, review.RejSummary{Ts: rej.Ts, RuleID: rej.RuleID, Reason: rej.Reason})
	}
	// 窗口内 K 线首尾价与连续性
	a.mu.RLock()
	for _, c := range a.candles {
		if c.OpenTime < from.UnixMilli() {
			continue
		}
		if in.PriceFirst == 0 {
			in.PriceFirst = c.Close
		}
		in.PriceLast = c.Close
		in.CandlesSeen++
	}
	a.mu.RUnlock()
	switch st := a.strat.(type) {
	case *grid.Grid:
		gs := st.Stats()
		in.Strategy = fmt.Sprintf("grid: rounds=%d realized=%.2f broke=%v position=%.4f | 市况 %s",
			gs.Rounds, gs.Realized, gs.Broke, gs.Position, a.regimeDet.Current())
	case *trend.Donchian:
		in.Strategy = fmt.Sprintf("trend: %s | 市况 %s", st.Describe(), a.regimeDet.Current())
	default:
		in.Strategy = fmt.Sprintf("%s | 市况 %s", a.strat.Name(), a.regimeDet.Current())
	}
	return in
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
	if v := fs.Lookup("review-every"); v != nil {
		if d, perr := time.ParseDuration(v.Value.String()); perr == nil && d > 0 {
			if a.reviewer, err = review.New(cfg.DataDir, d, a.collectReviewInput); err != nil {
				return err
			}
		}
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

	// WS 行情触发器：收到已收盘 K 线即时拉取校验（秒级响应）；
	// 数据纪律：WS 只触发，入库与策略驱动仍走 REST 校验链（docs/02）。
	go okx.NewWSCandles(cfg.Exchange.InstID, cfg.Trading.Interval).WithHandler(
		func(ts int64) {
			if _, err := a.pol.FetchOnce(ctx, 300); err != nil {
				log.Printf("ws 触发拉取失败（等下一轮轮询兜底）: %v", err)
			}
		},
		func(err error) { log.Printf("%v", err) },
	).Run(ctx)

	if a.exec != nil {
		go a.exec.ReconcileLoop()
	}

	// 小时级复盘：停下来 → 生成记录落盘 → 恢复交易（失败留痕不中断进程）
	go func() {
		ticker := time.NewTicker(a.reviewer.Every())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.reviewing.Store(true)
				rec, err := a.reviewer.ReviewOnce()
				a.reviewing.Store(false)
				if err != nil {
					log.Printf("复盘失败: %v", err)
					a.notifier.Send(fmt.Sprintf("复盘失败：%v（复盘管道异常，需排查）", err))
					continue
				}
				log.Printf("复盘完成 %s：窗口收益 %+.2f%% 买入持有 %+.2f%% 成交 %d 拒单 %d（%s）",
					rec.Ts.Format("15:04"), rec.WindowRetPct, rec.PriceChgPct, len(rec.Fills), len(rec.Rejections), rec.Stage)
				for _, crit := range notify.Critical(*rec) {
					a.notifier.Send(crit)
				}
			}
		}
	}()

	runBacktest := func(ctx context.Context) (*backtest.Result, error) {
		return a.runBacktest(ctx)
	}
	runWF := func(ctx context.Context, strategyName string, train, test int) (*lab.WFReport, error) {
		if strategyName == "" {
			strategyName = "trend"
		}
		candles := a.fetchCandles(ctx, train+test)
		if len(candles) < train+test {
			return nil, fmt.Errorf("样本 %d 根不足 train+test=%d（先 fetch 长历史）", len(candles), train+test)
		}
		var selector lab.StrategySelector
		switch strategyName {
		case "trend":
			selector = lab.FixedSelector(func() strategy.Strategy {
				s, _ := trend.New(trend.Params{
					EntryN: a.cfg.Strategy.Trend.EntryN, ExitN: a.cfg.Strategy.Trend.ExitN,
					AtrN: a.cfg.Strategy.Trend.AtrN, AtrMult: a.cfg.Strategy.Trend.AtrMult,
					RiskPct: a.cfg.Strategy.Trend.RiskPct, MaxPosPct: a.cfg.Strategy.Trend.MaxPosPct,
				})
				return s
			}, fmt.Sprintf("trend:%dx%d", a.cfg.Strategy.Trend.EntryN, a.cfg.Strategy.Trend.ExitN))
		case "grid":
			selector = lab.FixedSelector(func() strategy.Strategy { return a.grid }, "grid:config")
		default:
			return nil, fmt.Errorf("未知策略 %q（支持 trend / grid）", strategyName)
		}
		return lab.WalkForward(candles, lab.WFConfig{
			TrainBars: train, TestBars: test, SeedCash: 10000,
			Cost:   backtest.CostModel{SlippageBps: a.cfg.Trading.SlippageBps, MakerFeeBps: 2, TakerFeeBps: 5},
			Symbol: a.cfg.Exchange.InstID, Interval: a.cfg.Trading.Interval,
		}, selector)
	}
	runUMP := func(ctx context.Context, strategyName string, minSamples int) (int, *ump.OOSReport, error) {
		if strategyName == "" {
			strategyName = "grid"
		}
		if minSamples <= 0 {
			minSamples = ump.DefaultMinSamples
		}
		candles := a.fetchCandles(ctx, 400)
		if len(candles) < 300 {
			return 0, nil, fmt.Errorf("样本 %d 根不足 300", len(candles))
		}
		var mk func() strategy.Strategy
		switch strategyName {
		case "trend":
			mk = func() strategy.Strategy {
				s, _ := trend.New(trend.Params{
					EntryN: a.cfg.Strategy.Trend.EntryN, ExitN: a.cfg.Strategy.Trend.ExitN,
					AtrN: a.cfg.Strategy.Trend.AtrN, AtrMult: a.cfg.Strategy.Trend.AtrMult,
					RiskPct: a.cfg.Strategy.Trend.RiskPct, MaxPosPct: a.cfg.Strategy.Trend.MaxPosPct,
				})
				return s
			}
		case "grid":
			mk = func() strategy.Strategy { return a.grid }
		default:
			return 0, nil, fmt.Errorf("未知策略 %q", strategyName)
		}
		return lab.UMPCheck(candles, backtest.CostModel{SlippageBps: a.cfg.Trading.SlippageBps, MakerFeeBps: 2, TakerFeeBps: 5},
			10000, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, mk, ump.DefaultMinWinRate, minSamples)
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
	srv.RecentReviews = a.reviewer.Recent
	srv.RunWalkForward = runWF
	srv.RunUMPCheck = runUMP
	srv.CurrentStrategy = a.CurrentStrategy
	srv.SwitchStrategy = a.SwitchStrategy
	srv.Fills = func() []execution.Event {
		var out []execution.Event
		for _, ev := range a.exec.Events(500) {
			if ev.DeltaQty > 0 {
				out = append(out, ev)
			}
		}
		return out
	}
	srv.EquityCurve = func() []review.Record { return a.reviewer.Recent(72) }
	if a.exec != nil {
		srv.CancelOrder = func(ctx context.Context, id string) error {
			return a.exec.Cancel(ctx, cfg.Exchange.InstID, id)
		}
		srv.PlaceOrder = func(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
			return a.exec.Submit(ctx, req) // 完整风控路径（限额/频率/现金/Kill 全部生效）
		}
	}
	srv.Candles = func(interval string, limit int) []exchange.Candle {
		if interval == "" {
			interval = cfg.Trading.Interval
		}
		if a.candlesDB != nil {
			if cs, err := a.candlesDB.Latest("okx", cfg.Exchange.InstID, interval, limit); err == nil && len(cs) > 0 {
				return cs
			}
		}
		// 库空回退内存缓存（仅默认周期）
		a.mu.RLock()
		defer a.mu.RUnlock()
		return append([]exchange.Candle(nil), a.candles...)
	}
	srv.ActiveMode = a.ActiveMode
	srv.BootMode = cfg.Mode
	srv.SwitchMode = a.SwitchMode
	srv.Regime = func() regime.Reading {
		return regime.Reading{Kind: a.regimeDet.Current(), Lookback: regime.DefaultLookback, Confirm: regime.DefaultConfirmBars}
	}

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

// persistState 运行态落盘（游标/试验数/Kill）；失败留痕不影响主流程。
func (a *app) persistState() {
	a.mu.RLock()
	st := state.Runtime{
		LastCandle: a.lastCandle, Trials: a.trials,
		KillTripped: a.rk.Kill.Tripped(), KillReason: a.rk.Kill.Reason(),
	}
	if a.umpFilter != nil {
		for k, v := range a.umpFilter.Snapshot() {
			st.UMP = append(st.UMP, state.UmpCell{Key: k, Wins: v[0], Total: v[1]})
		}
	}
	a.mu.RUnlock()
	if err := a.store.Save(st); err != nil {
		log.Printf("state 落盘失败: %v", err)
	}
}

// ActiveMode 当前活跃环境（页面热切目标；未初始化回退启动配置）。
func (a *app) ActiveMode() config.Mode {
	if v, ok := a.activeMode.Load().(string); ok && v != "" {
		return config.Mode(v)
	}
	return a.cfg.Mode
}

// SwitchMode 页面热切活跃环境。
// 权限矩阵（docs/08 门禁纪律）：research↔paper 自由切；
// 目标 live 仅当启动配置本身就是 live（升级必须重启 + 环境变量门禁 + 确认词，页面不得一键进实盘）。
func (a *app) SwitchMode(target config.Mode, confirm string) error {
	switch target {
	case config.ModeResearch, config.ModePaper:
		// paper 需要执行器（research 启动的进程没装配）
		if target == config.ModePaper && a.exec == nil {
			return fmt.Errorf("本进程以 research 配置启动，未装配执行器——切换 paper 需以 paper 配置重启")
		}
	case config.ModeLive:
		if a.cfg.Mode != config.ModeLive {
			return fmt.Errorf("live 升级必须以 live 配置重启进程 + 环境变量门禁（%s），页面不得一键进入实盘", config.LiveGateEnv)
		}
		if confirm != "I_UNDERSTAND_THE_RISK" {
			return fmt.Errorf("切回 live 需要确认词 I_UNDERSTAND_THE_RISK")
		}
	default:
		return fmt.Errorf("未知环境 %q", target)
	}
	a.activeMode.Store(string(target))
	return nil
}

// SwitchStrategy 页面热切策略：仅无持仓时允许（持仓中换策略=退出规则悬空，禁止）。
func (a *app) SwitchStrategy(name string) error {
	var s strategy.Strategy
	switch name {
	case "grid":
		g, err := grid.New(grid.Params{
			Lower: a.cfg.Strategy.Grid.Lower, Upper: a.cfg.Strategy.Grid.Upper,
			Grids: a.cfg.Strategy.Grid.Grids, QtyPerGrid: a.cfg.Strategy.Grid.QtyPerGrid,
			Spacing: a.cfg.Strategy.Grid.Spacing, StopOnBreak: a.cfg.Strategy.Grid.StopOnBreak,
		})
		if err != nil {
			return err
		}
		s = g
	case "trend":
		tr, err := trend.New(trend.Params{
			EntryN: a.cfg.Strategy.Trend.EntryN, ExitN: a.cfg.Strategy.Trend.ExitN,
			AtrN: a.cfg.Strategy.Trend.AtrN, AtrMult: a.cfg.Strategy.Trend.AtrMult,
			RiskPct: a.cfg.Strategy.Trend.RiskPct, MaxPosPct: a.cfg.Strategy.Trend.MaxPosPct,
		})
		if err != nil {
			return err
		}
		s = tr
	case "both":
		tr, err := trend.New(trend.Params{
			EntryN: a.cfg.Strategy.Trend.EntryN, ExitN: a.cfg.Strategy.Trend.ExitN,
			AtrN: a.cfg.Strategy.Trend.AtrN, AtrMult: a.cfg.Strategy.Trend.AtrMult,
			RiskPct: a.cfg.Strategy.Trend.RiskPct, MaxPosPct: a.cfg.Strategy.Trend.MaxPosPct,
		})
		if err != nil {
			return err
		}
		s = strategy.NewComposite([]string{"grid", "trend"}, []strategy.Strategy{a.grid, tr},
			[]float64{a.cfg.Strategy.Both.GridWeight, a.cfg.Strategy.Both.TrendWeight})
	default:
		return fmt.Errorf("未知策略 %q（支持 grid / trend / both）", name)
	}
	_, positions, _ := a.pf.Snapshot()
	for _, p := range positions {
		if p.Qty != 0 {
			return fmt.Errorf("持仓中禁止切换策略（%s 数量 %v，先平仓再切）", p.Symbol, p.Qty)
		}
	}
	a.strat = s
	return nil
}

// CurrentStrategy 当前策略名与描述。
func (a *app) CurrentStrategy() string {
	if d, ok := a.strat.(interface{ Describe() string }); ok {
		return d.Describe()
	}
	return a.strat.Name()
}

// fetchCandles 数据降级链（docs/02 完整性优先）：缓存 → 实时拉取 → 快照 → 固定样本。
func (a *app) fetchCandles(ctx context.Context, minBars int) []exchange.Candle {
	a.mu.RLock()
	candles := append([]exchange.Candle(nil), a.candles...)
	a.mu.RUnlock()
	if len(candles) < minBars {
		cs, err := a.ex.GetCandles(ctx, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, 300)
		if err == nil {
			cs, err = market.Validate(cs, market.IntervalMs(a.cfg.Trading.Interval))
			if err == nil {
				candles = cs
			}
		} else {
			log.Printf("实时拉取失败，降级快照: %v", err)
		}
	}
	if len(candles) < minBars {
		if cs, err := a.snap.LoadLatest(snapshotName(a.cfg)); err == nil {
			candles = cs
		}
	}
	if len(candles) < minBars {
		if err := a.loadFixedSample(); err == nil {
			log.Printf("使用固定样本层 data/samples/（离线口径，结果仅用于演示）")
			a.mu.RLock()
			candles = append([]exchange.Candle(nil), a.candles...)
			a.mu.RUnlock()
		}
	}
	return candles
}

// runBacktest 用当前缓存的 K 线跑回测（试验计数累计，防数据窥探）。
// 数据降级顺序（docs/02 完整性优先）：缓存 → 实时拉取 → 快照 → 固定样本。
func (a *app) runBacktest(ctx context.Context) (*backtest.Result, error) {
	a.btMu.Lock()
	defer a.btMu.Unlock()
	candles := a.fetchCandles(ctx, 30)
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

// cmdWalkforwardFlagSet walkforward 子命令的旗标。
func cmdWalkforwardFlagSet() (*flag.FlagSet, *int, *int, *string) {
	fs := flag.NewFlagSet("walkforward", flag.ExitOnError)
	fs.String("config", "config.json", "配置文件路径")
	train := fs.Int("train", 150, "训练窗（根）")
	test := fs.Int("test", 50, "测试窗（根），同时为滚动步长")
	strategyName := fs.String("strategy", "trend", "验证策略：trend | grid")
	return fs, train, test, strategyName
}

// cmdWalkforward 走样前向滚动验证：训练窗选参 → 测试窗出样本外成绩 → 滚动推进。
// 这是策略晋级的硬门槛（docs/01）：样本外不过，回测再好看也只是研究线索。
func cmdWalkforward(args []string) error {
	fs, train, test, strategyName := cmdWalkforwardFlagSet()
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
	candles := a.fetchCandles(context.Background(), *train+*test)
	if len(candles) < *train+*test {
		return fmt.Errorf("K 线不足（%d 根，walkforward 需要 ≥ train+test = %d）", len(candles), *train+*test)
	}
	wfcfg := lab.WFConfig{
		TrainBars: *train, TestBars: *test, SeedCash: 10000,
		Cost:   backtest.CostModel{SlippageBps: cfg.Trading.SlippageBps, MakerFeeBps: 2, TakerFeeBps: 5},
		Symbol: cfg.Exchange.InstID, Interval: cfg.Trading.Interval,
	}
	var selector lab.StrategySelector
	switch *strategyName {
	case "trend":
		selector = lab.FixedSelector(func() strategy.Strategy {
			s, _ := trend.New(trend.DefaultParams())
			return s
		}, "trend:"+trend.DefaultParams().String())
	case "grid":
		selector = lab.FixedSelector(func() strategy.Strategy {
			s, _ := grid.New(grid.Params{
				Lower: cfg.Strategy.Grid.Lower, Upper: cfg.Strategy.Grid.Upper,
				Grids: cfg.Strategy.Grid.Grids, QtyPerGrid: cfg.Strategy.Grid.QtyPerGrid,
				Spacing: cfg.Strategy.Grid.Spacing, StopOnBreak: cfg.Strategy.Grid.StopOnBreak,
			})
			return s
		}, "grid:config")
	default:
		return fmt.Errorf("未知策略 %q（支持 trend / grid）", *strategyName)
	}
	rep, err := lab.WalkForward(candles, wfcfg, selector)
	if err != nil {
		return err
	}
	fmt.Printf("walkforward（%s，%d 根 %s K 线，train %d / test %d，共 %d 折，试验 %d 次）\n",
		*strategyName, rep.Candles, cfg.Trading.Interval, *train, *test, len(rep.Folds), rep.TotalTrials)
	for _, f := range rep.Folds {
		fmt.Printf("  折%-2d 测试区间 %s~%s  %s  收益 %+.2f%%  MDD %.2f%%  交易 %d\n",
			f.Fold,
			time.UnixMilli(f.TestFrom).Format("01-02 15:04"), time.UnixMilli(f.TestTo).Format("01-02 15:04"),
			f.Strategy, f.Metrics.TotalReturnPct, f.Metrics.MaxDrawdownPct, f.Metrics.TradeCount)
	}
	m := rep.OOSMetrics
	totalTrades := 0
	for _, f := range rep.Folds {
		totalTrades += f.Metrics.TradeCount
	}
	fmt.Printf("样本外（OOS）：收益 %+.2f%%  MDD %.2f%%  Calmar %.2f  交易 %d  |  对照（买入持有）%+.2f%%\n",
		m.TotalReturnPct, m.MaxDrawdownPct, m.Calmar, totalTrades, rep.BuyHoldPct)
	switch {
	case totalTrades < 10:
		fmt.Println("判定：交易数 < 10，证据不足（样本太短或参数未触发，不得据此下任何结论）")
	case len(rep.Folds) < 3:
		fmt.Println("判定：折数不足 3，证据不足（不得据此下任何结论）")
	case m.TotalReturnPct > rep.BuyHoldPct:
		fmt.Println("判定：样本外跑赢买入持有（仅过第一道门槛；还需参数高原 + 成本敏感性 + 模拟盘）")
	default:
		fmt.Println("判定：样本外未跑赢买入持有——策略在本样本上不成立，降级为研究线索")
	}
	fmt.Println("（OOS 成绩不代表实盘收益；口径与晋级门槛见 docs/01、docs/02）")
	return nil
}

// cmdFetch 分页拉取长历史 K 线 → market.Validate 清洗 → 固化到固定样本层。
// 用途：给 walk-forward 提供足够长的样本（趋势策略统计意义需要数百笔交易）。
func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	fs.String("config", "config.json", "配置文件路径")
	bars := fs.Int("bars", 2000, "目标根数（上限 20000）")
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
	client, ok := a.ex.(*okx.Client)
	if !ok {
		return fmt.Errorf("fetch 需要 okx 适配器（当前 %s）", a.ex.Name())
	}
	// 超时按拉取量估算：每页 100 根、限频 10 页/秒 × 3 倍网络余量，下限 5 分钟
	timeout := time.Duration(*bars/100) * 300 * time.Millisecond
	if timeout < 5*time.Minute {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, err := client.GetCandlesHistory(ctx, cfg.Exchange.InstID, cfg.Trading.Interval, *bars)
	if err != nil {
		return fmt.Errorf("历史拉取失败: %w", err)
	}
	clean, err := market.Validate(raw, market.IntervalMs(cfg.Trading.Interval))
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s_%s.json",
		strings.ToLower(strings.ReplaceAll(cfg.Exchange.InstID, "-", "_")),
		strings.ToLower(cfg.Trading.Interval))
	path := filepath.Join(cfg.DataDir, "samples", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(clean, "", " ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	// 同步入库 SQLite 缓存库（图表/回测统一数据面）
	if db, derr := candlestore.Open(cfg.DataDir); derr == nil {
		if uerr := db.Upsert(clean); uerr != nil {
			log.Printf("fetch 入库失败: %v", uerr)
		} else if n, _ := db.Count("okx", cfg.Exchange.InstID, cfg.Trading.Interval); n > 0 {
			fmt.Printf("已入库 SQLite：%d 根 %s K 线（data/candles.db）\n", n, cfg.Trading.Interval)
		}
		db.Close()
	}
	first, last := clean[0], clean[len(clean)-1]
	fmt.Printf("已固化 %d 根 %s K 线到 %s\n区间: %s ~ %s\n",
		len(clean), cfg.Trading.Interval, path,
		time.UnixMilli(first.OpenTime).Format("2006-01-02 15:04"),
		time.UnixMilli(last.OpenTime).Format("2006-01-02 15:04"))
	return nil
}

// cmdUMPCheck 拦截器研究入口：样本 → 回测 → 提取交易情境 → 拦截器 OOS 自验证。
// 通过才允许部署到 serve（拦截器未过样本外就是拟合噪音）。
func cmdUMPCheck(args []string) error {
	fs := flag.NewFlagSet("umpcheck", flag.ExitOnError)
	fs.String("config", "config.json", "配置文件路径")
	strategyName := fs.String("strategy", "trend", "研究策略：trend | grid")
	minSamples := fs.Int("min-samples", ump.DefaultMinSamples, "情境最小样本数（证据不足不拦截）")
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
	candles := a.fetchCandles(context.Background(), 400)
	if len(candles) < 300 {
		return fmt.Errorf("K 线不足（%d 根，umpcheck 需要 ≥300）", len(candles))
	}
	cost := backtest.CostModel{SlippageBps: cfg.Trading.SlippageBps, MakerFeeBps: 2, TakerFeeBps: 5}
	var mk func() strategy.Strategy
	switch *strategyName {
	case "trend":
		mk = func() strategy.Strategy { s, _ := trend.New(trend.DefaultParams()); return s }
	case "grid":
		mk = func() strategy.Strategy {
			g, _ := grid.New(grid.Params{
				Lower: cfg.Strategy.Grid.Lower, Upper: cfg.Strategy.Grid.Upper,
				Grids: cfg.Strategy.Grid.Grids, QtyPerGrid: cfg.Strategy.Grid.QtyPerGrid,
				Spacing: cfg.Strategy.Grid.Spacing, StopOnBreak: cfg.Strategy.Grid.StopOnBreak,
			})
			return g
		}
	default:
		return fmt.Errorf("未知策略 %q（支持 trend / grid）", *strategyName)
	}
	n, rep, err := lab.UMPCheck(candles, cost, 10000, cfg.Exchange.InstID, cfg.Trading.Interval,
		mk, ump.DefaultMinWinRate, *minSamples)
	if err != nil {
		return err
	}
	fmt.Printf("umpcheck（%s，%d 根 %s K 线，交易样本 %d 笔）\n", *strategyName, len(candles), cfg.Trading.Interval, n)
	fmt.Printf("  %s\n", rep.Reason)
	if rep.Usable {
		fmt.Println("判定：拦截器样本外有效——可部署（建议先模拟盘观察一段时间）")
	} else {
		fmt.Println("判定：拦截器未过样本外验证——不得部署到 serve（防拟合噪音）")
	}
	fmt.Println("（拦截器是研究工具；结论基于当前样本，样本更新后需重验）")
	return nil
}
