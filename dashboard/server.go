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
	"github.com/HarveyBase/QuantForge/grid"
	"github.com/HarveyBase/QuantForge/portfolio"
	"github.com/HarveyBase/QuantForge/regime"
	"github.com/HarveyBase/QuantForge/review"
	"github.com/HarveyBase/QuantForge/risk"
	"strconv"
)

// Server 后台服务。
type Server struct {
	Cfg           *config.Config
	Pf            *portfolio.Portfolio
	Rk            *risk.Manager
	Ex            OrderSource
	Grid          *grid.Grid
	Snapshots     func() []exchange.Candle                           // 最近已确认 K 线
	Candles       func(interval string, limit int) []exchange.Candle // SQLite 库读取（任意周期/全历史）
	RunBacktest   func(ctx context.Context) (*backtest.Result, error)
	RecentReviews func(n int) []review.Record // 最近 n 份小时复盘
	Regime        func() regime.Reading       // 当前市况读数
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
	mux.HandleFunc("GET /api/grid", s.auth(s.handleGrid))
	mux.HandleFunc("GET /api/mode", s.auth(s.handleGetMode))
	mux.HandleFunc("POST /api/mode", s.auth(s.handleSwitchMode))
	mux.HandleFunc("POST /api/killswitch", s.auth(s.handleKillSwitch))
	mux.HandleFunc("POST /api/backtest", s.auth(s.handleBacktest))
	mux.HandleFunc("GET /api/reviews", s.auth(s.handleReviews))
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
