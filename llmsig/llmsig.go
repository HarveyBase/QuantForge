// Package llmsig LLM 结构化信号层（docs/03 口径）：
// 输出收敛到固定枚举（long/neutral/short）+ 有界数值（confidence 0-100）+ 证据溯源；
// LLM 失败必须回退规则基线并记录 note——LLM 只做解释层，不做决策层。
package llmsig

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/indicators"
)

// Signal 固定枚举信号。
type Signal string

const (
	Long    Signal = "long"
	Neutral Signal = "neutral"
	Short   Signal = "short"
)

// Output 结构化输出（docs/03：枚举 + 有界 + 溯源 + reviewFlags）。
type Output struct {
	Signal      Signal   `json:"signal"`                 // long/neutral/short
	Confidence  int      `json:"confidence"`             // 0-100（有界；不是上涨概率）
	Reason      string   `json:"reason"`                 // 证据溯源（可追溯到输入字段）
	Source      string   `json:"source"`                 // llm / rule-baseline（回退标注）
	ReviewFlags []string `json:"review_flags,omitempty"` // 需人工复核的异常
}

// Provider LLM 提供方（接口注入；生产用 OpenAI 兼容端点，测试用 mock）。
type Provider interface {
	Ask(ctx context.Context, prompt string) (string, error)
}

// Evaluator 信号评估器：K 线摘要 → Provider → 结构化解析 → 校验失败回退规则基线。
type Evaluator struct {
	provider Provider // nil = 永远走规则基线（研究口径）
}

// New 构造（provider 可为 nil）。
func New(p Provider) *Evaluator { return &Evaluator{provider: p} }

// OpenAICompatEnv 从环境变量构造 OpenAI 兼容 Provider
// （LLM_ENDPOINT / LLM_API_KEY / LLM_MODEL，任一缺失返回 nil）。
func OpenAICompatEnv() Provider {
	endpoint := os.Getenv("LLM_ENDPOINT")
	key := os.Getenv("LLM_API_KEY")
	model := os.Getenv("LLM_MODEL")
	if endpoint == "" || key == "" || model == "" {
		return nil
	}
	return &openAICompat{endpoint: endpoint, key: key, model: model, http: &http.Client{Timeout: 30 * time.Second}}
}

type openAICompat struct {
	endpoint string
	key      string
	model    string
	http     *http.Client
}

func (o *openAICompat) Ask(ctx context.Context, prompt string) (string, error) {
	body := map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是量化信号解释层。只输出 JSON：{\"signal\":\"long|neutral|short\",\"confidence\":0-100,\"reason\":\"一句话证据\"}。confidence 不是上涨概率。"},
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint, strings.NewReader(string(b)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.key)
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("LLM 响应解析失败")
	}
	return out.Choices[0].Message.Content, nil
}

// Evaluate 评估一组已收盘 K 线：先规则基线，再（可选）LLM 覆盖；LLM 失败回退。
// 防前视：candles 必须全部已收盘（由调用方保证）。
func (e *Evaluator) Evaluate(ctx context.Context, candles []exchange.Candle) Output {
	base := RuleBaseline(candles)
	if e.provider == nil {
		return base
	}
	prompt := BuildPrompt(candles, base)
	raw, err := e.provider.Ask(ctx, prompt)
	if err != nil {
		out := base
		out.Reason = fmt.Sprintf("LLM 失败回退规则基线（%v）；%s", err, base.Reason)
		out.ReviewFlags = append(out.ReviewFlags, "LLM_FALLBACK")
		return out
	}
	parsed, perr := parseStrict(raw)
	if perr != nil {
		out := base
		out.Reason = fmt.Sprintf("LLM 输出非法（%v）回退规则基线；%s", perr, base.Reason)
		out.ReviewFlags = append(out.ReviewFlags, "LLM_INVALID_OUTPUT")
		return out
	}
	parsed.Source = "llm"
	return parsed
}

// parseStrict 严格解析：枚举合法 + confidence 夹取 [0,100]；reason 非空。
func parseStrict(raw string) (Output, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var o Output
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return o, fmt.Errorf("非 JSON: %.80s", raw)
	}
	switch o.Signal {
	case Long, Neutral, Short:
	default:
		return Output{}, fmt.Errorf("枚举非法 %q", o.Signal)
	}
	if o.Confidence < 0 {
		o.Confidence = 0
	}
	if o.Confidence > 100 {
		o.Confidence = 100
	}
	if strings.TrimSpace(o.Reason) == "" {
		return Output{}, fmt.Errorf("reason 为空")
	}
	return o, nil
}

// RuleBaseline 规则基线（LLM 不可用时的回退，也作为 LLM 的对照）：
// 双均线趋向 + 波动过滤——弱趋势一律 neutral（宁可不说不说错）。
func RuleBaseline(candles []exchange.Candle) Output {
	n := len(candles)
	if n < 60 {
		return Output{Signal: Neutral, Confidence: 0, Reason: "样本不足 60 根，证据不足", Source: "rule-baseline", ReviewFlags: []string{"INSUFFICIENT_DATA"}}
	}
	closes := make([]float64, n)
	for i, c := range candles {
		closes[i] = c.Close
	}
	fast := indicators.SMA(closes, 20)[n-1]
	slow := indicators.SMA(closes, 60)[n-1]
	if math.IsNaN(fast) || math.IsNaN(slow) || slow == 0 {
		return Output{Signal: Neutral, Confidence: 0, Reason: "指标预热不足", Source: "rule-baseline"}
	}
	diffPct := (fast/slow - 1) * 100
	switch {
	case diffPct > 1.0:
		return Output{Signal: Long, Confidence: clamp(int(diffPct * 20)), Source: "rule-baseline",
			Reason: fmt.Sprintf("SMA20 高于 SMA60 %.2f%%（多头排列，证据：收盘价序列）", diffPct)}
	case diffPct < -1.0:
		return Output{Signal: Short, Confidence: clamp(int(-diffPct * 20)), Source: "rule-baseline",
			Reason: fmt.Sprintf("SMA20 低于 SMA60 %.2f%%（空头排列，证据：收盘价序列）", diffPct)}
	default:
		return Output{Signal: Neutral, Confidence: clamp(int(math.Abs(diffPct) * 20)), Source: "rule-baseline",
			Reason: fmt.Sprintf("SMA20/60 偏离仅 %.2f%%（趋势证据不足，弱信号一律 neutral）", diffPct)}
	}
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	if v > 90 {
		return 90 // 规则基线置信度封顶 90（永远保留不确定性）
	}
	return v
}

// BuildPrompt 构造 LLM 输入（摘要统计 + 规则基线对照，防幻觉：每个数字来自输入）。
func BuildPrompt(candles []exchange.Candle, baseline Output) string {
	n := len(candles)
	first, last := candles[0], candles[n-1]
	retPct := (last.Close/first.Close - 1) * 100
	var hi, lo float64
	for _, c := range candles {
		hi = math.Max(hi, c.High)
		lo = math.Min(lo, c.Low)
		if lo == 0 {
			lo = c.Low
		}
	}
	return fmt.Sprintf(`已收盘 K 线统计（%d 根，区间收益 %.2f%%，最高 %.2f 最低 %.2f，最新收盘 %.2f）。
规则基线判定：%s（SMA20/60 偏离）。请基于以上数字给出 JSON 信号；不得引用未提供的数字。`,
		n, retPct, hi, lo, last.Close, baseline.Signal)
}
