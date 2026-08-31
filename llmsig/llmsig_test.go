package llmsig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func c(ot int64, px float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: px, High: px * 1.01, Low: px * 0.99, Close: px, Confirmed: true}
}

func series(n int, drift float64) []exchange.Candle {
	out := make([]exchange.Candle, 0, n)
	px := 100.0
	for i := 0; i < n; i++ {
		px *= 1 + drift
		out = append(out, c(int64(i+1), px))
	}
	return out
}

func TestRuleBaselineSignals(t *testing.T) {
	up := RuleBaseline(series(100, 0.01))
	if up.Signal != Long || up.Confidence <= 0 {
		t.Fatalf("上涨序列应 long: %+v", up)
	}
	if !strings.Contains(up.Reason, "SMA20") {
		t.Fatalf("证据必须溯源: %s", up.Reason)
	}
	down := RuleBaseline(series(100, -0.01))
	if down.Signal != Short {
		t.Fatalf("下跌序列应 short: %+v", down)
	}
	// 弱趋势 neutral（宁可不说不说错）
	flat := RuleBaseline(series(200, 0.0001))
	if flat.Signal != Neutral {
		t.Fatalf("弱趋势应 neutral: %+v", flat)
	}
	// 样本不足
	ins := RuleBaseline(series(30, 0.01))
	if ins.Signal != Neutral || len(ins.ReviewFlags) == 0 {
		t.Fatalf("样本不足应 neutral + reviewFlag: %+v", ins)
	}
	// 置信度封顶 90
	big := RuleBaseline(series(100, 0.05))
	if big.Confidence > 90 {
		t.Fatalf("规则基线封顶 90: %d", big.Confidence)
	}
}

type mockProvider struct {
	resp string
	err  error
}

func (m *mockProvider) Ask(ctx context.Context, prompt string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.resp, nil
}

func TestEvaluatorLLMParse(t *testing.T) {
	// 合法 LLM 输出（带前后噪音）
	e := New(&mockProvider{resp: "废话 {\"signal\":\"short\",\"confidence\":88,\"reason\":\"跌破关键支撑\"} 结尾"})
	out := e.Evaluate(context.Background(), series(100, 0.01))
	if out.Source != "llm" || out.Signal != Short || out.Confidence != 88 {
		t.Fatalf("LLM 覆盖失败: %+v", out)
	}
	// 非法枚举 → 回退 + reviewFlag
	e2 := New(&mockProvider{resp: `{"signal":"yolo","confidence":99,"reason":"x"}`})
	out2 := e2.Evaluate(context.Background(), series(100, 0.01))
	if out2.Source != "rule-baseline" || !hasFlag(out2.ReviewFlags, "LLM_INVALID_OUTPUT") {
		t.Fatalf("非法输出必须回退: %+v", out2)
	}
	// LLM 出错 → 回退 + flag
	e3 := New(&mockProvider{err: errors.New("timeout")})
	out3 := e3.Evaluate(context.Background(), series(100, 0.01))
	if !hasFlag(out3.ReviewFlags, "LLM_FALLBACK") || !strings.Contains(out3.Reason, "回退") {
		t.Fatalf("LLM 失败必须回退留痕: %+v", out3)
	}
	// confidence 越界夹取
	e4 := New(&mockProvider{resp: `{"signal":"long","confidence":250,"reason":"ok"}`})
	if out4 := e4.Evaluate(context.Background(), series(100, 0.01)); out4.Confidence != 100 {
		t.Fatalf("confidence 夹取 100: %+v", out4)
	}
	// reason 为空 → 非法
	e5 := New(&mockProvider{resp: `{"signal":"long","confidence":50,"reason":""}`})
	if out5 := e5.Evaluate(context.Background(), series(100, 0.01)); out5.Source != "rule-baseline" {
		t.Fatalf("空 reason 非法: %+v", out5)
	}
	// nil provider 永远走基线
	out6 := New(nil).Evaluate(context.Background(), series(100, 0.01))
	if out6.Source != "rule-baseline" {
		t.Fatal("nil provider 走基线")
	}
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

func TestBuildPromptNumbers(t *testing.T) {
	cs := series(80, 0.005)
	base := RuleBaseline(cs)
	p := BuildPrompt(cs, base)
	for _, want := range []string{"80 根", "SMA20"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 缺 %q: %s", want, p)
		}
	}
}
