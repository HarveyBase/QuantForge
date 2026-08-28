// Package review 运行态小时级复盘：每小时暂停交易、生成结构化复盘记录并落盘。
// 纪律（docs/01、docs/08）：复盘是审计产物——口径可追溯、结论标注阶段（research/paper/live）、
// 失败与异常留痕不静默；复盘记录不构成投资建议。
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/market"
)

const Version = 1

// FillSummary 窗口内成交摘要。
type FillSummary struct {
	Ts    time.Time `json:"ts"`
	Side  string    `json:"side"`
	Qty   float64   `json:"qty"`
	Price float64   `json:"price"`
	Note  string    `json:"note,omitempty"`
}

// RejSummary 窗口内风控拒单摘要。
type RejSummary struct {
	Ts     time.Time `json:"ts"`
	RuleID string    `json:"rule_id"`
	Reason string    `json:"reason"`
}

// Record 单次复盘记录（每小时一份）。
type Record struct {
	Version    int       `json:"version"`
	Stage      string    `json:"stage"` // research / paper / live（表述红线：不得跨阶段暗示）
	Ts         time.Time `json:"ts"`    // 复盘时刻
	WindowFrom time.Time `json:"window_from"`
	WindowTo   time.Time `json:"window_to"`
	Symbol     string    `json:"symbol"`
	Interval   string    `json:"interval"`

	Equity       float64 `json:"equity"`
	EquityStart  float64 `json:"equity_start"` // 窗口起始权益（上次复盘时）
	WindowRetPct float64 `json:"window_ret_pct"`

	Cash        float64 `json:"cash"`
	PriceFirst  float64 `json:"price_first"` // 窗口首尾收盘价
	PriceLast   float64 `json:"price_last"`
	PriceChgPct float64 `json:"price_chg_pct"`
	CandlesSeen int     `json:"candles_seen"` // 窗口内已收盘 K 线数（数据连续性检查）

	Fills       []FillSummary `json:"fills"`
	Rejections  []RejSummary  `json:"rejections"`
	OpenOrders  int           `json:"open_orders"`
	UMPBlocked  int           `json:"ump_blocked"`
	KillTripped bool          `json:"kill_tripped"`
	KillReason  string        `json:"kill_reason,omitempty"`

	Strategy string   `json:"strategy"` // 策略运行摘要（由调用方注入）
	Notes    []string `json:"notes"`    // 自动诊断结论（口径见 diagnose）

	MaxFillsPerHour int `json:"max_fills_per_hour"` // 过频判定阈值（复盘参数留痕）
}

// Input 复盘数据采集（由 serve 注入；全部字段取自实时账本，无前视问题）。
// collect 收到窗口起点 from，据此过滤窗口内事件与 K 线。
type Input struct {
	Stage       string
	Symbol      string
	Interval    string
	Equity      float64
	Cash        float64
	Fills       []FillSummary
	Rejections  []RejSummary
	UMPBlocked  int
	OpenOrders  int
	KillTripped bool
	KillReason  string
	PriceFirst  float64
	PriceLast   float64
	CandlesSeen int
	Strategy    string
}

// Reviewer 小时级复盘器：窗口收益基准（lastEquity）由自身维护。
type Reviewer struct {
	mu         sync.Mutex
	dir        string        // data/reviews
	every      time.Duration // 复盘周期（默认 1h）
	first      bool          // 首次复盘（无窗口基准）
	lastTs     time.Time
	lastEquity float64
	collect    func(from time.Time) Input
}

// New 构造复盘器。collect 必须非 nil（采集失败即复盘失败，不静默）。
func New(dataDir string, every time.Duration, collect func(from time.Time) Input) (*Reviewer, error) {
	if collect == nil {
		return nil, fmt.Errorf("review: 采集函数不能为空")
	}
	if every <= 0 {
		every = time.Hour
	}
	return &Reviewer{
		dir:     filepath.Join(dataDir, "reviews"),
		every:   every,
		first:   true,
		collect: collect,
	}, nil
}

// Every 复盘周期。
func (r *Reviewer) Every() time.Duration { return r.every }

// ReviewOnce 执行一次复盘：采集 → 计算窗口指标 → 诊断 → 落盘，返回记录。
// 首次复盘只建立基准（无窗口收益），同样落盘留痕。
func (r *Reviewer) ReviewOnce() (*Record, error) {
	r.mu.Lock()
	now := time.Now().UTC()
	from := now.Add(-r.every)
	if !r.first {
		from = r.lastTs
	}
	r.mu.Unlock()
	in := r.collect(from)
	r.mu.Lock()
	defer r.mu.Unlock()
	now = time.Now().UTC()
	rec := &Record{
		Version: Version, Stage: in.Stage, Ts: now, WindowTo: now,
		Symbol: in.Symbol, Interval: in.Interval,
		Equity: in.Equity, EquityStart: in.Equity, Cash: in.Cash,
		PriceFirst: in.PriceFirst, PriceLast: in.PriceLast,
		CandlesSeen: in.CandlesSeen,
		Fills:       in.Fills, Rejections: in.Rejections, OpenOrders: in.OpenOrders, UMPBlocked: in.UMPBlocked,
		KillTripped: in.KillTripped, KillReason: in.KillReason,
		Strategy:        in.Strategy,
		MaxFillsPerHour: maxFillsPerHour,
	}
	if !r.first {
		rec.WindowFrom = r.lastTs
		rec.EquityStart = r.lastEquity
		if r.lastEquity > 0 {
			rec.WindowRetPct = (in.Equity/r.lastEquity - 1) * 100
		}
	} else {
		rec.WindowFrom = now.Add(-r.every) // 首次窗口按周期回溯（无交易基准）
	}
	if in.PriceFirst > 0 && in.PriceLast > 0 {
		rec.PriceChgPct = (in.PriceLast/in.PriceFirst - 1) * 100
	}
	rec.Notes = diagnose(rec, r.every)
	r.first = false
	r.lastTs = now
	r.lastEquity = in.Equity
	if err := r.persist(rec); err != nil {
		return rec, fmt.Errorf("review: 复盘落盘失败: %w", err)
	}
	return rec, nil
}

// maxFillsPerHour 过频判定阈值：1 小时窗口内成交超过此数视为交易过频。
const maxFillsPerHour = 6

// diagnose 自动诊断（每条结论口径明确、可证伪）。
func diagnose(rec *Record, every time.Duration) []string {
	var notes []string
	// 1. 窗口收益 vs 买入持有（价格变动）
	if rec.WindowRetPct != 0 || rec.PriceChgPct != 0 {
		diff := rec.WindowRetPct - rec.PriceChgPct
		verb := "持平"
		switch {
		case diff > 0.01:
			verb = "跑赢"
		case diff < -0.01:
			verb = "跑输"
		}
		notes = append(notes, fmt.Sprintf("窗口收益 %.2f%% vs 买入持有 %.2f%%（%s，%s 阶段口径，不代表实盘）",
			rec.WindowRetPct, rec.PriceChgPct, verb, rec.Stage))
	}
	// 2. 拒单归因
	if n := len(rec.Rejections); n > 0 {
		byRule := map[string]int{}
		for _, rej := range rec.Rejections {
			byRule[rej.RuleID]++
		}
		rules := make([]string, 0, len(byRule))
		for k := range byRule {
			rules = append(rules, k)
		}
		sort.Strings(rules)
		top := ""
		for _, k := range rules {
			if top != "" {
				top += "，"
			}
			top += fmt.Sprintf("%s×%d", k, byRule[k])
		}
		notes = append(notes, fmt.Sprintf("风控拒单 %d 笔：%s（拒单不静默，全部留痕）", n, top))
	}
	// 3. 交易过频检查（"不要太频繁"的量化护栏；无成交留给"市况平静"兜底）
	if n := len(rec.Fills); n > maxFillsPerHour {
		notes = append(notes, fmt.Sprintf("警告：窗口内成交 %d 笔超过阈值 %d——交易过频，检查策略冷却与网格密度", n, maxFillsPerHour))
	} else if n > 0 {
		notes = append(notes, fmt.Sprintf("窗口内成交 %d 笔（频率护栏内）", n))
	}
	// 3.5 UMP 拦截留痕（拦截不静默）
	if rec.UMPBlocked > 0 {
		notes = append(notes, fmt.Sprintf("UMP 拦截 %d 个买入信号（历史同情境低胜率，防失败模式复现）", rec.UMPBlocked))
	}
	// 4. Kill Switch 状态
	if rec.KillTripped {
		notes = append(notes, fmt.Sprintf("Kill Switch 处于触发状态（%s）：禁止一切新下单，需人工复位", rec.KillReason))
	}
	// 5. 数据连续性：按周期估算应有 K 线数（容差 2 根）
	if rec.Interval != "" {
		if want := int(every / intervalApprox(rec.Interval)); want > 0 && rec.CandlesSeen+2 < want {
			notes = append(notes, fmt.Sprintf("数据连续性异常：窗口应见约 %d 根 %s K 线，实际 %d 根（行情断流或拉取失败）",
				want, rec.Interval, rec.CandlesSeen))
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "窗口内无事件，市况平静")
	}
	return notes
}

// intervalApprox 周期近似时长（复盘诊断容差用，非精确换算）。
func intervalApprox(interval string) time.Duration {
	if ms := market.IntervalMs(interval); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return time.Hour
}

// persist 双写：归档 JSON + ledger.jsonl 索引（与 backtests 目录同风格）。
func (r *Reviewer) persist(rec *Record) error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	name := rec.Ts.Format("20060102_150405.000000000") // 纳秒精度：连续复盘不得互相覆盖
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.dir, name+".json"), b, 0o644); err != nil {
		return err
	}
	ledger, _ := json.Marshal(map[string]any{
		"ts": rec.Ts.Format(time.RFC3339), "file": name + ".json",
		"stage": rec.Stage, "window_ret_pct": rec.WindowRetPct, "price_chg_pct": rec.PriceChgPct,
		"fills": len(rec.Fills), "rejections": len(rec.Rejections), "kill_tripped": rec.KillTripped,
	})
	f, err := os.OpenFile(filepath.Join(r.dir, "ledger.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(ledger, '\n'))
	return err
}

// LatestRecent 读最近 n 份复盘（dashboard 查询用；文件名即时间序）。
func (r *Reviewer) Recent(n int) []Record {
	r.mu.Lock()
	dir := r.dir
	r.mu.Unlock()
	if n <= 0 {
		n = 10
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" && e.Name() != "ledger.jsonl" {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // 文件名时间序倒排
	out := make([]Record, 0, n)
	for _, name := range names {
		if len(out) >= n {
			break
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(b, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// Loop 周期复盘直到 ctx 取消；错误留痕不中断（复盘失败不得杀死交易进程，但必须可见）。
func (r *Reviewer) Loop(ctx context.Context, onErr func(error)) {
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.ReviewOnce(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
