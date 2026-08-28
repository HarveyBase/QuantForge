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

type Config struct {
	Mode      Mode            `json:"mode"`
	Exchange  ExchangeConfig  `json:"exchange"`
	Trading   TradingConfig   `json:"trading"`
	Risk      RiskConfig      `json:"risk"`
	Strategy  StrategyConfig  `json:"strategy"`
	Ump       UmpConfig       `json:"ump"`
	Dashboard DashboardConfig `json:"dashboard"`
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
	Interval    string  `json:"interval"`     // K线周期：1m/5m/15m/30m/1H/4H/1D
	SlippageBps float64 `json:"slippage_bps"` // 回测/模拟撮合滑点（基点）
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
	Name string     `json:"name"` // grid
	Grid GridConfig `json:"grid"`
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
		Trading: TradingConfig{Interval: "1H", SlippageBps: 5},
		Risk: RiskConfig{
			MaxOrderNotionalUSD:    1000,
			MaxDailyNotionalUSD:    10000,
			MaxPositionNotionalUSD: 5000,
			MaxOrdersPerMinute:     10,
			MaxDailyLossPct:        5,
			CooldownAfterRejectSec: 30,
		},
		Strategy: StrategyConfig{
			Name: "grid",
			Grid: GridConfig{
				Lower: 40000, Upper: 80000, Grids: 20,
				QtyPerGrid: 0.001, Spacing: "geo", StopOnBreak: true,
			},
		},
		Ump:       UmpConfig{Enabled: true},
		Dashboard: DashboardConfig{Enabled: true, Listen: "127.0.0.1:8080"},
		DataDir:   "data",
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
	return nil
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
