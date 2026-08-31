// Package okx WebSocket 行情：订阅 candle 频道，已收盘 K 线即时触发回调。
// 数据纪律（docs/02）：WS 只做"及时触发器"——收到 confirm=1 的收盘根后回调
// OnCandleClosed，由上层执行一次 REST 校验拉取（走 market.Validate 完整链），
// WS 数据不直接入库/驱动策略，防推送乱序与断线丢根。
package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// WSCandles candle 频道订阅客户端（自动重连，退避 5s→30s）。
type WSCandles struct {
	URL      string // 默认 wss://ws.okx.com:8443/ws/v5/public
	Symbol   string
	Interval string

	OnCandleClosed func(ts int64) // 已收盘根 OpenTime（上层触发 REST 校验拉取）
	OnError        func(error)

	dialer *websocket.Dialer
}

// NewWSCandles 构造（URL 空用默认公共频道）。
func NewWSCandles(symbol, interval string) *WSCandles {
	return &WSCandles{
		URL:    "wss://ws.okx.com:8443/ws/v5/public",
		Symbol: symbol, Interval: interval,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
			Proxy:            http.ProxyFromEnvironment, // 走环境代理（https_proxy）
		},
	}
}

// WithHandler 设置回调（链式）。
func (w *WSCandles) WithHandler(onClosed func(int64), onError func(error)) *WSCandles {
	w.OnCandleClosed, w.OnError = onClosed, onError
	return w
}

// Run 阻塞运行直到 ctx 取消；断线自动重连（REST 轮询兜底期间不影响交易）。
func (w *WSCandles) Run(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if w.OnError != nil {
			w.OnError(fmt.Errorf("ws 断开（%.0fs 后重连）: %w", backoff.Seconds(), err))
		} else {
			log.Printf("okx ws 断开（%.0fs 后重连）: %v", backoff.Seconds(), err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff += 5 * time.Second
		}
	}
}

func (w *WSCandles) runOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := w.dialer.DialContext(dialCtx, w.URL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 订阅 candle 频道
	sub := map[string]any{
		"op":   "subscribe",
		"args": []map[string]string{{"channel": "candle" + w.Interval, "instId": w.Symbol}},
	}
	b, _ := json.Marshal(sub)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		return err
	}
	// 心跳：服务端 ping 用 text "pong" 应答（OKX 约定）
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env struct {
			Event string `json:"event"`
			Arg   struct {
				Channel string `json:"channel"`
				InstID  string `json:"instId"`
			} `json:"arg"`
			Data [][]string `json:"data"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			continue // 非 JSON/订阅回执忽略
		}
		if env.Event == "ping" {
			conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			continue
		}
		if env.Arg.Channel != "candle"+w.Interval {
			continue
		}
		for _, row := range env.Data {
			if len(row) < 9 || row[8] != "1" {
				continue // 只认已收盘根
			}
			ts, err := strconv.ParseInt(row[0], 10, 64)
			if err != nil {
				continue
			}
			if w.OnCandleClosed != nil {
				w.OnCandleClosed(ts)
			}
		}
	}
}
