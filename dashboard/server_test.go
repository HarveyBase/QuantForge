package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/risk"
)

func TestStatusEndpoint(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status 应 200: %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["mode"] != "research" || body["symbol"] != "BTC-USDT" {
		t.Fatalf("status 字段错误: %v", body)
	}
}

func TestKillSwitchTripRequiresReason(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/killswitch", "application/json", strings.NewReader(`{"action":"trip"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无 reason 必须拒绝: %d", resp.StatusCode)
	}
	resp2, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"trip","reason":"演练"}`))
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("带 reason 应成功: %d", resp2.StatusCode)
	}
	if !s.Rk.Kill.Tripped() {
		t.Fatal("Kill Switch 应已触发")
	}
}

func TestKillSwitchResetRequiresConfirm(t *testing.T) {
	s := newServerForTest(t)
	s.Rk.Kill.Trip("测试")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"reset"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("无确认词必须拒绝复位")
	}
	resp2, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"reset","confirm":"RESET"}`))
	resp2.Body.Close()
	if resp2.StatusCode != 200 || s.Rk.Kill.Tripped() {
		t.Fatal("确认词 RESET 应允许复位")
	}
}

func newServerForTest(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrdersPerMinute: 10, MaxDailyLossPct: 5}, pf, "")
	return New(cfg, pf, rk, NoopExecutor{}, nil, nil, nil)
}
