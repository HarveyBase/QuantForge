// Package config 加载与校验 QuantForge 配置。
// 密钥一律走环境变量（OKX_API_KEY / OKX_SECRET / OKX_PASSPHRASE），不入配置文件、不入库。
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/HarveyBase/QuantForge/market"
)

// Mode 交易阶段分级：research(回测) / paper(模拟盘) / live(实盘)。
// 升级到更高级别必须过门槛并由人显式开启，代码不得自行切换。
type Mode string

const (
	ModeResearch Mode = "research"
	ModePaper    Mode = "paper"
	ModeLive     Mode = "live"
)

// LiveGateEnv 实盘门禁环境变量：live 模式必须显式设置该值才允许启动真实下单。
const LiveGateEnv = "QUANTFORGE_ALLOW_LIVE"

// LiveGateValue 门禁确认值，要求操作者亲手输入风险确认。
const LiveGateValue = "I_UNDERSTAND_THE_RISK"

// OKX 现货 L1 档手续费（bps）：maker 8 / taker 10。
// 保守口径：宁可高估成本也不让回测偏乐观（历史版本硬编码 2/5 系统性低估）。
const (
	DefaultMakerFeeBps = 8
	DefaultTakerFeeBps = 10
)

type Config struct {
	Mode      Mode            `json:"mode"`
	Exchange  ExchangeConfig  `json:"exchange"`
	Trading   TradingConfig   `json:"trading"`
	Risk      RiskConfig      `json:"risk"`
	Strategy  StrategyConfig  `json:"strategy"`
	Ump       UmpConfig       `json:"ump"`
	Dashboard DashboardConfig `json:"dashboard"`
	Ops       OpsConfig       `json:"ops"`
	DataDir   string          `json:"data_dir"`
}

type ExchangeConfig struct {
	Name     string  `json:"name"`     // 交易所：okx（binance 二期）
	Market   string  `json:"market"`   // SPOT | SWAP
	InstID   string  `json:"inst_id"`  // 如 BTC-USDT / BTC-USDT-SWAP
	RestURL  string  `json:"rest_url"` // 默认 https://www.okx.com
	TdMode   string  `json:"td_mode"`  // 合约下单模式：cross | isolated；现货固定 cash
	Leverage float64 `json:"leverage"` // 合约杠杆，现货忽略
}

type TradingConfig struct {
	Interval    string     `json:"interval"`     // K线周期：1m/5m/15m/30m/1H/4H/1D
	SlippageBps float64    `json:"slippage_bps"` // 回测/模拟撮合滑点（基点）
	Fees        FeesConfig `json:"fees"`         // 手续费模型（基点），默认 OKX 现货 L1 保守口径
}

// FeesConfig 手续费模型（单位 bps）。默认 8/10 = OKX 现货 L1 档（保守口径：
// 宁可高估成本也不让回测系统性偏乐观）。老配置缺该节时回填默认值。
type FeesConfig struct {
	MakerBps float64 `json:"maker_bps"` // 挂单手续费（bps）
	TakerBps float64 `json:"taker_bps"` // 吃单手续费（bps）
}

type RiskConfig struct {
	MaxOrderNotionalUSD    float64 `json:"max_order_notional_usd"`
	MaxDailyNotionalUSD    float64 `json:"max_daily_notional_usd"`
	MaxPositionNotionalUSD float64 `json:"max_position_notional_usd"`
	MaxOrdersPerMinute     int     `json:"max_orders_per_minute"`
	MaxDailyLossPct        float64 `json:"max_daily_loss_pct"`
	CooldownAfterRejectSec int     `json:"cooldown_after_reject_sec"`
}

type StrategyConfig struct {
	Name  string      `json:"name"` // grid | trend | both（组合）
	Grid  GridConfig  `json:"grid"`
	Trend TrendConfig `json:"trend"`
	// BothConfig 组合模式：grid+trend 并行按权重分资金。
	Both BothConfig `json:"both"`
}

type BothConfig struct {
	GridWeight  float64 `json:"grid_weight"`  // grid 资金权重（0-1]
	TrendWeight float64 `json:"trend_weight"` // trend 资金权重（0-1]
	// RegimeRoute 按 regime 自动路由（趋势市只跑 trend、震荡市只跑 grid）。
	// 默认 false：docs/10 §6 互补性在真实样本不成立，无 OOS 证据不开自动切换。
	RegimeRoute bool `json:"regime_route"`
}

// TrendConfig 趋势策略参数（docs/10 假设卡口径）。
type TrendConfig struct {
	EntryN    int     `json:"entry_n"`     // 入场突破窗口
	ExitN     int     `json:"exit_n"`      // 出场通道窗口
	AtrN      int     `json:"atr_n"`       // ATR 周期
	AtrMult   float64 `json:"atr_mult"`    // 跟踪止损倍数
	RiskPct   float64 `json:"risk_pct"`    // 单笔风险比例
	MaxPosPct float64 `json:"max_pos_pct"` // 单笔仓位上限
}

type GridConfig struct {
	Lower       float64 `json:"lower"`         // 网格下界
	Upper       float64 `json:"upper"`         // 网格上界
	Grids       int     `json:"grids"`         // 格数（≥2）
	QtyPerGrid  float64 `json:"qty_per_grid"`  // 每格数量（币本位）
	Spacing     string  `json:"spacing"`       // arith(等差) | geo(等比)
	StopOnBreak bool    `json:"stop_on_break"` // 下界打穿停止补格并告警
}

// UmpConfig UMP 信号拦截器配置（grid 版已过样本外验证 docs/10 §5B；
// 拦截只减少下单不增加风险，故默认开启）。
type UmpConfig struct {
	Enabled bool `json:"enabled"`
}

type DashboardConfig struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"` // 默认 127.0.0.1:8080，只绑本机
	Token   string `json:"token"`  // 可选 Bearer Token；为空则仅本机访问
}

// OpsConfig 运行保障参数（权益看门狗、账户对账、REST 限速、优雅退出与 Kill Switch 行为）。
// 供 serve/paper/live 各模式的常驻编排消费；老配置缺省时全部回填保守默认值。
type OpsConfig struct {
	EquityWatchSec        int     `json:"equity_watch_sec"`        // 权益看门狗巡检周期（秒），默认 30
	ReconcileSec          int     `json:"reconcile_sec"`           // 账户对账周期（秒），默认 300（5 分钟）
	ReconcileTolerancePct float64 `json:"reconcile_tolerance_pct"` // 对账容差百分比（%），默认 0.5
	RestRateLimitPerSec   int     `json:"rest_rate_limit_per_sec"` // 适配器 REST 限速（req/s），默认 10
	// ShutdownCancelOrders 优雅退出时是否撤掉所有挂单，默认 true（live 留守值守可关）。
	ShutdownCancelOrders bool `json:"shutdown_cancel_orders"`
	// KillFlatten Kill Switch 触发时是否市价平仓，默认 false（仅撤单——
	// 极端行情市价平仓有滑点风险，是否平仓由人工决定）。
	KillFlatten bool `json:"kill_flatten"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Mode: ModeResearch,
		Exchange: ExchangeConfig{
			Name:    "okx",
			Market:  "SPOT",
			InstID:  "BTC-USDT",
			RestURL: "https://www.okx.com",
			TdMode:  "cross",
		},
		Trading: TradingConfig{
			Interval:    "1H",
			SlippageBps: 5,
			Fees:        FeesConfig{MakerBps: DefaultMakerFeeBps, TakerBps: DefaultTakerFeeBps},
		},
		Risk: RiskConfig{
			MaxOrderNotionalUSD:    1000,
			MaxDailyNotionalUSD:    10000,
			MaxPositionNotionalUSD: 5000,
			MaxOrdersPerMinute:     10,
			MaxDailyLossPct:        5,
			CooldownAfterRejectSec: 30,
		},
		Strategy: StrategyConfig{
			Name:  "grid",
			Trend: TrendConfig{EntryN: 20, ExitN: 10, AtrN: 14, AtrMult: 2, RiskPct: 0.005, MaxPosPct: 0.5},
			Both:  BothConfig{GridWeight: 0.5, TrendWeight: 0.3, RegimeRoute: false},
			Grid: GridConfig{
				Lower: 40000, Upper: 80000, Grids: 20,
				QtyPerGrid: 0.001, Spacing: "geo", StopOnBreak: true,
			},
		},
		Ump:       UmpConfig{Enabled: true},
		Dashboard: DashboardConfig{Enabled: true, Listen: "127.0.0.1:8080"},
		Ops: OpsConfig{
			EquityWatchSec:        30,
			ReconcileSec:          300,
			ReconcileTolerancePct: 0.5,
			RestRateLimitPerSec:   10,
			ShutdownCancelOrders:  true,
			KillFlatten:           false,
		},
		DataDir: "data",
	}
}

// Load 从 path 读取 JSON 配置，缺省字段回填默认值并做校验。
func Load(path string) (*Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config: %s 不存在（可从 config.example.json 复制）: %w", path, err)
		}
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.CheckLiveGate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 校验业务约束，返回第一个错误。
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeResearch, ModePaper, ModeLive:
	default:
		return fmt.Errorf("config: mode 必须是 research/paper/live，当前 %q", c.Mode)
	}
	if c.Exchange.Name != "okx" {
		return fmt.Errorf("config: exchange.name 当前仅支持 okx（binance 二期），当前 %q", c.Exchange.Name)
	}
	switch c.Exchange.Market {
	case "SPOT", "SWAP":
	default:
		return fmt.Errorf("config: exchange.market 必须是 SPOT 或 SWAP，当前 %q", c.Exchange.Market)
	}
	if c.Exchange.InstID == "" {
		return fmt.Errorf("config: exchange.inst_id 不能为空")
	}
	if c.Exchange.RestURL == "" {
		return fmt.Errorf("config: exchange.rest_url 不能为空")
	}
	u, err := url.Parse(c.Exchange.RestURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("config: exchange.rest_url 必须是合法 HTTPS 地址")
	}
	if c.Mode != ModeResearch && c.Exchange.Market == "SWAP" {
		return fmt.Errorf("config: SWAP 永续当前仅支持 research，paper/live 为安全禁用")
	}
	if c.Trading.SlippageBps < 0 || c.Trading.SlippageBps > 1000 {
		return fmt.Errorf("config: trading.slippage_bps 必须在 [0,1000]")
	}
	if market.IntervalMs(c.Trading.Interval) == 0 {
		return fmt.Errorf("config: trading.interval 不支持 %q", c.Trading.Interval)
	}
	if c.Trading.Fees.MakerBps <= 0 || c.Trading.Fees.MakerBps >= 200 ||
		c.Trading.Fees.TakerBps <= 0 || c.Trading.Fees.TakerBps >= 200 {
		return fmt.Errorf("config: trading.fees.maker_bps/taker_bps 必须均在 (0,200)（单位 bps，默认 OKX 现货 L1 %d/%d 保守口径），当前 maker=%v taker=%v",
			DefaultMakerFeeBps, DefaultTakerFeeBps, c.Trading.Fees.MakerBps, c.Trading.Fees.TakerBps)
	}
	if c.Exchange.Market == "SWAP" {
		if c.Exchange.Leverage < 1 || c.Exchange.Leverage > 10 {
			return fmt.Errorf("config: 杠杆必须在 [1,10]（风险纪律：禁高杠杆），当前 %v", c.Exchange.Leverage)
		}
		switch c.Exchange.TdMode {
		case "cross", "isolated":
		default:
			return fmt.Errorf("config: exchange.td_mode 必须是 cross/isolated，当前 %q", c.Exchange.TdMode)
		}
	}
	if c.Risk.MaxOrderNotionalUSD <= 0 || c.Risk.MaxDailyNotionalUSD <= 0 ||
		c.Risk.MaxPositionNotionalUSD <= 0 || c.Risk.MaxOrdersPerMinute <= 0 {
		return fmt.Errorf("config: risk 限额必须全部为正（风控前置，不允许无限制下单）")
	}
	if c.Risk.MaxDailyLossPct <= 0 || c.Risk.MaxDailyLossPct > 100 {
		return fmt.Errorf("config: risk.max_daily_loss_pct 必须在 (0,100]")
	}
	if c.Risk.CooldownAfterRejectSec < 0 {
		return fmt.Errorf("config: cooldown_after_reject_sec 不能为负")
	}
	if c.Ops.EquityWatchSec < 5 {
		return fmt.Errorf("config: ops.equity_watch_sec 必须 ≥5（巡检太稀疏会漏掉权益异常），当前 %d", c.Ops.EquityWatchSec)
	}
	if c.Ops.ReconcileSec < 60 {
		return fmt.Errorf("config: ops.reconcile_sec 必须 ≥60（对账过频会撞 REST 限速），当前 %d", c.Ops.ReconcileSec)
	}
	if c.Ops.ReconcileTolerancePct <= 0 || c.Ops.ReconcileTolerancePct > 5 {
		return fmt.Errorf("config: ops.reconcile_tolerance_pct 必须在 (0,5]（单位百分比），当前 %v", c.Ops.ReconcileTolerancePct)
	}
	if c.Ops.RestRateLimitPerSec < 1 || c.Ops.RestRateLimitPerSec > 50 {
		return fmt.Errorf("config: ops.rest_rate_limit_per_sec 必须在 [1,50]，当前 %d", c.Ops.RestRateLimitPerSec)
	}
	switch c.Strategy.Name {
	case "grid", "trend", "both":
	default:
		return fmt.Errorf("config: strategy.name 必须是 grid/trend，当前 %q", c.Strategy.Name)
	}
	g := c.Strategy.Grid
	if c.Strategy.Name == "grid" {
		if g.Lower <= 0 || g.Upper <= g.Lower {
			return fmt.Errorf("config: grid 需要 0 < lower < upper，当前 %v~%v", g.Lower, g.Upper)
		}
		if g.Grids < 2 {
			return fmt.Errorf("config: grid.grids 必须 ≥2，当前 %d", g.Grids)
		}
		if g.QtyPerGrid <= 0 {
			return fmt.Errorf("config: grid.qty_per_grid 必须为正")
		}
		switch g.Spacing {
		case "arith", "geo":
		default:
			return fmt.Errorf("config: grid.spacing 必须是 arith/geo，当前 %q", g.Spacing)
		}
	}
	if c.Strategy.Name == "both" {
		b := c.Strategy.Both
		if b.GridWeight <= 0 || b.GridWeight > 1 || b.TrendWeight <= 0 || b.TrendWeight > 1 {
			return fmt.Errorf("config: strategy.both 权重必须在 (0,1]（权重和 <1 时剩余为现金缓冲——永不满仓）")
		}
	}
	return nil
}

// FeeModel 返回手续费模型（maker/taker，单位 bps），供成本模型统一取数。
// Load 回填默认值后总是非零；即便手工构造的 Config 漏设费率，
// 也回退到保守默认 8/10，绝不让成本被低估为零。
func (c Config) FeeModel() (makerBps, takerBps float64) {
	makerBps, takerBps = c.Trading.Fees.MakerBps, c.Trading.Fees.TakerBps
	if makerBps <= 0 {
		makerBps = DefaultMakerFeeBps
	}
	if takerBps <= 0 {
		takerBps = DefaultTakerFeeBps
	}
	return makerBps, takerBps
}

func (c *Config) Sanitized() *Config {
	copyCfg := *c
	copyCfg.Dashboard = c.Dashboard
	copyCfg.Dashboard.Token = ""
	return &copyCfg
}

// CheckLiveGate 实盘门禁：mode=live 时必须显式设置确认环境变量。
func (c *Config) CheckLiveGate() error {
	if c.Mode != ModeLive {
		return nil
	}
	if v := strings.TrimSpace(os.Getenv(LiveGateEnv)); v != LiveGateValue {
		return fmt.Errorf("config: mode=live 需要环境变量 %s=%s 确认（实盘有真实资金风险，晋级门槛见 docs/08）",
			LiveGateEnv, LiveGateValue)
	}
	return nil
}
