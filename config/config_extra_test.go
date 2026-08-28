package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileMissing(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("配置缺失必须报错: %v", err)
	}
}

func TestLoadCorruptJSON(t *testing.T) {
	path := writeTemp(t, `{invalid`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "解析") {
		t.Fatalf("损坏 JSON 必须报错: %v", err)
	}
}

func TestValidateBranches(t *testing.T) {
	cases := []struct {
		mutate func(*Config)
		msg    string
	}{
		{func(c *Config) { c.Mode = "yolo" }, "mode"},
		{func(c *Config) { c.Exchange.Name = "binance" }, "exchange.name"},
		{func(c *Config) { c.Exchange.Market = "MARGIN" }, "market"},
		{func(c *Config) { c.Exchange.InstID = "" }, "inst_id"},
		{func(c *Config) { c.Exchange.RestURL = "" }, "rest_url"},
		{func(c *Config) { c.Exchange.RestURL = "http://plain.example.com" }, "HTTPS"},
		{func(c *Config) { c.Mode = ModePaper; c.Exchange.Market = "SWAP" }, "SWAP"},
		{func(c *Config) { c.Trading.SlippageBps = -1 }, "slippage_bps"},
		{func(c *Config) { c.Trading.SlippageBps = 1001 }, "slippage_bps"},
		{func(c *Config) { c.Trading.Interval = "7x" }, "interval"},
		{func(c *Config) { c.Exchange.Market = "SWAP"; c.Exchange.Leverage = 0.5 }, "杠杆"},
		{func(c *Config) { c.Exchange.Market = "SWAP"; c.Exchange.Leverage = 1; c.Exchange.TdMode = "net" }, "td_mode"},
		{func(c *Config) { c.Risk.MaxDailyNotionalUSD = 0 }, "risk 限额"},
		{func(c *Config) { c.Risk.MaxPositionNotionalUSD = -1 }, "risk 限额"},
		{func(c *Config) { c.Risk.MaxOrdersPerMinute = 0 }, "risk 限额"},
		{func(c *Config) { c.Risk.MaxDailyLossPct = 0 }, "max_daily_loss_pct"},
		{func(c *Config) { c.Risk.MaxDailyLossPct = 101 }, "max_daily_loss_pct"},
		{func(c *Config) { c.Risk.CooldownAfterRejectSec = -1 }, "cooldown"},
		{func(c *Config) { c.Strategy.Grid.Lower = 0 }, "lower"},
		{func(c *Config) { c.Strategy.Grid.Upper = 40000 }, "upper"},
		{func(c *Config) { c.Strategy.Grid.Grids = 1 }, "grids"},
		{func(c *Config) { c.Strategy.Grid.QtyPerGrid = 0 }, "qty_per_grid"},
		{func(c *Config) { c.Strategy.Grid.Spacing = "log" }, "spacing"},
	}
	for i, c := range cases {
		cfg := Default()
		c.mutate(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("case %d 应报含 %q 的错误: %v", i, c.msg, err)
		}
	}
}

func TestValidateSWAPResearchAllowed(t *testing.T) {
	cfg := Default()
	cfg.Exchange.Market = "SWAP"
	cfg.Exchange.InstID = "BTC-USDT-SWAP"
	cfg.Exchange.Leverage = 3
	cfg.Exchange.TdMode = "isolated"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("research + SWAP + 合法杠杆应通过: %v", err)
	}
}

func TestSanitizedStripsToken(t *testing.T) {
	cfg := Default()
	cfg.Dashboard.Token = "secret-token"
	s := cfg.Sanitized()
	if s.Dashboard.Token != "" {
		t.Fatal("脱敏视图必须清除 Token")
	}
	if cfg.Dashboard.Token != "secret-token" {
		t.Fatal("脱敏不得改动原配置")
	}
	if s.Dashboard.Listen != cfg.Dashboard.Listen {
		t.Fatal("其余字段应保留")
	}
}

func TestCheckLiveGate(t *testing.T) {
	cfg := Default()
	cfg.Mode = ModeLive
	if err := cfg.CheckLiveGate(); err == nil {
		t.Fatal("live 无门禁必须拒绝")
	}
	t.Setenv(LiveGateEnv, "wrong")
	if err := cfg.CheckLiveGate(); err == nil {
		t.Fatal("门禁值错误必须拒绝")
	}
	t.Setenv(LiveGateEnv, LiveGateValue)
	if err := cfg.CheckLiveGate(); err != nil {
		t.Fatalf("门禁正确应放行: %v", err)
	}
	cfg.Mode = ModeResearch
	os.Unsetenv(LiveGateEnv)
	if err := cfg.CheckLiveGate(); err != nil {
		t.Fatalf("非 live 模式不查门禁: %v", err)
	}
}
