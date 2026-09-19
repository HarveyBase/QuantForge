package config

import (
	"strings"
	"testing"
)

// legacyJSON 模拟老版本配置文件（无 trading.fees、无 ops 节），
// 升级二进制后必须无缝回填默认值且不报错。
const legacyJSON = `{
  "mode": "research",
  "exchange": {
    "name": "okx",
    "market": "SPOT",
    "inst_id": "BTC-USDT",
    "rest_url": "https://www.okx.com",
    "td_mode": "cross",
    "leverage": 3
  },
  "trading": {
    "interval": "1H",
    "slippage_bps": 5
  },
  "risk": {
    "max_order_notional_usd": 1000,
    "max_daily_notional_usd": 10000,
    "max_position_notional_usd": 5000,
    "max_orders_per_minute": 10,
    "max_daily_loss_pct": 5,
    "cooldown_after_reject_sec": 30
  },
  "strategy": {
    "name": "grid",
    "grid": {
      "lower": 40000,
      "upper": 80000,
      "grids": 20,
      "qty_per_grid": 0.001,
      "spacing": "geo",
      "stop_on_break": true
    }
  },
  "dashboard": {"enabled": true, "listen": "127.0.0.1:8080", "token": ""},
  "data_dir": "data",
  "ump": {"enabled": true}
}`

func TestLoadLegacyConfigBackfillsNewFields(t *testing.T) {
	cfg, err := Load(writeTemp(t, legacyJSON))
	if err != nil {
		t.Fatalf("老配置（缺 fees/ops 节）必须无缝加载: %v", err)
	}
	if cfg.Trading.Fees.MakerBps != DefaultMakerFeeBps || cfg.Trading.Fees.TakerBps != DefaultTakerFeeBps {
		t.Fatalf("缺省费率应回填保守默认 %d/%d bps，当前 %+v", DefaultMakerFeeBps, DefaultTakerFeeBps, cfg.Trading.Fees)
	}
	want := OpsConfig{
		EquityWatchSec:        30,
		ReconcileSec:          300,
		ReconcileTolerancePct: 0.5,
		RestRateLimitPerSec:   10,
		ShutdownCancelOrders:  true,
		KillFlatten:           false,
	}
	if cfg.Ops != want {
		t.Fatalf("缺省 ops 节应回填默认值 %+v，当前 %+v", want, cfg.Ops)
	}
}

func TestLoadExplicitFeesAndOpsPreserved(t *testing.T) {
	path := writeTemp(t, `{
		"trading": {"fees": {"maker_bps": 1, "taker_bps": 1.5}},
		"ops": {"equity_watch_sec": 15, "reconcile_sec": 120, "reconcile_tolerance_pct": 1.5,
			"rest_rate_limit_per_sec": 20, "shutdown_cancel_orders": false, "kill_flatten": true}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("显式合法值必须原样保留: %v", err)
	}
	if cfg.Trading.Fees.MakerBps != 1 || cfg.Trading.Fees.TakerBps != 1.5 {
		t.Fatalf("显式费率被覆盖: %+v", cfg.Trading.Fees)
	}
	if cfg.Ops.EquityWatchSec != 15 || cfg.Ops.ReconcileSec != 120 ||
		cfg.Ops.ReconcileTolerancePct != 1.5 || cfg.Ops.RestRateLimitPerSec != 20 ||
		cfg.Ops.ShutdownCancelOrders || !cfg.Ops.KillFlatten {
		t.Fatalf("显式 ops 值被覆盖: %+v", cfg.Ops)
	}
}

func TestValidateFeesBounds(t *testing.T) {
	cases := []struct {
		maker, taker float64
	}{
		{0, 10},    // maker 为 0
		{-1, 10},   // maker 为负
		{200, 10},  // maker 超上界
		{8, 0},     // taker 为 0
		{8, -0.1},  // taker 为负
		{8, 200},   // taker 超上界（边界值本身也拒绝）
		{8, 200.1}, // taker 远超上界
	}
	for i, c := range cases {
		cfg := Default()
		cfg.Trading.Fees = FeesConfig{MakerBps: c.maker, TakerBps: c.taker}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "trading.fees") {
			t.Errorf("case %d (%v/%v) 应报含 trading.fees 的错误: %v", i, c.maker, c.taker, err)
		}
	}
	// 合法边界：均严格落在 (0,200) 内。
	cfg := Default()
	cfg.Trading.Fees = FeesConfig{MakerBps: 0.01, TakerBps: 199.99}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("(0,200) 内的合法费率应通过: %v", err)
	}
}

func TestValidateOpsBounds(t *testing.T) {
	cases := []struct {
		mutate func(*OpsConfig)
		msg    string
	}{
		{func(o *OpsConfig) { o.EquityWatchSec = 4 }, "equity_watch_sec"},
		{func(o *OpsConfig) { o.EquityWatchSec = 0 }, "equity_watch_sec"},
		{func(o *OpsConfig) { o.EquityWatchSec = -30 }, "equity_watch_sec"},
		{func(o *OpsConfig) { o.ReconcileSec = 59 }, "reconcile_sec"},
		{func(o *OpsConfig) { o.ReconcileSec = 0 }, "reconcile_sec"},
		{func(o *OpsConfig) { o.ReconcileSec = -1 }, "reconcile_sec"},
		{func(o *OpsConfig) { o.ReconcileTolerancePct = 0 }, "reconcile_tolerance_pct"},
		{func(o *OpsConfig) { o.ReconcileTolerancePct = -0.5 }, "reconcile_tolerance_pct"},
		{func(o *OpsConfig) { o.ReconcileTolerancePct = 5.1 }, "reconcile_tolerance_pct"},
		{func(o *OpsConfig) { o.RestRateLimitPerSec = 0 }, "rest_rate_limit_per_sec"},
		{func(o *OpsConfig) { o.RestRateLimitPerSec = -1 }, "rest_rate_limit_per_sec"},
		{func(o *OpsConfig) { o.RestRateLimitPerSec = 51 }, "rest_rate_limit_per_sec"},
	}
	for i, c := range cases {
		cfg := Default()
		c.mutate(&cfg.Ops)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("case %d 应报含 %q 的错误: %v", i, c.msg, err)
		}
	}
	// 合法边界：下限/上限本身可取。
	cfg := Default()
	cfg.Ops = OpsConfig{
		EquityWatchSec:        5,
		ReconcileSec:          60,
		ReconcileTolerancePct: 5,
		RestRateLimitPerSec:   50,
		ShutdownCancelOrders:  false,
		KillFlatten:           true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ops 边界合法值应通过: %v", err)
	}
}

func TestValidateOpsRejectsViaLoad(t *testing.T) {
	path := writeTemp(t, `{"ops":{"equity_watch_sec":1}}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "equity_watch_sec") {
		t.Fatalf("Load 路径下非法 ops 值必须报错: %v", err)
	}
}

func TestFeeModel(t *testing.T) {
	m, tk := Default().FeeModel()
	if m != DefaultMakerFeeBps || tk != DefaultTakerFeeBps {
		t.Fatalf("默认配置 FeeModel 应为 %d/%d，当前 %v/%v", DefaultMakerFeeBps, DefaultTakerFeeBps, m, tk)
	}
	cfg := Default()
	cfg.Trading.Fees = FeesConfig{MakerBps: 2, TakerBps: 5}
	if m, tk = cfg.FeeModel(); m != 2 || tk != 5 {
		t.Fatalf("显式设置应原样返回，当前 %v/%v", m, tk)
	}
	// 手工构造的零值 Config 也必须拿到非零保守费率（宁可高估成本）。
	var zero Config
	if m, tk = zero.FeeModel(); m != DefaultMakerFeeBps || tk != DefaultTakerFeeBps {
		t.Fatalf("零值 Config FeeModel 应回退保守默认 %d/%d，当前 %v/%v", DefaultMakerFeeBps, DefaultTakerFeeBps, m, tk)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json 必须是可加载的合法模板: %v", err)
	}
	if m, tk := cfg.FeeModel(); m != DefaultMakerFeeBps || tk != DefaultTakerFeeBps {
		t.Fatalf("示例文件费率应与保守默认一致 %d/%d，当前 %v/%v", DefaultMakerFeeBps, DefaultTakerFeeBps, m, tk)
	}
	if cfg.Ops.ReconcileSec != 300 {
		t.Fatalf("示例文件 ops 默认值漂移: %+v", cfg.Ops)
	}
}
