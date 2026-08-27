package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsValid(t *testing.T) {
	path := writeTemp(t, `{"mode":"research"}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("默认配置应当合法: %v", err)
	}
	if cfg.Mode != ModeResearch || cfg.Exchange.Name != "okx" || cfg.Strategy.Grid.Grids != 20 {
		t.Fatalf("默认值回填错误: %+v", cfg)
	}
}

func TestLiveGateBlocksWithoutEnv(t *testing.T) {
	os.Unsetenv(LiveGateEnv)
	path := writeTemp(t, `{"mode":"live"}`)
	if _, err := Load(path); err == nil {
		t.Fatal("mode=live 且未设置门禁环境变量时必须拒绝启动")
	}
}

func TestLiveGatePassWithEnv(t *testing.T) {
	t.Setenv(LiveGateEnv, LiveGateValue)
	path := writeTemp(t, `{"mode":"live"}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("门禁通过时应允许 live: %v", err)
	}
	if cfg.Mode != ModeLive {
		t.Fatal("mode 应为 live")
	}
}

func TestValidateGrid(t *testing.T) {
	path := writeTemp(t, `{"strategy":{"grid":{"lower":100,"upper":50,"grids":10,"qty_per_grid":0.001,"spacing":"arith"}}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("lower>=upper 必须报错")
	}
}

func TestValidateLeverageCap(t *testing.T) {
	path := writeTemp(t, `{"exchange":{"market":"SWAP","inst_id":"BTC-USDT-SWAP","leverage":25}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("杠杆 >10 必须被配置校验拒绝（禁高杠杆红线）")
	}
}

func TestValidateRiskLimits(t *testing.T) {
	path := writeTemp(t, `{"risk":{"max_order_notional_usd":0}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("风控限额缺失必须报错")
	}
}
