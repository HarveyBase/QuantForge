// reconcile_test.go Kill 复位前置对账门禁与 /api/status 对账状态（P1-1）测试。
package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/portfolio"
)

// TestKillResetBlockedByReconcileDiff 对账不通过 → 409 + 差异摘要，Kill 不复位；
// 对账通过 → 复位成功。
func TestKillResetBlockedByReconcileDiff(t *testing.T) {
	s := newServerForTest(t)
	s.Rk.Kill.Trip("演练停机")
	blocked := true
	s.Reconcile = func() (portfolio.ReconcileReport, bool, error) {
		if blocked {
			return portfolio.ReconcileReport{
				Ts: time.Now().UTC(), Ok: false,
				Diffs: []portfolio.ReconcileDiff{
					{Kind: portfolio.DiffCash, Item: "USDT", Local: 10000, Remote: 8000, Diff: -2000},
				},
			}, false, nil
		}
		return portfolio.ReconcileReport{Ts: time.Now().UTC(), Ok: true}, true, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"reset","confirm":"RESET"}`))
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("对账不通过必须 409 拒绝复位: %d", resp.StatusCode)
	}
	if msg := string(body[:n]); !strings.Contains(msg, "USDT") || !strings.Contains(msg, "10000") {
		t.Fatalf("409 响应应带差异摘要: %s", msg)
	}
	if !s.Rk.Kill.Tripped() {
		t.Fatal("对账不通过时 Kill 不得复位（带病重启交易）")
	}

	// 对账恢复一致 → 复位成功
	blocked = false
	resp2, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"reset","confirm":"RESET"}`))
	resp2.Body.Close()
	if resp2.StatusCode != 200 || s.Rk.Kill.Tripped() {
		t.Fatalf("对账通过后应允许复位: %d tripped=%v", resp2.StatusCode, s.Rk.Kill.Tripped())
	}
}

// TestKillResetReconcileError 对账无法完成（余额拉取失败）→ 同样 409 拒绝复位。
func TestKillResetReconcileError(t *testing.T) {
	s := newServerForTest(t)
	s.Rk.Kill.Trip("演练停机")
	s.Reconcile = func() (portfolio.ReconcileReport, bool, error) {
		return portfolio.ReconcileReport{}, false, errors.New("拉取余额失败: timeout")
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"reset","confirm":"RESET"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("对账失败必须 409 拒绝复位: %d", resp.StatusCode)
	}
	if !s.Rk.Kill.Tripped() {
		t.Fatal("对账失败时 Kill 不得复位")
	}
}

// TestKillResetGateOrder 门禁顺序：确认词 → live 重启门禁 → 对账前置。
// live 模式即使对账通过也必须维持 403（重启门禁不放松）。
func TestKillResetGateOrder(t *testing.T) {
	cfg := configDefaultWithToken("tk")
	cfg.Mode = "live"
	s := newServerWithCfg(t, cfg)
	s.Rk.Kill.Trip("演练")
	reconciled := false
	s.Reconcile = func() (portfolio.ReconcileReport, bool, error) {
		reconciled = true
		return portfolio.ReconcileReport{Ok: true}, true, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/api/killswitch?token=tk", "application/json",
		strings.NewReader(`{"action":"reset","confirm":"RESET"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("live 复位必须先撞重启门禁 403: %d", resp.StatusCode)
	}
	if reconciled {
		t.Fatal("live 门禁未过就不应触发对账（顺序：确认词 → live → 对账）")
	}
}

// TestStatusReconcileFields /api/status 的 reconcile 块：拦截态来自风控、
// 最近报告来自注入数据源；未注入时给安全默认值。
func TestStatusReconcileFields(t *testing.T) {
	s := newServerForTest(t)
	ts := time.Now().UTC().Truncate(time.Second)
	s.LastReconcile = func() portfolio.ReconcileReport {
		return portfolio.ReconcileReport{
			Ts: ts, Ok: false,
			Diffs: []portfolio.ReconcileDiff{
				{Kind: portfolio.DiffCash, Item: "USDT", Local: 10000, Remote: 8000, Diff: -2000},
				{Kind: portfolio.DiffPosition, Item: "BTC-USDT", Local: 0.05, Remote: 0.04, Diff: -0.01},
			},
		}
	}
	s.Rk.BlockForReconcile("USDT 差异: 本地 10000.00 vs 交易所 8000.00")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	rec, ok := body["reconcile"].(map[string]any)
	if !ok {
		t.Fatalf("status 应含 reconcile 块: %v", body)
	}
	if rec["ok"] != false || rec["blocked"] != true {
		t.Fatalf("拦截态错误: %v", rec)
	}
	if r, _ := rec["reason"].(string); !strings.Contains(r, "USDT") {
		t.Fatalf("reason 应透传差异摘要: %v", rec["reason"])
	}
	if tsMilli, _ := rec["last_ts"].(float64); int64(tsMilli) != ts.UnixMilli() {
		t.Fatalf("last_ts 应为最近对账时间: %v", rec["last_ts"])
	}
	if diffs, _ := rec["diffs"].([]any); len(diffs) != 2 {
		t.Fatalf("diffs 应透传差异明细: %v", rec["diffs"])
	}

	// 未注入 LastReconcile：安全默认（diffs=[] / last_ts=null），不 500
	s.LastReconcile = nil
	resp2, _ := http.Get(srv.URL + "/api/status")
	var body2 map[string]any
	json.NewDecoder(resp2.Body).Decode(&body2)
	resp2.Body.Close()
	rec2, ok := body2["reconcile"].(map[string]any)
	if !ok {
		t.Fatalf("reconcile 块必须始终存在: %v", body2)
	}
	if diffs, _ := rec2["diffs"].([]any); diffs == nil || len(diffs) != 0 {
		t.Fatalf("未注入时 diffs 应为空数组而非 null: %v", rec2["diffs"])
	}
	if rec2["last_ts"] != nil {
		t.Fatalf("未注入时 last_ts 应为 null: %v", rec2["last_ts"])
	}
}
