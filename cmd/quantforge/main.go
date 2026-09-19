// QuantForge 入口：serve（行情+策略+后台）/ backtest（命令行回测）。
// 模式由 config.json 的 mode 决定：research / paper / live（live 需过环境变量门禁）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
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

	// 运行保障状态（权益看门狗 / 账户对账 / 断流告警）
	recMu     sync.Mutex                // lastRec 读写锁
	lastRec   portfolio.ReconcileReport // 最近一次对账报告（dashboard /api/status 展示）
	recFails  atomic.Int64              // 对账拉取连续失败计数（成功清零；RISK-3 fail-closed 判定）
	recAlert  alertThrottle             // 对账差异告警节流（10 分钟防刷屏）
	feedErrs  atomic.Int64              // 行情拉取连续失败计数（恢复清零）
	feedAlert alertThrottle             // 断流告警节流
	wsAlert   alertThrottle             // WS 行情异常告警节流
	markAlert alertThrottle             // 看门狗 ticker 拉取失败日志节流

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
	// 交易所适配器：三级模式同一抽象，只换实例。
	// REST 限速统一走 ops.rest_rate_limit_per_sec（防触发交易所限流）。
	rlOpt := okx.WithRateLimit(float64(cfg.Ops.RestRateLimitPerSec))
	var ex exchange.Exchange
	switch cfg.Mode {
	case config.ModePaper:
		ex = okx.NewPaperWithURL(cfg.Exchange.RestURL, cfg.Exchange.TdMode, cfg.Exchange.Leverage, rlOpt)
	case config.ModeLive:
		ex = okx.NewLiveWithURL(cfg.Exchange.RestURL, cfg.Exchange.TdMode, cfg.Exchange.Leverage, rlOpt)
	default:
		ex = okx.NewPublicWithURL(cfg.Exchange.RestURL, rlOpt)
	}
	// 时钟校准（paper/live）：本地时钟漂移超签名窗口会导致私有接口静默拒绝，
	// 尽力而为——失败只告警不阻断（首次签名请求前还会自动重试一次）。
	if cfg.Mode != config.ModeResearch {
		if oc, ok := ex.(*okx.Client); ok {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := oc.CalibrateClock(cctx)
			cancel()
			switch {
			case err != nil:
				log.Printf("okx: 时钟校准失败（不阻断启动）: %v", err)
			case abs64(oc.ClockOffsetMS()) > 3000:
				log.Printf("⚠️ 本地时钟与 OKX 服务器偏移 %dms（>3000ms）：签名接口可能间歇失败，请检查 NTP", oc.ClockOffsetMS())
			default:
				log.Printf("okx: 时钟校准完成，偏移 %dms", oc.ClockOffsetMS())
			}
		}
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
	// RISK-2：余额拉取必须晚于在途单认领（adopt 在下方交易装配段执行）——
	// T0 拉挂单 → T1 拉余额 → 清零账本 → Seed：(T0,T1] 窗口内的成交已包含在
	// T1 余额里，以余额重建账本无成交丢失窗口。
	rk := risk.NewManager(rkLimits, pf, filepath.Join(cfg.DataDir, "logs", "rejections.jsonl"))
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
	// 告警通道提前装配：启动恢复/对账/断流告警在 buildApp 阶段就要用。
	// Kill/断流/连续拒单/复盘严重项 → Telegram（无凭据自动退化为日志）。
	a.notifier = notify.NewFromEnv(cfg.Mode, cfg.Exchange.InstID)
	// 运行态提前读取（日内基线延续要用；游标/Kill/UMP 的恢复应用仍在装配末段统一做）。
	a.store = state.New(cfg.DataDir)
	st, stErr := a.store.Load()
	if stErr != nil {
		log.Printf("state 恢复失败（按全新状态启动）: %v", stErr)
	}
	// paper/live 交易装配。启动顺序（红线：先对账再恢复交易）：
	// journal 恢复订单簿 → 认领交易所在途单（T0）→ 拉取余额（T1，(T0,T1] 成交无丢失）→
	// 清零并以真实 last 价 Seed 账本 → 日内权益基线跨重启延续 →
	// 强制账户对账（差异则拦截下单，不阻断进程）。
	if cfg.Mode != config.ModeResearch {
		// 执行器；research 用空执行器（后台展示零订单）
		a.exec = execution.New(ex, rk, pf, func(ev execution.Event) {
			if ev.Order.FilledQty > 0 {
				// BUG-E：回调可能在任意 goroutine 触发，读 a.strat 必须持读锁
				// （与 SwitchStrategy 的写锁配对），锁内只取局部引用。
				a.mu.RLock()
				strat := a.strat
				a.mu.RUnlock()
				if g, ok := strat.(interface {
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
				// RISK-7：撤单失败不得静默——与本地挂单数比对，不完整即告警人工兜底
				a.cancelAllChecked(ctx, "Kill 触发")
			}()
		})
		// Kill 触发链保持现状：撤单 + 告警。cfg.Ops.KillFlatten 本期只读不实现：
		// 是否市价平仓由人工决定（config 注释已声明），dashboard 不加平仓按钮。
		// ① journal 恢复 + ② 在途单认领
		adopted, rerr := recoverExecutor(context.Background(), a.exec,
			filepath.Join(cfg.DataDir, "state", "orders.jsonl"), cfg.Exchange.InstID)
		if rerr != nil {
			return nil, rerr
		}
		if adopted > 0 {
			log.Printf("启动恢复：认领交易所在途单 %d 张（孤儿单已并入本地订单簿）", adopted)
			a.notifier.Send(fmt.Sprintf("启动恢复：认领交易所在途挂单 %d 张（重启前的挂单已重新接管，请核对）", adopted))
		}
		if cerr := a.exec.CompactJournal(); cerr != nil {
			log.Printf("execution: journal 启动压缩失败（不影响交易，下次启动重试）: %v", cerr)
		}
		// ③ 余额拉取（T1）：必须在 AdoptOpenOrders（T0）之后——(T0,T1] 窗口内的
		// 成交已体现在 T1 余额里，随后清零账本以余额为唯一权威重建，无丢失窗口。
		// 失败则不进入交易流程。
		balances, berr := ex.GetBalances(context.Background())
		if berr != nil {
			return nil, fmt.Errorf("账户初始化失败: %w", berr)
		}
		// ④ Seed 账本：先清掉认领阶段打到未初始化账本上的成交增量——
		// 停机期间的成交已包含在交易所余额里，Seed 以余额为唯一权威重建账本。
		pf.Reset()
		mark := fetchMark(ex, cfg.Exchange.InstID)
		pf.Seed(balances, cfg.Exchange.InstID, strings.Split(cfg.Exchange.InstID, "-")[0], "USDT", mark)
		if mark > 0 {
			// 复用收盘根更新 mark 的同一入口（pf.UpdateMark），让权益基线即刻含持仓市值
			// ——此前 mark=0 导致 Seed 后权益只有现金，日内回撤基线口径漂移。
			pf.UpdateMark(cfg.Exchange.InstID, mark)
		}
		// ⑤ 日内权益基线跨重启：同日延续旧基线（当日亏损不因重启遗忘），跨日重置。
		today := time.Now().UTC().Format("2006-01-02")
		if stErr == nil && st.Day == today && st.DayStartEq > 0 {
			rk.SetDayStartEquity(st.DayStartEq)
			log.Printf("日内权益基线延续（%s 保存值 %.2f）", st.Day, st.DayStartEq)
		} else {
			rk.SetDayStartEquity(pf.Equity())
		}
		// ⑥ 强制账户对账：不阻断进程（dashboard 可看、人工介入），差异时下单被
		// RECONCILE_BLOCK 拦截。
		a.reconcileOnce(context.Background())
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
	rk.Kill.OnTrip(func(reason string) {
		a.notifier.Send(fmt.Sprintf("Kill Switch 触发：%s（已自动撤单，人工复位前禁止一切新下单）", reason))
	})
	// 运行态恢复应用：游标（防重启重复驱动策略）、试验计数、Kill Switch 状态
	if stErr == nil {
		a.mu.Lock()
		a.lastCandle = st.LastCandle
		a.trials = st.Trials
		a.mu.Unlock()
		if st.KillTripped {
			rk.Kill.Restore(true, st.KillReason)
			log.Printf("Kill Switch 处于触发状态（%s），继续停机", st.KillReason)
			// RISK-4：Kill 态跨重启恢复补撤单——上次停机 OnTrip 链的撤单可能没执行
			// 完（进程被杀），Kill 语义是"无挂单在场"。research（无执行器）跳过。
			a.killRestoreCancel(st.KillReason)
		}
		a.restoreStrategyState(st.StrategyState)
		a.activeMode.Store(string(cfg.Mode)) // 活跃环境初始 = 启动配置（页面可降级；升级受门禁）
		if len(st.UMP) > 0 && a.umpFilter != nil {
			snap := make(map[[3]int][2]int, len(st.UMP))
			for _, c := range st.UMP {
				snap[c.Key] = [2]int{c.Wins, c.Total}
			}
			a.umpFilter.Restore(snap)
			log.Printf("UMP 拦截器统计已恢复（%d 个情境，%d 笔样本）", len(st.UMP), a.umpFilter.Total())
		}
	} else {
		a.activeMode.Store(string(cfg.Mode))
	}
	// 交易模式启动即落盘一次：日内基线/Kill/游标尽快持久化（防启动后立刻崩溃丢失）。
	if cfg.Mode != config.ModeResearch {
		a.persistState()
	}
	return a, nil
}

// abs64 绝对值（时钟偏移阈值判断用）。
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// recoverExecutor 启动恢复编排（纯函数便于测试）：挂载 journal → 重放事件日志重建
// 订单簿（orders/byClient/claimed 三张表）→ 认领交易所在途单（孤儿单并入、已知单对齐）。
// journal 不存在视为全新启动（LoadJournal 返回空）；损坏行跳过计数仅告警（重放容错）。
func recoverExecutor(ctx context.Context, exec *execution.Executor, journalPath, symbol string) (adopted int, err error) {
	j, err := execution.NewJournal(journalPath)
	if err != nil {
		return 0, fmt.Errorf("journal 挂载失败: %w", err)
	}
	exec.AttachJournal(j)
	events, skipped, err := execution.LoadJournal(journalPath)
	if err != nil {
		return 0, fmt.Errorf("journal 读取失败: %w", err)
	}
	if skipped > 0 {
		log.Printf("execution: journal 有 %d 行损坏/空行被跳过（重放容错，不阻断恢复）", skipped)
	}
	exec.RestoreFromEvents(events)
	adopted, err = exec.AdoptOpenOrders(ctx, symbol)
	if err != nil {
		return adopted, fmt.Errorf("在途单认领失败: %w", err)
	}
	return adopted, nil
}

// fetchMark 拉真实 last 价作为 Seed 的 mark（权益基线口径）。失败回退 0 并告警，
// 不阻断启动（首根收盘后 UpdateMark 会修复）。
func fetchMark(ex exchange.Exchange, symbol string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tk, err := ex.GetTicker(ctx, symbol)
	if err != nil {
		log.Printf("ticker 拉取失败，Seed mark=0（权益基线降级为现金口径，待首根收盘修复）: %v", err)
		return 0
	}
	return tk.Last
}

// alertThrottle 告警节流：同类告警在 every 时间窗内只放行一次（日志不受限）。
// 零值可用（默认 10 分钟）。
type alertThrottle struct {
	mu    sync.Mutex
	last  time.Time
	every time.Duration
}

func (t *alertThrottle) allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	every := t.every
	if every <= 0 {
		every = 10 * time.Minute
	}
	if time.Since(t.last) >= every {
		t.last = time.Now()
		return true
	}
	return false
}

// reconcileDustUSDT 对账粉尘豁免：绝对差异 ≤0.5 USDT 等值忽略（防小额残渣误报刷告警）。
// 仅限 cash 类差异（计价币，天然 USDT 等值）；position/available 类按币数量计，
// 0.4 BTC ≈ 数万美元不得当粉尘放行——其浮点残差由内禀 0.1% 相对容差覆盖。
const reconcileDustUSDT = 0.5

// reconcileWithinTolerance 对账"判定 Ok"层：ReconcileDetail 内禀容差 0.1% 之外，
// 再按配置容差（ops.reconcile_tolerance_pct，默认 0.5%）过滤——Diffs 全部在配置
// 容差内（或属粉尘豁免）也算 Ok；任一差异超容差才判定不一致。
func reconcileWithinTolerance(rep portfolio.ReconcileReport, tolerancePct float64) bool {
	if rep.Ok {
		return true
	}
	if tolerancePct <= 0 || tolerancePct > 5 {
		tolerancePct = 0.5
	}
	for _, d := range rep.Diffs {
		if d.Kind == portfolio.DiffCash && math.Abs(d.Diff) <= reconcileDustUSDT {
			continue // 粉尘豁免（仅计价币）
		}
		ref := math.Max(math.Abs(d.Local), math.Abs(d.Remote))
		if ref <= 0 {
			return false // 双侧为零却报差异（不应出现），保守判不一致
		}
		if math.Abs(d.Diff)/ref*100 > tolerancePct {
			return false
		}
	}
	return true
}

// reconcileDiffSummary 对账差异摘要（告警正文 / 日志共用，最多列 5 项防刷屏）。
func reconcileDiffSummary(diffs []portfolio.ReconcileDiff) string {
	if len(diffs) == 0 {
		return "（差异明细为空）"
	}
	parts := make([]string, 0, len(diffs))
	for i, d := range diffs {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("…共 %d 项", len(diffs)))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s: 本地 %.8f vs 交易所 %.8f", d.Kind, d.Item, d.Local, d.Remote))
	}
	return strings.Join(parts, "; ")
}

// reconcileOnce 强制账户对账一次：拉取交易所余额 → ReconcileDetail 结构化比对 →
// 配置容差内一致 → ClearReconcileBlock；超容差 → BlockForReconcile（新下单被
// RECONCILE_BLOCK 拦截）+ 严重告警（10 分钟节流）。
// 拉取失败 fail-closed（RISK-3）：从未成功对账过（lastRec 零值）→ 直接拦截——
// 开始交易前必须先见过一次账本；成功过一次后短暂失败保持现状（宽容），连续失败
// ≥3 次再拦截。返回错误供 dashboard Kill 复位门禁使用。
func (a *app) reconcileOnce(ctx context.Context) (rep portfolio.ReconcileReport, ok bool, err error) {
	bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	balances, berr := a.ex.GetBalances(bctx)
	cancel()
	if berr != nil {
		berr = fmt.Errorf("拉取余额失败: %w", berr)
		fails := a.recFails.Add(1)
		if last := a.lastReconcileReport(); last.Ts.IsZero() {
			a.rk.BlockForReconcile("对账从未成功（" + berr.Error() + "）")
			log.Printf("账户对账从未成功，新下单被拦截（fail-closed，对账成功后自动解除）: %v", berr)
		} else if fails >= 3 {
			a.rk.BlockForReconcile(fmt.Sprintf("对账连续失败 %d 次（%v）", fails, berr))
			log.Printf("账户对账连续失败 %d 次，新下单被拦截: %v", fails, berr)
		} else {
			log.Printf("账户对账失败（连续 %d 次，保持现有拦截状态）: %v", fails, berr)
		}
		return a.lastReconcileReport(), false, berr
	}
	a.recFails.Store(0)
	rep = a.pf.ReconcileDetail(balances)
	ok = reconcileWithinTolerance(rep, a.cfg.Ops.ReconcileTolerancePct)
	a.recMu.Lock()
	a.lastRec = rep
	a.recMu.Unlock()
	if ok {
		if blocked, _ := a.rk.ReconcileBlocked(); blocked {
			log.Printf("账户对账恢复一致，解除下单拦截")
		}
		a.rk.ClearReconcileBlock()
		return rep, true, nil
	}
	summary := reconcileDiffSummary(rep.Diffs)
	a.rk.BlockForReconcile(summary)
	log.Printf("账户对账存在差异，新下单被 RECONCILE_BLOCK 拦截（对账恢复一致后自动解除，人工核查）：%s", summary)
	if a.recAlert.allow() {
		a.notifier.Send(fmt.Sprintf("🔴 账户对账差异（新下单已拦截）：%s", summary))
	}
	return rep, false, nil
}

// lastReconcileReport 最近一次对账报告快照（dashboard /api/status 数据源）。
func (a *app) lastReconcileReport() portfolio.ReconcileReport {
	a.recMu.Lock()
	defer a.recMu.Unlock()
	rep := a.lastRec
	rep.Diffs = append([]portfolio.ReconcileDiff(nil), a.lastRec.Diffs...)
	return rep
}

// reconcileLoop 账户对账循环：每 ops.reconcile_sec 对账一次（默认 300s）。
// 周期到点即对账，恢复一致自动解除拦截；差异持续则持续拦截 + 节流告警。
func (a *app) reconcileLoop(ctx context.Context) {
	every := time.Duration(a.cfg.Ops.ReconcileSec) * time.Second
	if every < time.Minute {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reconcileOnce(ctx)
		}
	}
}

// equityWatchdog 权益看门狗：每 ops.equity_watch_sec 巡检——先复用 GetTicker +
// pf.UpdateMark（与收盘根驱动同一 mark 更新入口，不新造口径）刷新持仓市值，
// 再 EvaluateDailyLoss 评估当日回撤；超限触发 Kill Switch（OnTrip 链自动撤单+告警，
// 此处只负责留痕日志——策略不出单时也独立巡检，不依赖下单路径）。
func (a *app) equityWatchdog(ctx context.Context) {
	every := time.Duration(a.cfg.Ops.EquityWatchSec) * time.Second
	if every < 5*time.Second {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			tk, err := a.ex.GetTicker(tctx, a.cfg.Exchange.InstID)
			cancel()
			if err != nil {
				if a.markAlert.allow() { // 失败日志同样节流，巡检周期短防刷屏
					log.Printf("权益看门狗：ticker 拉取失败（mark 暂不刷新，等下一轮）: %v", err)
				}
			} else if tk.Last > 0 {
				a.pf.UpdateMark(a.cfg.Exchange.InstID, tk.Last)
			}
			if tripped, reason := a.rk.EvaluateDailyLoss(); tripped {
				log.Printf("权益看门狗：当日回撤超限，Kill Switch 已触发（%s）", reason)
			}
		}
	}
}

// onFeedUpdate 行情恢复入口：连续失败计数清零 + 恢复留痕，再走正常收盘驱动。
func (a *app) onFeedUpdate(candles []exchange.Candle) {
	if n := a.feedErrs.Swap(0); n >= 3 {
		log.Printf("行情拉取恢复（此前连续失败 %d 次）", n)
	}
	a.onCandles(candles)
}

// onFeedError 行情拉取失败回调：连续失败计数；≥3 次告警（10 分钟节流）。
func (a *app) onFeedError(err error) {
	n := a.feedErrs.Add(1)
	log.Printf("行情拉取失败（连续 %d 次）: %v", n, err)
	if n >= 3 && a.feedAlert.allow() {
		a.notifier.Send(fmt.Sprintf("🟠 行情拉取连续失败 %d 次（最近错误：%v）——策略驱动已停滞，请检查网络/交易所状态", n, err))
	}
}

// onWSError WS 行情异常回调（断线重连提示等）：log 常开，notify 节流
// （WS 有自动重连 + REST 轮询兜底，属降级不属停摆）。
func (a *app) onWSError(err error) {
	log.Printf("ws 行情异常: %v", err)
	if a.wsAlert.allow() {
		a.notifier.Send(fmt.Sprintf("🟠 WS 行情异常：%v（自动重连中，REST 轮询兜底）", err))
	}
}

// cancelAllChecked 撤单 + 完整性核对（RISK-7）：撤单前记本地挂单数 n，CancelAll
// 返回实撤数 c，c < n 告警（CancelAll 内部拉取挂单失败同样表现为 c=0，n>0 时必告警，
// 不碰 execution 包即可在 main 层完成核对）。返回实撤数。
func (a *app) cancelAllChecked(ctx context.Context, scene string) int {
	n := len(a.exec.OpenOrders())
	c := a.exec.CancelAll(ctx, a.cfg.Exchange.InstID)
	if c < n {
		a.notifier.Send(fmt.Sprintf("🔴 %s撤单可能不完整：期望 %d 实撤 %d（请人工核对交易所挂单）", scene, n, c))
	}
	return c
}

// killRestoreCancel Kill 态跨重启恢复补撤单（RISK-4）：进程被杀时 OnTrip 链的撤单
// 可能没执行完，Kill 语义是"无挂单在场"——恢复即补撤一次并告警留痕；CancelAll
// 返回值无法区分"没单可撤"与"拉取失败"，一律推送人工兜底提示。撤单与本地挂单数
// 比对，不完整追加告警（RISK-7 同口径）。
func (a *app) killRestoreCancel(reason string) {
	if a.exec == nil { // research 未装配执行器：无交易无挂单，跳过
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	n := len(a.exec.OpenOrders())
	c := a.exec.CancelAll(ctx, a.cfg.Exchange.InstID)
	log.Printf("Kill 态跨重启恢复：已补撤挂单 %d/%d 张", c, n)
	a.notifier.Send(fmt.Sprintf("🔴 Kill 态跨重启恢复，已补撤挂单 %d 张；若撤单失败请人工处理（原因：%s）", c, reason))
	if c < n {
		a.notifier.Send(fmt.Sprintf("🔴 Kill 态恢复撤单可能不完整：期望 %d 实撤 %d（请人工核对交易所挂单）", n, c))
	}
}

// gracefulShutdown 优雅退出编排（P1-2，总预算 30s 超时保护）：
//  1. httpSrv.Shutdown（3s，httpSrv 可为 nil）最先执行：拒绝新请求——drain 期间
//     手动下单/切环境/复位 Kill 等写入口不再开放（RISK-6；Shutdown 会等 in-flight
//     handler 返回，新请求进不来）；
//  2. 停策略驱动——外层 ctx 已取消，Poller/WS/看门狗/对账循环/复盘循环自行退出，
//     不再产生新信号/新下单；
//  3. 可选撤单：ops.shutdown_cancel_orders=true（默认）且 paper/live → CancelAll
//     （10s 子预算）+ 完整性核对告警。false 的语义是"挂单留守在场，重启后由
//     AdoptOpenOrders 认领"（live 留守值守场景的人工选择）；
//  4. persistState：游标/试验数/Kill/UMP/日内基线最新态落盘；
//  5. exec.Stop()：停执行器订单回报同步循环。
//
// 退出码 0：drain 尽力完成，预算耗尽也正常返回（留痕）。
func (a *app) gracefulShutdown(httpSrv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log.Printf("收到退出信号，开始优雅退出（预算 30s）")
	if httpSrv != nil {
		hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
		_ = httpSrv.Shutdown(hctx)
		hcancel()
	}
	if a.cfg.Ops.ShutdownCancelOrders && a.exec != nil {
		cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
		c := a.cancelAllChecked(cctx, "优雅退出")
		ccancel()
		if c > 0 {
			log.Printf("优雅退出：已撤挂单 %d 张", c)
		}
	} else if a.exec != nil {
		log.Printf("优雅退出：shutdown_cancel_orders=false，挂单留守（重启后由 AdoptOpenOrders 认领）")
	}
	a.persistState()
	if a.exec != nil {
		a.exec.Stop()
	}
	log.Printf("优雅退出完成")
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
		// BUG-E：读 a.strat 持读锁取局部引用（锁范围尽量小）
		a.mu.RLock()
		cp, isComposite := a.strat.(*strategy.Composite)
		a.mu.RUnlock()
		if isComposite {
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
	// BUG-E：读 a.strat 持读锁取局部引用后使用（驱动用局部变量，不得在锁内跑策略）
	a.mu.RLock()
	strat := a.strat
	a.mu.RUnlock()
	for intentIndex, intent := range strat.OnCandle(sctx) {
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
		Cost:     a.costModel(),
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
	// BUG-E：读 a.strat 持读锁取局部引用
	a.mu.RLock()
	strat := a.strat
	a.mu.RUnlock()
	switch st := strat.(type) {
	case *grid.Grid:
		gs := st.Stats()
		in.Strategy = fmt.Sprintf("grid: rounds=%d realized=%.2f broke=%v position=%.4f | 市况 %s",
			gs.Rounds, gs.Realized, gs.Broke, gs.Position, a.regimeDet.Current())
	case *trend.Donchian:
		in.Strategy = fmt.Sprintf("trend: %s | 市况 %s", st.Describe(), a.regimeDet.Current())
	default:
		in.Strategy = fmt.Sprintf("%s | 市况 %s", strat.Name(), a.regimeDet.Current())
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

// costModelOf 统一成本模型入口（P1-8）：滑点 + 手续费全部走配置。
// cfg.FeeModel() 默认 OKX 现货 L1 保守口径 maker 8 / taker 10 bps，取代历史硬编码
// 2/5 的系统性低估；回测/research 路径同用配置值——保守化，回测成本假设变高是预期行为。
func costModelOf(cfg *config.Config) backtest.CostModel {
	makerBps, takerBps := cfg.FeeModel()
	return backtest.CostModel{SlippageBps: cfg.Trading.SlippageBps, MakerFeeBps: makerBps, TakerFeeBps: takerBps}
}

func (a *app) costModel() backtest.CostModel { return costModelOf(a.cfg) }

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
	// 断流告警（P0-5）：连续失败计数 + 节流告警；恢复（下一次成功拉取）清零留痕
	a.pol.OnUpdate = a.onFeedUpdate
	a.pol.OnError = a.onFeedError
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
	// WS 异常/断线走统一断流告警（log 常开 + notify 节流）。
	go okx.NewWSCandles(cfg.Exchange.InstID, cfg.Trading.Interval).WithHandler(
		func(ts int64) {
			if _, err := a.pol.FetchOnce(ctx, 300); err != nil {
				a.onFeedError(err) // 与轮询同一失败计数流（等下一轮轮询兜底）
			}
		},
		a.onWSError,
	).Run(ctx)

	if a.exec != nil {
		go a.exec.ReconcileLoop()
		// 权益看门狗 + 账户对账循环（paper/live）：ctx 取消即退出（优雅退出的一部分）
		go a.equityWatchdog(ctx)
		go a.reconcileLoop(ctx)
	}

	// 小时级复盘：停下来 → 生成记录落盘 → 恢复交易（失败留痕不中断进程）；
	// 每 24 个复盘周期聚合一次风控日报推送（Kriss 数据面，docs/09）
	go func() {
		ticker := time.NewTicker(a.reviewer.Every())
		defer ticker.Stop()
		cycles := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cycles++
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
				if cycles%24 == 0 {
					a.notifier.Send(review.DailyDigest(a.reviewer.Recent(24)))
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
			selector = lab.FixedSelector(func() strategy.Strategy { return a.currentGrid() }, "grid:config")
		default:
			return nil, fmt.Errorf("未知策略 %q（支持 trend / grid）", strategyName)
		}
		return lab.WalkForward(candles, lab.WFConfig{
			TrainBars: train, TestBars: test, SeedCash: 10000,
			Cost:   a.costModel(),
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
			mk = func() strategy.Strategy { return a.currentGrid() }
		default:
			return 0, nil, fmt.Errorf("未知策略 %q", strategyName)
		}
		return lab.UMPCheck(candles, a.costModel(),
			10000, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, mk, ump.DefaultMinWinRate, minSamples)
	}
	var orderSrc dashboard.OrderSource = dashboard.NoopExecutor{}
	if a.exec != nil {
		orderSrc = a.exec
	}
	srv := dashboard.New(cfg, a.pf, a.rk, orderSrc, a.currentGrid, func() []exchange.Candle {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return append([]exchange.Candle(nil), a.candles...)
	}, runBacktest)
	srv.RecentReviews = a.reviewer.Recent
	srv.RunWalkForward = runWF
	srv.RunUMPCheck = runUMP
	srv.RunPlateau = a.RunPlateau
	srv.RunCostScan = a.RunCostScan
	srv.CurrentStrategy = a.CurrentStrategy
	srv.SwitchStrategy = a.SwitchStrategy
	srv.OrderBook = func() *exchange.OrderBook {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ob, err := a.ex.GetOrderBook(ctx, cfg.Exchange.InstID, 50); err == nil {
			return &ob
		}
		return nil
	}
	if a.exec != nil {
		srv.Fills = func() []execution.Event {
			var out []execution.Event
			for _, ev := range a.exec.Events(500) {
				if ev.DeltaQty > 0 {
					out = append(out, ev)
				}
			}
			return out
		}
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
	// 对账数据面（P1-1）：Kill 复位前置对账门禁 + /api/status 对账状态展示。
	// research（无执行器/无账户）不注入，dashboard 退回原语义。
	if a.exec != nil {
		srv.Reconcile = func() (portfolio.ReconcileReport, bool, error) {
			return a.reconcileOnce(context.Background())
		}
		srv.LastReconcile = a.lastReconcileReport
	}

	if !cfg.Dashboard.Enabled {
		log.Printf("mode=%s symbol=%s（后台未启用）", cfg.Mode, cfg.Exchange.InstID)
		<-ctx.Done()
		a.gracefulShutdown(nil)
		return nil
	}
	httpSrv := &http.Server{Addr: cfg.Dashboard.Listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		// 恢复默认信号语义：drain 期间再收到 SIGTERM/SIGINT 立即强杀（人工兜底，
		// 不必等 30s 预算耗尽）
		stop()
		a.gracefulShutdown(httpSrv)
	}()
	log.Printf("QuantForge %s mode=%s symbol=%s 后台 http://%s",
		"v0.1.0", cfg.Mode, cfg.Exchange.InstID, cfg.Dashboard.Listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// restoreStrategyState 策略运行态恢复（T6）：持久化快照非空且当前策略实现
// strategy.Stateful → ImportState 延续运行态（如网格 lastIdx，防重启后重复挂格）。
// 失败路径全部"留痕 + 冷启动"，绝不阻断进程：
//   - 策略不支持 Stateful：跳过并留痕（冷启动语义）；
//   - 导入失败（含配置漂移 / 版本不兼容 / 数据损坏）：警告并冷启动，由调用方继续。
//
// 防前视：快照与游标同根持久化（persistState 在收盘根驱动后调用），只含
// "截至上一根已收盘 K 线"的运行态，不引入未来数据。
func (a *app) restoreStrategyState(raw json.RawMessage) {
	if len(raw) == 0 {
		return // 首次启动 / 老版本 state 文件：无快照，正常冷启动（不留痕防刷屏）
	}
	// BUG-E：读 a.strat 持读锁取局部引用（exec 回调可能与恢复并发）
	a.mu.RLock()
	strat := a.strat
	a.mu.RUnlock()
	s, ok := strat.(strategy.Stateful)
	if !ok {
		log.Printf("策略不支持状态恢复，将冷启动（%s）", strat.Name())
		return
	}
	if err := s.ImportState(raw); err != nil {
		log.Printf("策略运行态恢复失败，已冷启动（状态与当前配置可能不一致，人工核对）: %v", err)
		return
	}
	log.Printf("策略运行态已恢复（%s）", strat.Name())
}

// persistState 运行态落盘（游标/试验数/Kill/UMP/日内权益基线/策略运行态）；
// 失败留痕不影响主流程。
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
	strat := a.strat
	a.mu.RUnlock()
	// 日内权益基线随每次落盘保存（跨重启延续用；research 无账户时为 0，恢复侧忽略）
	if day, eq := a.rk.EquityBaseline(); day != "" {
		st.Day, st.DayStartEq = day, eq
	}
	// 策略运行态快照（T6）：实现 Stateful 才导出；导出失败留痕（保留旧快照，
	// 下次落盘重试——比写入半旧状态更保守）。热切换策略后 strat 已是新实例，
	// 导出的是新策略当前（冷）态，语义正确：切换即 deliberate 冷启动。
	if s, ok := strat.(strategy.Stateful); ok {
		if raw, err := s.ExportState(); err != nil {
			log.Printf("策略运行态导出失败（本次落盘保留旧快照）: %v", err)
		} else {
			st.StrategyState = raw
		}
	}
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
// grid/both 分支一律新建 grid 实例（RISK-9）：切换即冷启动——复用旧实例会让旧
// lastIdx 复活引发追赶单；a.grid 同步替换，dashboard（GridFn）/研究入口自动跟随。
func (a *app) SwitchStrategy(name string) error {
	var s strategy.Strategy
	var g *grid.Grid
	switch name {
	case "grid":
		ng, err := grid.New(grid.Params{
			Lower: a.cfg.Strategy.Grid.Lower, Upper: a.cfg.Strategy.Grid.Upper,
			Grids: a.cfg.Strategy.Grid.Grids, QtyPerGrid: a.cfg.Strategy.Grid.QtyPerGrid,
			Spacing: a.cfg.Strategy.Grid.Spacing, StopOnBreak: a.cfg.Strategy.Grid.StopOnBreak,
		})
		if err != nil {
			return err
		}
		s, g = ng, ng
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
		ng, err := grid.New(grid.Params{
			Lower: a.cfg.Strategy.Grid.Lower, Upper: a.cfg.Strategy.Grid.Upper,
			Grids: a.cfg.Strategy.Grid.Grids, QtyPerGrid: a.cfg.Strategy.Grid.QtyPerGrid,
			Spacing: a.cfg.Strategy.Grid.Spacing, StopOnBreak: a.cfg.Strategy.Grid.StopOnBreak,
		})
		if err != nil {
			return err
		}
		// 组合：grid+trend 按权重分资金（regime 路由默认关——证据纪律）
		s = strategy.NewComposite([]string{"grid", "trend"}, []strategy.Strategy{ng, tr},
			[]float64{a.cfg.Strategy.Both.GridWeight, a.cfg.Strategy.Both.TrendWeight})
		g = ng
	default:
		return fmt.Errorf("未知策略 %q（支持 grid / trend / both）", name)
	}
	_, positions, _ := a.pf.Snapshot()
	for _, p := range positions {
		if p.Qty != 0 {
			return fmt.Errorf("持仓中禁止切换策略（%s 数量 %v，先平仓再切）", p.Symbol, p.Qty)
		}
	}
	// 写锁与 persistState/各读点配对；grid 实例一并替换（dashboard GridFn 取当前实例）
	a.mu.Lock()
	a.strat = s
	if g != nil {
		a.grid = g
	}
	a.mu.Unlock()
	return nil
}

// CurrentStrategy 当前策略名与描述。
func (a *app) CurrentStrategy() string {
	// BUG-E：读 a.strat 持读锁取局部引用（与 SwitchStrategy 写锁配对）
	a.mu.RLock()
	strat := a.strat
	a.mu.RUnlock()
	if d, ok := strat.(interface{ Describe() string }); ok {
		return d.Describe()
	}
	return strat.Name()
}

// currentGrid 当前 grid 实例快照（RISK-9：策略热切换会替换 grid 实例，
// dashboard/研究入口统一经此取"当前"实例，读侧必须持锁）。
func (a *app) currentGrid() *grid.Grid {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.grid
}

// RunPlateau 参数邻域高原检验（研究工作台）。窗口=最近 3000 根。
func (a *app) RunPlateau(ctx context.Context, strategyName string) (*lab.PlateauReport, error) {
	all := a.fetchCandles(ctx, 600)
	if len(all) < 300 {
		return nil, fmt.Errorf("样本 %d 根不足 300", len(all))
	}
	// 敏感性检验用最近 3000 根（全历史 7 万根 ×3 点回测超出页面 120s 预算；结论按窗口标注）
	if len(all) > 3000 {
		all = all[len(all)-3000:]
	}
	candles := all
	cost := a.costModel()
	mk := func(label string) strategy.Strategy {
		switch strategyName {
		case "grid": // 网格密度邻域：Grids±2（区间不变）
			p := grid.Params{
				Lower: a.cfg.Strategy.Grid.Lower, Upper: a.cfg.Strategy.Grid.Upper,
				Grids: a.cfg.Strategy.Grid.Grids, QtyPerGrid: a.cfg.Strategy.Grid.QtyPerGrid,
				Spacing: a.cfg.Strategy.Grid.Spacing, StopOnBreak: a.cfg.Strategy.Grid.StopOnBreak,
			}
			switch label {
			case "entry-2":
				p.Grids -= 2
			case "entry+2":
				p.Grids += 2
			}
			if p.Grids < 2 {
				p.Grids = 2
			}
			g, err := grid.New(p)
			if err != nil {
				return a.currentGrid()
			}
			return g
		default: // trend：EntryN 邻域 ±2（其余默认）
			p := trend.Params{
				EntryN: a.cfg.Strategy.Trend.EntryN, ExitN: a.cfg.Strategy.Trend.ExitN,
				AtrN: a.cfg.Strategy.Trend.AtrN, AtrMult: a.cfg.Strategy.Trend.AtrMult,
				RiskPct: a.cfg.Strategy.Trend.RiskPct, MaxPosPct: a.cfg.Strategy.Trend.MaxPosPct,
			}
			switch label {
			case "entry-2":
				p.EntryN -= 2
			case "entry+2":
				p.EntryN += 2
			}
			if p.EntryN < 2 {
				p.EntryN = 2
			}
			s, _ := trend.New(p)
			return s
		}
	}
	labels := []string{"base", "entry-2", "entry+2"}
	points, _, err := lab.ScanParams(candles, cost, 10000, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, mk, labels...)
	if err != nil {
		return nil, err
	}
	return lab.PlateauCheck(points, 0.5)
}

// RunCostScan 成本敏感性扫描（研究工作台）。窗口=最近 3000 根。
func (a *app) RunCostScan(ctx context.Context, strategyName string) ([]lab.CostPoint, error) {
	all := a.fetchCandles(ctx, 600)
	if len(all) < 300 {
		return nil, fmt.Errorf("样本 %d 根不足 300", len(all))
	}
	if len(all) > 3000 {
		all = all[len(all)-3000:]
	}
	candles := all
	var mk func() strategy.Strategy
	switch strategyName {
	case "grid":
		mk = func() strategy.Strategy { return a.currentGrid() }
	default:
		mk = func() strategy.Strategy {
			s, _ := trend.New(trend.Params{
				EntryN: a.cfg.Strategy.Trend.EntryN, ExitN: a.cfg.Strategy.Trend.ExitN,
				AtrN: a.cfg.Strategy.Trend.AtrN, AtrMult: a.cfg.Strategy.Trend.AtrMult,
				RiskPct: a.cfg.Strategy.Trend.RiskPct, MaxPosPct: a.cfg.Strategy.Trend.MaxPosPct,
			})
			return s
		}
	}
	pts, _, err := lab.CostScan(candles, a.costModel(),
		10000, a.cfg.Exchange.InstID, a.cfg.Trading.Interval, mk, 0, 0.5, 1, 2, 4)
	return pts, err
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
		Cost:     a.costModel(),
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
		Cost:   costModelOf(cfg),
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
	// 超时按拉取量估算：远古分页明显变慢（OKX history 对 2018-2019 段限流严格），
	// 每页按 2s 预算；中断不丢数据（每页增量入 SQLite，重跑自动续拉）。
	timeout := time.Duration(*bars/100) * 2 * time.Second
	if timeout < 5*time.Minute {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	db, derr := candlestore.Open(cfg.DataDir)
	if derr != nil {
		return fmt.Errorf("K 线库打开失败: %w", derr)
	}
	defer db.Close()
	// 断点续拉：库中已有部分数据且不足目标时，从最老一根继续向历史方向拉
	var afterFrom int64
	if existing, err := db.Latest(cfg.Exchange.Name, cfg.Exchange.InstID, cfg.Trading.Interval, 1<<30); err == nil && len(existing) > 0 && len(existing) < *bars {
		afterFrom = existing[0].OpenTime
		if afterFrom > 0 {
			fmt.Printf("断点续拉：库中已有 %d 根，从 %s 继续向历史方向拉取\n",
				len(existing), time.UnixMilli(afterFrom).Format("2006-01-02 15:04"))
		}
	}
	raw, err := client.GetCandlesHistoryFrom(ctx, cfg.Exchange.InstID, cfg.Trading.Interval, afterFrom, func(page []exchange.Candle) error {
		return db.Upsert(page)
	})
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
	if n, _ := db.Count(cfg.Exchange.Name, cfg.Exchange.InstID, cfg.Trading.Interval); n > 0 {
		fmt.Printf("库中累计：%d 根 %s K 线（data/candles.db）\n", n, cfg.Trading.Interval)
	}
	if len(clean) == 0 {
		return nil
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
	cost := costModelOf(cfg)
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
