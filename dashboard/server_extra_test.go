package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/execution"
	"github.com/HarveyBase/QuantForge/grid"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/review"
	"github.com/HarveyBase/QuantForge/risk"
)

func TestAuthWithToken(t *testing.T) {
	cfg := configDefaultWithToken("topsecret")
	s := newServerWithCfg(t, cfg)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// 无 Token 401
	resp, _ := http.Get(srv.URL + "/api/status")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 Token 应 401: %d", resp.StatusCode)
	}
	// 错误 Token 401
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 Token 应 401: %d", resp2.StatusCode)
	}
	// Header Token 通过
	req2, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req2.Header.Set("Authorization", "Bearer topsecret")
	resp3, _ := http.DefaultClient.Do(req2)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("正确 Token 应放行: %d", resp3.StatusCode)
	}
	// Query Token 通过
	resp4, _ := http.Get(srv.URL + "/api/status?token=topsecret")
	resp4.Body.Close()
	if resp4.StatusCode != 200 {
		t.Fatalf("Query Token 应放行: %d", resp4.StatusCode)
	}
}

func TestAuthLoopbackOnlyWithoutToken(t *testing.T) {
	s := newServerForTest(t)
	handler := s.Handler()
	// 非回环来源直接调 handler（伪造 RemoteAddr）
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.RemoteAddr = "203.0.113.10:5555"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("无 Token 且非回环应 403: %d", w.Code)
	}
	// IPv6 回环放行
	req6 := httptest.NewRequest("GET", "/api/status", nil)
	req6.RemoteAddr = "[::1]:4444"
	w6 := httptest.NewRecorder()
	handler.ServeHTTP(w6, req6)
	if w6.Code != 200 {
		t.Fatalf("IPv6 回环应放行: %d", w6.Code)
	}
	// 无端口的回环地址（SplitHostPort 失败退化用原串）
	reqNH := httptest.NewRequest("GET", "/api/status", nil)
	reqNH.RemoteAddr = "localhost"
	wNH := httptest.NewRecorder()
	handler.ServeHTTP(wNH, reqNH)
	if wNH.Code != 200 {
		t.Fatalf("localhost 应放行: %d", wNH.Code)
	}
}

func TestAllGetEndpoints(t *testing.T) {
	s := newServerForTest(t)
	s.Snapshots = func() []exchange.Candle { return nil } // research 模式无快照源
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, ep := range []string{"/api/positions", "/api/orders", "/api/rejections", "/api/events", "/api/candles", "/api/config"} {
		resp, err := http.Get(srv.URL + ep)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s 应 200: %d", ep, resp.StatusCode)
		}
	}
	// config 视图脱敏
	cfg := configDefaultWithToken("tk")
	s2 := newServerWithCfg(t, cfg)
	srv2 := httptest.NewServer(s2.Handler())
	defer srv2.Close()
	resp, err := http.Get(srv2.URL + "/api/config?token=tk")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	dash := body["dashboard"].(map[string]any)
	if dash["token"] != "" {
		t.Fatal("config 接口不得泄露 Token")
	}
}

func TestGridEndpoint(t *testing.T) {
	s := newServerForTest(t)
	g, err := grid.New(grid.Params{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0.1, Spacing: "arith"})
	if err != nil {
		t.Fatal(err)
	}
	s.Grid = g
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/grid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["levels"] == nil || body["stats"] == nil {
		t.Fatalf("grid 端点应返回网格线与统计: %v", body)
	}
	// nil 网格返回空对象
	s.Grid = nil
	resp2, _ := http.Get(srv.URL + "/api/grid")
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("nil grid 应 200 空对象: %d", resp2.StatusCode)
	}
}

func TestCandlesEndpointReturnsSnapshots(t *testing.T) {
	s := newServerForTest(t)
	s.Snapshots = func() []exchange.Candle {
		return []exchange.Candle{{Symbol: "BTC-USDT", Close: 100, Confirmed: true}}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/candles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Candles []exchange.Candle `json:"candles"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Candles) != 1 || body.Candles[0].Close != 100 {
		t.Fatalf("candles 端点应透传快照: %+v", body.Candles)
	}
}

func TestKillSwitchLiveModeResetForbidden(t *testing.T) {
	cfg := configDefaultWithToken("tk")
	cfg.Mode = "live"
	s := newServerWithCfg(t, cfg)
	s.Rk.Kill.Trip("演练")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/api/killswitch?token=tk", "application/json",
		strings.NewReader(`{"action":"reset","confirm":"RESET"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("live 模式禁止热复位 Kill Switch: %d", resp.StatusCode)
	}
	if !s.Rk.Kill.Tripped() {
		t.Fatal("live 模式复位必须无效")
	}
}

func TestKillSwitchBadRequests(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// 非法 JSON
	resp, _ := http.Post(srv.URL+"/api/killswitch", "application/json", strings.NewReader(`{`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法请求体应 400: %d", resp.StatusCode)
	}
	// 未知 action
	resp2, _ := http.Post(srv.URL+"/api/killswitch", "application/json",
		strings.NewReader(`{"action":"nuke"}`))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知 action 应 400: %d", resp2.StatusCode)
	}
}

func TestBacktestEndpoint(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// 未配置数据源
	resp, _ := http.Post(srv.URL+"/api/backtest", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未配置回测源应 503: %d", resp.StatusCode)
	}
	// 配置后成功
	s.RunBacktest = func(ctx context.Context) (*backtest.Result, error) {
		return &backtest.Result{Metrics: backtest.Metrics{FinalEquity: 100}}, nil
	}
	resp2, err := http.Post(srv.URL+"/api/backtest", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("回测端点应 200: %d", resp2.StatusCode)
	}
	var res backtest.Result
	json.NewDecoder(resp2.Body).Decode(&res)
	if res.Metrics.FinalEquity != 100 {
		t.Fatal("回测结果应透传")
	}
	// 回测失败 → 500
	s.RunBacktest = func(ctx context.Context) (*backtest.Result, error) {
		return nil, fmt.Errorf("数据缺失")
	}
	resp3, _ := http.Post(srv.URL+"/api/backtest", "application/json", nil)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusInternalServerError {
		t.Fatalf("回测失败应 500: %d", resp3.StatusCode)
	}
}

func TestSSEStream(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("SSE Content-Type 错误: %s", ct)
		}
	}
	// Broadcast 后订阅者可收到事件（用内部机制验证）
	got := make(chan []byte, 1)
	s.mu.Lock()
	s.subs[got] = struct{}{}
	s.mu.Unlock()
	s.Broadcast("test_event", map[string]any{"v": 1})
	select {
	case msg := <-got:
		if !strings.Contains(string(msg), "test_event") {
			t.Fatalf("SSE 事件内容错误: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("Broadcast 应送达订阅者")
	}
}

func TestStaticServesAssets(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// 根路径无 index.html（webdist 只含 assets）→ 404
	resp, _ := http.Get(srv.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Logf("根路径返回 %d（无 index.html 时 404 为预期）", resp.StatusCode)
	}
	// 未知 API 路径 → 404
	resp2, _ := http.Get(srv.URL + "/api/unknown")
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("未知 API 应 404: %d", resp2.StatusCode)
	}
}

func TestStatusEndpointFields(t *testing.T) {
	s := newServerForTest(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	for _, key := range []string{"equity", "cash", "positions", "kill_switch", "daily_notional_used", "risk_limits", "uptime_sec"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status 缺少字段 %s", key)
		}
	}
	ks := body["kill_switch"].(map[string]any)
	if ks["tripped"] != false {
		t.Fatal("初始 Kill Switch 应未触发")
	}
}

func configDefaultWithToken(token string) *config.Config {
	cfg := config.Default()
	cfg.Dashboard.Token = token
	return cfg
}

func newServerWithCfg(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	pf := portfolio.New(10000)
	rk := risk.NewManager(risk.Limits{MaxOrdersPerMinute: 10, MaxDailyLossPct: 5}, pf, "")
	return New(cfg, pf, rk, NoopExecutor{}, nil, nil, nil)
}

func TestReviewsEndpoint(t *testing.T) {
	s := newServerForTest(t)
	s.RecentReviews = func(n int) []review.Record {
		return []review.Record{{Stage: "paper", Symbol: "BTC-USDT"}}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/reviews")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Reviews []review.Record `json:"reviews"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Reviews) != 1 || body.Reviews[0].Symbol != "BTC-USDT" {
		t.Fatalf("复盘端点应透传记录: %+v", body.Reviews)
	}
	// nil 注入安全
	s.RecentReviews = nil
	resp2, _ := http.Get(srv.URL + "/api/reviews")
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("nil 注入应安全: %d", resp2.StatusCode)
	}
}

func TestModeSwitchPermissionMatrix(t *testing.T) {
	newSrv := func(boot config.Mode, hasExec bool) *Server {
		s := newServerForTest(t)
		s.BootMode = boot
		s.ActiveMode = func() config.Mode { return boot }
		s.SwitchMode = func(m config.Mode, confirm string) error {
			switch m {
			case config.ModeResearch:
				return nil
			case config.ModePaper:
				if !hasExec {
					return fmt.Errorf("research 启动无执行器")
				}
				return nil
			case config.ModeLive:
				if boot != config.ModeLive {
					return fmt.Errorf("live 需 live 配置重启 + 门禁")
				}
				if confirm != "I_UNDERSTAND_THE_RISK" {
					return fmt.Errorf("需确认词")
				}
				return nil
			}
			return fmt.Errorf("未知环境")
		}
		return s
	}
	post := func(s *Server, body string) int {
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		resp, _ := http.Post(srv.URL+"/api/mode", "application/json", strings.NewReader(body))
		resp.Body.Close()
		return resp.StatusCode
	}
	// research 启动：可切 research；切 paper 被拒（无执行器）；切 live 被拒
	s1 := newSrv(config.ModeResearch, false)
	if c := post(s1, `{"mode":"research"}`); c != 200 {
		t.Fatalf("research→research 应允许: %d", c)
	}
	if c := post(s1, `{"mode":"paper"}`); c != 403 {
		t.Fatalf("research 启动切 paper 应 403: %d", c)
	}
	if c := post(s1, `{"mode":"live","confirm":"I_UNDERSTAND_THE_RISK"}`); c != 403 {
		t.Fatalf("research 启动切 live 必须 403（红线）: %d", c)
	}
	// paper 启动：research↔paper 自由；live 仍拒
	s2 := newSrv(config.ModePaper, true)
	if c := post(s2, `{"mode":"paper"}`); c != 200 {
		t.Fatalf("paper→paper 应允许: %d", c)
	}
	if c := post(s2, `{"mode":"live","confirm":"I_UNDERSTAND_THE_RISK"}`); c != 403 {
		t.Fatalf("paper 启动切 live 必须 403: %d", c)
	}
	// live 启动：无确认词拒；有确认词允许；可降级 paper/research
	s3 := newSrv(config.ModeLive, true)
	if c := post(s3, `{"mode":"live"}`); c != 403 {
		t.Fatalf("live 无确认词应 403: %d", c)
	}
	if c := post(s3, `{"mode":"live","confirm":"I_UNDERSTAND_THE_RISK"}`); c != 200 {
		t.Fatalf("live 启动+确认词切 live 应允许: %d", c)
	}
	if c := post(s3, `{"mode":"paper"}`); c != 200 {
		t.Fatalf("live 降级 paper 应允许: %d", c)
	}
	// GET /api/mode
	s4 := newSrv(config.ModePaper, true)
	srv := httptest.NewServer(s4.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/mode")
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["active"] != "paper" || body["boot"] != "paper" {
		t.Fatalf("mode 状态错误: %v", body)
	}
	// 未知环境
	if c := post(s4, `{"mode":"yolo"}`); c != 403 {
		t.Fatalf("未知环境应 403: %d", c)
	}
}

func TestFillsEquityCurveEndpoints(t *testing.T) {
	s := newServerForTest(t)
	s.Fills = func() []execution.Event {
		return []execution.Event{{Kind: "filled", DeltaQty: 0.5, DeltaPrice: 100, Order: exchange.Order{OrderID: "o1", Side: exchange.Buy}}}
	}
	s.EquityCurve = func() []review.Record { // Recent 语义：最新在前
		return []review.Record{
			{Ts: time.Now(), Equity: 110},
			{Ts: time.Now().Add(-time.Hour), Equity: 100},
		}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/fills")
	var fb struct {
		Fills []execution.Event `json:"fills"`
	}
	json.NewDecoder(resp.Body).Decode(&fb)
	resp.Body.Close()
	if len(fb.Fills) != 1 || fb.Fills[0].DeltaQty != 0.5 {
		t.Fatalf("fills 端点错误: %+v", fb)
	}
	resp2, _ := http.Get(srv.URL + "/api/equitycurve")
	var eb struct {
		Points []struct {
			Ts     int64   `json:"ts"`
			Equity float64 `json:"equity"`
		} `json:"points"`
	}
	json.NewDecoder(resp2.Body).Decode(&eb)
	resp2.Body.Close()
	if len(eb.Points) != 2 || eb.Points[0].Equity != 100 || eb.Points[1].Equity != 110 {
		t.Fatalf("equitycurve 应升序: %+v", eb.Points)
	}
	// nil 注入安全
	s.Fills = nil
	s.EquityCurve = nil
	r3, _ := http.Get(srv.URL + "/api/fills")
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("nil fills 应安全: %d", r3.StatusCode)
	}
}

func TestManualOrderAndCancel(t *testing.T) {
	s := newServerForTest(t)
	placed := make(chan exchange.OrderRequest, 1)
	cancelled := make(chan string, 1)
	s.PlaceOrder = func(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
		placed <- req
		return exchange.Order{OrderID: "ok-1"}, nil
	}
	s.CancelOrder = func(ctx context.Context, id string) error {
		cancelled <- id
		return nil
	}
	s.ActiveMode = func() config.Mode { return config.ModePaper } // 活跃模拟盘
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// 下单：research 拒
	s.ActiveMode = func() config.Mode { return config.ModeResearch }
	r1, _ := http.Post(srv.URL+"/api/order", "application/json", strings.NewReader(`{"side":"buy","type":"market","qty":0.001}`))
	r1.Body.Close()
	if r1.StatusCode != 403 {
		t.Fatalf("研究环境手动下单应 403: %d", r1.StatusCode)
	}
	// 下单：paper 通过
	s.ActiveMode = func() config.Mode { return config.ModePaper }
	r2, _ := http.Post(srv.URL+"/api/order", "application/json", strings.NewReader(`{"side":"buy","type":"market","qty":0.001}`))
	var o exchange.Order
	json.NewDecoder(r2.Body).Decode(&o)
	r2.Body.Close()
	if o.OrderID != "ok-1" {
		t.Fatalf("手动下单失败: %+v", o)
	}
	select {
	case req := <-placed:
		if req.Symbol != "BTC-USDT" || req.ClientOrderID == "" {
			t.Fatalf("下单参数缺失: %+v", req)
		}
	default:
		t.Fatal("PlaceOrder 未被调用")
	}
	// 撤单
	r3, _ := http.Post(srv.URL+"/api/cancel", "application/json", strings.NewReader(`{"order_id":"ok-1"}`))
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("撤单失败: %d", r3.StatusCode)
	}
	select {
	case id := <-cancelled:
		if id != "ok-1" {
			t.Fatalf("撤单 ID 错误: %s", id)
		}
	default:
		t.Fatal("CancelOrder 未被调用")
	}
	// 空 order_id 拒
	r4, _ := http.Post(srv.URL+"/api/cancel", "application/json", strings.NewReader(`{}`))
	r4.Body.Close()
	if r4.StatusCode != 400 {
		t.Fatalf("空 order_id 应 400: %d", r4.StatusCode)
	}
	// 无执行器 503
	s.PlaceOrder = nil
	r5, _ := http.Post(srv.URL+"/api/order", "application/json", strings.NewReader(`{"side":"buy","type":"market","qty":0.001}`))
	r5.Body.Close()
	if r5.StatusCode != 503 {
		t.Fatalf("无执行器应 503: %d", r5.StatusCode)
	}
}
