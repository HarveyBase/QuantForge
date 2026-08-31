package okx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSCandlesClosedCandleTrigger(t *testing.T) {
	closed := make(chan int64, 4)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// 读订阅请求
		_, sub, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if !contains(string(sub), `"channel":"candle1H"`) || !contains(string(sub), `"instId":"BTC-USDT"`) {
			t.Errorf("订阅参数错误: %s", sub)
		}
		// 推送：未收盘根（confirm=0，不触发）→ 已收盘根（触发）→ ping（应答）
		conn.WriteMessage(websocket.TextMessage, []byte(
			`{"arg":{"channel":"candle1H","instId":"BTC-USDT"},"data":[["1700000000000","100","101","99","100","1","0","0","0"]]}`))
		conn.WriteMessage(websocket.TextMessage, []byte(
			`{"arg":{"channel":"candle1H","instId":"BTC-USDT"},"data":[["1700003600000","100","101","99","100","1","0","0","1"]]}`))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"ping"}`))
		// 等 pong 后收尾
		deadline := time.Now().Add(2 * time.Second)
		conn.SetReadDeadline(deadline)
		for time.Now().Before(deadline) {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}))
	defer srv.Close()

	ws := &WSCandles{URL: "ws" + srv.URL[len("http"):], Symbol: "BTC-USDT", Interval: "1H",
		OnCandleClosed: func(ts int64) { closed <- ts },
		dialer:         &websocket.Dialer{HandshakeTimeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go ws.Run(ctx)
	select {
	case ts := <-closed:
		if ts != 1700003600000 {
			t.Fatalf("收盘根时间戳错误: %d", ts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("已收盘根未触发回调")
	}
	// 确认未收盘根没有触发第二条
	select {
	case ts := <-closed:
		t.Fatalf("未收盘根不应触发: %d", ts)
	case <-time.After(300 * time.Millisecond):
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
