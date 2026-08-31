package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/review"
)

func TestTelegramSendAndFormat(t *testing.T) {
	type payload struct{ chatID, text string }
	got := make(chan payload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken123/sendMessage" {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		var p map[string]string
		json.NewDecoder(r.Body).Decode(&p)
		w.Write([]byte(`{"ok":true}`))
		select {
		case got <- payload{p["chat_id"], p["text"]}:
		default:
		}
	}))
	defer srv.Close()

	tg := newTelegram(srv.URL, "token123", "42")
	go tg.loop("paper", "BTC-USDT")
	tg.Send("测试告警")
	select {
	case p := <-got:
		if p.chatID != "42" {
			t.Fatalf("chat_id 错误: %s", p.chatID)
		}
		if !contains(p.text, "[QuantForge paper BTC-USDT]") || !contains(p.text, "测试告警") {
			t.Fatalf("告警格式错误: %s", p.text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("告警未送达")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestCriticalFilter(t *testing.T) {
	// Kill 触发
	rec := review.Record{KillTripped: true, KillReason: "当日回撤超限",
		Notes: []string{"窗口收益 1% vs 买入持有 2%"}, Rejections: nil}
	got := Critical(rec)
	if len(got) != 1 || !contains(got[0], "Kill") || !contains(got[0], "当日回撤超限") {
		t.Fatalf("Kill 严重项: %v", got)
	}
	// 断流 + 过频 + 连续拒单
	rec2 := review.Record{
		Notes:      []string{"数据连续性异常：应见 60 根实见 10 根", "警告：窗口内成交 9 笔超过阈值 6——交易过频"},
		Rejections: make([]review.RejSummary, 5),
	}
	got2 := Critical(rec2)
	if len(got2) != 3 {
		t.Fatalf("应有 3 条严重项: %v", got2)
	}
	// 正常复盘不告警
	rec3 := review.Record{Notes: []string{"窗口收益 1%", "窗口内成交 2 笔（频率护栏内）"}}
	if got3 := Critical(rec3); len(got3) != 0 {
		t.Fatalf("正常复盘不应告警: %v", got3)
	}
}

func TestLogSink(t *testing.T) {
	s := &logSink{}
	if s.Enabled() {
		t.Fatal("兜底实现不应启用")
	}
	s.Send("test") // 只进日志，不 panic
}
