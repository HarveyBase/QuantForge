// Package dashboard Web 管理后台的 Go API 服务（REST + SSE）。
// 安全基线：默认只绑 127.0.0.1；Token 可选；Kill Switch 是唯一可写的高危操作且要求确认词。
package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/backtest"
	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/execution"
	"github.com/HarveyBase/QuantForge/grid"
	"github.com/HarveyBase/QuantForge/lab"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/regime"
	"github.com/HarveyBase/QuantForge/review"
	"github.com/HarveyBase/QuantForge/risk"
	"github.com/HarveyBase/QuantForge/ump"
	"strconv"
)

// Server 后台服务。
type Server struct {
	Cfg             *config.Config
	Pf              *portfolio.Portfolio
	Rk              *risk.Manager
	Ex              OrderSource
	Grid            *grid.Grid
	Snapshots       func() []exchange.Candle                           // 最近已确认 K 线
	Candles         func(interval string, limit int) []exchange.Candle // SQLite 库读取（任意周期/全历史）
	OrderBook       func() *exchange.OrderBook                         // 盘口深度（spread/流动性）
	RunBacktest     func(ctx context.Context) (*backtest.Result, error)
	RunWalkForward  func(ctx context.Context, strategyName string, train, test int) (*lab.WFReport, error)      // 研究工作台：WF
	RunUMPCheck     func(ctx context.Context, strategyName string, minSamples int) (int, *ump.OOSReport, error) // 研究工作台：UMP
	RunPlateau      func(ctx context.Context, strategyName string) (*lab.PlateauReport, error)                  // 研究工作台：参数高原
	RunCostScan     func(ctx context.Context, strategyName string) ([]lab.CostPoint, error)                     // 研究工作台：成本敏感性
	RecentReviews   func(n int) []review.Record                                                                 // 最近 n 份小时复盘
	Regime          func() regime.Reading                                                                       // 当前市况读数
	CurrentStrategy func() string                                                                               // 当前策略描述
	SwitchStrategy  func(name string) error                                                                     // 热切策略（无持仓才允许）
	Fills           func() []execution.Event                                                                    // 成交历史（事件流过滤）
	EquityCurve     func() []review.Record                                                                      // 权益曲线数据源（复盘记录聚合）
	CancelOrder     func(ctx context.Context, id string) error                                                  // 手动撤单
	PlaceOrder      func(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error)                // 手动下单（走风控）
	// ModeSwap 页面热切活跃环境（research↔paper；live 受启动配置门禁，见 cmd.SwitchMode）
	ActiveMode func() config.Mode // 当前活跃环境
	BootMode   config.Mode        // 启动配置环境（能力上界）
	SwitchMode func(config.Mode, string) error

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	started time.Time
	version string
}

// New 构造后台服务。
func New(cfg *config.Config, pf *portfolio.Portfolio, rk *risk.Manager,
	ex OrderSource, g *grid.Grid,
	snapshots func() []exchange.Candle, runBt func(ctx context.Context) (*backtest.Result, error)) *Server {
	return &Server{
		Cfg: cfg, Pf: pf, Rk: rk, Ex: ex, Grid: g,
		Snapshots: snapshots, RunBacktest: runBt,
		subs: map[chan []byte]struct{}{}, started: time.Now(), version: "0.1.0",
	}
}

// Broadcast 向 SSE 订阅者推送事件。
func (s *Server) Broadcast(kind string, payload any) {
	b, err := json.Marshal(map[string]any{"kind": kind, "data": payload, "ts": time.Now().UnixMilli()})
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // 订阅者阻塞则丢弃（前端断线自动重连）
		}
	}
}

// Handler 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /api/positions", s.auth(s.handlePositions))
	mux.HandleFunc("GET /api/orders", s.auth(s.handleOrders))
	mux.HandleFunc("GET /api/rejections", s.auth(s.handleRejections))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /api/candles", s.auth(s.handleCandles))
	mux.HandleFunc("GET /api/orderbook", s.auth(s.handleOrderBook))
	mux.HandleFunc("GET /api/grid", s.auth(s.handleGrid))
	mux.HandleFunc("GET /api/mode", s.auth(s.handleGetMode))
	mux.HandleFunc("POST /api/mode", s.auth(s.handleSwitchMode))
	mux.HandleFunc("POST /api/killswitch", s.auth(s.handleKillSwitch))
	mux.HandleFunc("POST /api/backtest", s.auth(s.handleBacktest))
	mux.HandleFunc("GET /api/reviews", s.auth(s.handleReviews))
	mux.HandleFunc("GET /api/strategy", s.auth(s.handleGetStrategy))
	mux.HandleFunc("POST /api/strategy", s.auth(s.handleSwitchStrategy))
	mux.HandleFunc("POST /api/research/walkforward", s.auth(s.handleResearchWF))
	mux.HandleFunc("POST /api/research/umpcheck", s.auth(s.handleResearchUMP))
	mux.HandleFunc("POST /api/research/plateau", s.auth(s.handleResearchPlateau))
	mux.HandleFunc("POST /api/research/costscan", s.auth(s.handleResearchCostScan))
	mux.HandleFunc("GET /api/fills", s.auth(s.handleFills))
	mux.HandleFunc("GET /api/equitycurve", s.auth(s.handleEquityCurve))
	mux.HandleFunc("POST /api/cancel", s.auth(s.handleCancel))
	mux.HandleFunc("POST /api/order", s.auth(s.handleManualOrder))
	mux.HandleFunc("GET /api/config", s.auth(s.handleConfig))
	mux.HandleFunc("GET /api/stream", s.auth(s.handleSSE))
	mux.HandleFunc("GET /", s.handleStatic)
	return s.logMiddleware(mux)
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/status" { // 高频轮询不刷屏
			log.Printf("dashboard %s %s %v", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// auth Token 校验（未配置 Token 时仅允许本机回环访问）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := s.Cfg.Dashboard.Token
		if token != "" {
			provided := r.Header.Get("Authorization")
			if provided == "" {
				provided = "Bearer " + r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+token)) != 1 {
				http.Error(w, "未授权", http.StatusUnauthorized)
				return
			}
		} else if !isLoopback(r.RemoteAddr) {
			http.Error(w, "未配置 Token 时后台仅限本机访问", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cash, positions, _ := s.Pf.Snapshot()
	_, _, marks := s.Pf.Snapshot()
	writeJSON(w, map[string]any{
		"version":             s.version,
		"mode":                s.Cfg.Mode,
		"exchange":            s.Cfg.Exchange.Name,
		"market":              s.Cfg.Exchange.Market,
		"symbol":              s.Cfg.Exchange.InstID,
		"interval":            s.Cfg.Trading.Interval,
		"equity":              s.Pf.Equity(),
		"cash":                cash,
		"positions":           positions,
		"marks":               marks,
		"kill_switch":         map[string]any{"tripped": s.Rk.Kill.Tripped(), "reason": s.Rk.Kill.Reason()},
		"daily_notional_used": s.Rk.DailyNotionalUsed(),
		"regime":              s.regimeSnapshot(),
		"risk_limits":         s.Cfg.Risk,
		"uptime_sec":          int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) regimeSnapshot() any {
	if s.Regime == nil {
		return nil
	}
	return s.Regime()
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	cash, positions, marks := s.Pf.Snapshot()
	writeJSON(w, map[string]any{"cash": cash, "positions": positions, "marks": marks})
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders := s.Ex.OpenOrders()
	if orders == nil {
		orders = []exchange.Order{} // research 模式 NoopExecutor 返回 nil：序列化为 [] 而非 null（前端空表兼容）
	}
	writeJSON(w, map[string]any{"orders": orders})
}

func (s *Server) handleRejections(w http.ResponseWriter, r *http.Request) {
	rejections := s.Rk.Rejections()
	if rejections == nil {
		rejections = []risk.Rejection{}
	}
	writeJSON(w, map[string]any{"rejections": rejections})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"events": s.Ex.Events(200)})
}

func (s *Server) handleCandles(w http.ResponseWriter, r *http.Request) {
	interval := r.URL.Query().Get("interval")
	limit := 300
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	if s.Candles != nil {
		writeJSON(w, map[string]any{"candles": s.Candles(interval, limit), "interval": interval})
		return
	}
	writeJSON(w, map[string]any{"candles": s.Snapshots()})
}

// handleOrderBook 盘口深度：spread（基点）与前 5 档流动性（滑点模型输入）。
func (s *Server) handleOrderBook(w http.ResponseWriter, r *http.Request) {
	if s.OrderBook == nil {
		writeJSON(w, map[string]any{"orderbook": nil})
		return
	}
	ob := s.OrderBook()
	if ob == nil {
		writeJSON(w, map[string]any{"orderbook": nil})
		return
	}
	bidN, askN := ob.DepthNotional(5)
	writeJSON(w, map[string]any{
		"orderbook":   ob,
		"spread_bp":   ob.SpreadBp(),
		"bid_depth_5": bidN,
		"ask_depth_5": askN,
	})
}

func (s *Server) handleGrid(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{}
	if s.Grid != nil {
		resp["levels"] = s.Grid.Levels()
		resp["stats"] = s.Grid.Stats()
	}
	writeJSON(w, resp)
}

// handleGetMode 环境状态：活跃环境 + 启动配置上界 + 可切换项。
func (s *Server) handleGetMode(w http.ResponseWriter, r *http.Request) {
	active := s.Cfg.Mode
	if s.ActiveMode != nil {
		active = s.ActiveMode()
	}
	switchable := []config.Mode{config.ModeResearch}
	if s.BootMode == config.ModePaper || s.BootMode == config.ModeLive {
		switchable = append(switchable, config.ModePaper)
	}
	if s.BootMode == config.ModeLive {
		switchable = append(switchable, config.ModeLive)
	}
	writeJSON(w, map[string]any{
		"active": active, "boot": s.BootMode, "switchable": switchable,
		"gate_env": config.LiveGateEnv, "gate_value": config.LiveGateValue,
	})
}

// handleSwitchMode 页面热切活跃环境。
// 升级纪律（docs/08）：live 只能来自 live 启动配置 + 确认词；research 启动无执行器不可切 paper。
func (s *Server) handleSwitchMode(w http.ResponseWriter, r *http.Request) {
	if s.SwitchMode == nil {
		http.Error(w, "本部署不支持环境切换", http.StatusNotImplemented)
		return
	}
	var req struct {
		Mode    config.Mode `json:"mode"`
		Confirm string      `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if err := s.SwitchMode(req.Mode, req.Confirm); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	s.Broadcast("mode_switch", map[string]any{"mode": req.Mode})
	writeJSON(w, map[string]any{"active": req.Mode})
}

// handleKillSwitch 唯一高危可写操作：trip 需 reason；reset 需确认词（防误触）。
func (s *Server) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string `json:"action"` // trip | reset
		Reason  string `json:"reason"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "trip":
		if req.Reason == "" {
			http.Error(w, "触发 Kill Switch 必须填写原因", http.StatusBadRequest)
			return
		}
		s.Rk.Kill.Trip(req.Reason)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s.Ex.CancelAll(ctx, s.Cfg.Exchange.InstID)
		}() // 触发即撤单
		s.Broadcast("kill_switch", map[string]any{"tripped": true, "reason": req.Reason})
		writeJSON(w, map[string]any{"tripped": true, "reason": req.Reason})
	case "reset":
		if req.Confirm != "RESET" {
			http.Error(w, "复位需要 confirm=RESET（人工确认）", http.StatusBadRequest)
			return
		}
		if s.Cfg.Mode == config.ModeLive {
			http.Error(w, "live 模式复位 Kill Switch 需重启进程（最强的门禁）", http.StatusForbidden)
			return
		}
		s.Rk.Kill.Reset()
		s.Broadcast("kill_switch", map[string]any{"tripped": false})
		writeJSON(w, map[string]any{"tripped": false})
	default:
		http.Error(w, "action 必须是 trip/reset", http.StatusBadRequest)
	}
}

// handleBacktest 用当前配置跑回测（研究入口；live 模式下也允许，只读数据面）。
func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	if s.RunBacktest == nil {
		http.Error(w, "未配置回测数据源", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.RunBacktest(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// nil 集合兜底为 []：前端对 null 取 .length 会崩整页
	if res.Trades == nil {
		res.Trades = []exchange.Order{}
	}
	if res.PendingOrders == nil {
		res.PendingOrders = []exchange.Order{}
	}
	if res.RiskRejections == nil {
		res.RiskRejections = []risk.Rejection{}
	}
	if res.EquityCurve == nil {
		res.EquityCurve = []backtest.EquityPoint{}
	}
	writeJSON(w, res)
}

// handleReviews 最近小时复盘记录（审计面）。
func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	if s.RecentReviews == nil {
		writeJSON(w, map[string]any{"reviews": nil})
		return
	}
	n := 10
	if v := r.URL.Query().Get("n"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 100 {
			n = p
		}
	}
	writeJSON(w, map[string]any{"reviews": s.RecentReviews(n)})
}

// handleGetStrategy 当前策略信息。
func (s *Server) handleGetStrategy(w http.ResponseWriter, r *http.Request) {
	desc := ""
	if s.CurrentStrategy != nil {
		desc = s.CurrentStrategy()
	}
	writeJSON(w, map[string]any{
		"name": s.Cfg.Strategy.Name, "desc": desc,
		"available": []string{"grid", "trend", "both"},
	})
}

// handleSwitchStrategy 热切策略（持仓中拒绝——退出规则不得悬空）。
func (s *Server) handleSwitchStrategy(w http.ResponseWriter, r *http.Request) {
	if s.SwitchStrategy == nil {
		http.Error(w, "本部署不支持策略切换", http.StatusNotImplemented)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name 必填", http.StatusBadRequest)
		return
	}
	if err := s.SwitchStrategy(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	s.Broadcast("strategy_switch", map[string]any{"name": req.Name})
	writeJSON(w, map[string]any{"switched": req.Name})
}

// handleResearchWF 研究工作台：walk-forward 样本外验证。
func (s *Server) handleResearchWF(w http.ResponseWriter, r *http.Request) {
	if s.RunWalkForward == nil {
		http.Error(w, "未配置研究数据源", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Strategy string `json:"strategy"`
		Train    int    `json:"train"`
		Test     int    `json:"test"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if req.Train <= 0 || req.Test <= 0 || req.Train > 5000 || req.Test > 2000 {
		http.Error(w, "train/test 越界（train≤5000, test≤2000）", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	rep, err := s.RunWalkForward(ctx, req.Strategy, req.Train, req.Test)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

// handleResearchUMP 研究工作台：UMP 拦截器样本外验证。
func (s *Server) handleResearchUMP(w http.ResponseWriter, r *http.Request) {
	if s.RunUMPCheck == nil {
		http.Error(w, "未配置研究数据源", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Strategy   string `json:"strategy"`
		MinSamples int    `json:"min_samples"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	n, rep, err := s.RunUMPCheck(ctx, req.Strategy, req.MinSamples)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"trade_samples": n, "report": rep})
}

// handleResearchPlateau 参数邻域高原检验（过拟合警报器）。
func (s *Server) handleResearchPlateau(w http.ResponseWriter, r *http.Request) {
	if s.RunPlateau == nil {
		http.Error(w, "未配置研究数据源", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Strategy string `json:"strategy"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	rep, err := s.RunPlateau(ctx, req.Strategy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

// handleResearchCostScan 成本敏感性扫描（2x 成本下转负即不可交易化）。
func (s *Server) handleResearchCostScan(w http.ResponseWriter, r *http.Request) {
	if s.RunCostScan == nil {
		http.Error(w, "未配置研究数据源", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Strategy string `json:"strategy"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	pts, err := s.RunCostScan(ctx, req.Strategy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pts == nil {
		pts = []lab.CostPoint{}
	}
	writeJSON(w, map[string]any{"points": pts})
}

// handleFills 成交历史（最近 200 条成交事件）。
func (s *Server) handleFills(w http.ResponseWriter, r *http.Request) {
	if s.Fills == nil {
		writeJSON(w, map[string]any{"fills": []execution.Event{}})
		return
	}
	fills := s.Fills()
	if fills == nil {
		fills = []execution.Event{}
	}
	writeJSON(w, map[string]any{"fills": fills})
}

// handleEquityCurve 权益曲线（复盘记录逐点：时间+权益）。
func (s *Server) handleEquityCurve(w http.ResponseWriter, r *http.Request) {
	type point struct {
		Ts     int64   `json:"ts"`
		Equity float64 `json:"equity"`
	}
	if s.EquityCurve == nil {
		writeJSON(w, map[string]any{"points": []point{}})
		return
	}
	var out []point
	for _, rec := range s.EquityCurve() {
		out = append(out, point{Ts: rec.Ts.UnixMilli(), Equity: rec.Equity})
	}
	if out == nil {
		out = []point{}
	}
	// 复盘倒序 → 时间升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, map[string]any{"points": out})
}

// handleCancel 手动撤单（应急控制：research 模式无执行器时报 503）。
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if s.CancelOrder == nil {
		http.Error(w, "本环境无执行器（撤单不可用）", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		OrderID string `json:"order_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.OrderID == "" {
		http.Error(w, "order_id 必填", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.CancelOrder(ctx, req.OrderID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Broadcast("cancel", map[string]any{"order_id": req.OrderID})
	writeJSON(w, map[string]any{"cancelled": req.OrderID})
}

// handleManualOrder 手动下单：必须走完整风控（限额/频率/现金校验），不得绕过。
func (s *Server) handleManualOrder(w http.ResponseWriter, r *http.Request) {
	if s.PlaceOrder == nil {
		http.Error(w, "本环境无执行器（手动下单不可用）", http.StatusServiceUnavailable)
		return
	}
	if s.ActiveMode != nil && s.ActiveMode() == config.ModeResearch {
		http.Error(w, "研究环境不下单（先切到模拟盘/实盘）", http.StatusForbidden)
		return
	}
	var req exchange.OrderRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	req.Symbol = s.Cfg.Exchange.InstID
	req.ClientOrderID = fmt.Sprintf("manual-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	o, err := s.PlaceOrder(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Broadcast("manual_order", map[string]any{"order_id": o.OrderID})
	writeJSON(w, o)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	// 只读视图；密钥不在 config 里（走环境变量），无脱敏需求
	writeJSON(w, s.Cfg.Sanitized())
}

// handleSSE 事件流。
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, "event: hello\ndata: {}\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", msg)
			fl.Flush()
		}
	}
}

// handleStatic 前端静态文件（web/dist 构建产物复制到 dashboard/webdist）。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if staticFS == nil {
		http.Error(w, "前端未构建：cd web && npm run build && cp -r dist ../dashboard/webdist/", http.StatusNotFound)
		return
	}
	http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
}
