// limiter_test.go 限速器与限流信号识别测试：
// ①令牌耗尽排队且 ctx 取消立即返回 ②OKX 限流响应（业务码/HTTP 429）→ IsRateLimitError。
package okx

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

// ① 令牌耗尽时请求排队；等待期间 ctx 取消立即返回错误。
func TestRateLimiterQueuesAndHonorsCancel(t *testing.T) {
	l := newRateLimiter(4) // 250ms 间隔
	if err := l.acquire(context.Background()); err != nil {
		t.Fatalf("首个令牌应立即可用: %v", err)
	}
	// 第二个令牌需排队约一个间隔（250ms）
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- l.acquire(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("第二个令牌不应提前发放（应排队等待）: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := <-done; err != nil {
		t.Fatalf("排队后应正常获得令牌: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("第二个令牌应等待约一个间隔，实际 %v", elapsed)
	}

	// 令牌耗尽状态下等待：ctx 取消必须立即返回而非死等下一个令牌
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	s := time.Now()
	err := l.acquire(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("等待期间 ctx 取消应返回 context.Canceled: %v", err)
	}
	if e := time.Since(s); e > 200*time.Millisecond {
		t.Fatalf("ctx 取消应立即返回，实际等待 %v", e)
	}
	// 已取消的 ctx 直接失败
	if err := l.acquire(ctx); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应直接失败: %v", err)
	}
}

// ① 直通：nil 限速器 / 0 速率不限速（直接构造的 Client 便于测试注入）。
func TestRateLimiterPassthrough(t *testing.T) {
	var nilLimiter *rateLimiter
	if err := nilLimiter.acquire(context.Background()); err != nil {
		t.Fatalf("nil 限速器应直通: %v", err)
	}
	if err := newRateLimiter(0).acquire(context.Background()); err != nil {
		t.Fatalf("0 速率应直通: %v", err)
	}
}

// ① do 入口接线：令牌耗尽 + ctx 取消 → 请求立即失败，不发出 HTTP。
func TestClientDoAcquireHonorsCancel(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"code":"0","data":[{"last":"1","ts":"1"}]}`))
	})
	c := newMockClient(t, mux)
	c.limiter = newRateLimiter(2) // 500ms 间隔
	if _, err := c.GetTicker(context.Background(), "BTC-USDT"); err != nil {
		t.Fatal(err) // 首个请求立即放行
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.GetTicker(ctx, "BTC-USDT")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("限速等待被取消应返回 ctx 错误: %v", err)
	}
	if e := time.Since(start); e > 300*time.Millisecond {
		t.Fatalf("应立即失败而非等待下一个令牌（500ms）: %v", e)
	}
	if hits != 1 {
		t.Fatalf("被限速阻塞的请求不应发出 HTTP: hits=%d", hits)
	}
}

// 默认限速 10 req/s；WithRateLimit 可覆盖/关闭。
func TestRateLimitDefaultsAndOverride(t *testing.T) {
	if got := NewLive("cross", 1).limiter.interval; got != 100*time.Millisecond {
		t.Fatalf("默认限速应为 10 req/s（100ms 间隔）: %v", got)
	}
	if got := NewLive("cross", 1, WithRateLimit(100)).limiter.interval; got != 10*time.Millisecond {
		t.Fatalf("WithRateLimit(100) 应为 10ms 间隔: %v", got)
	}
	if got := NewLive("cross", 1, WithRateLimit(0)).limiter.interval; got != 0 {
		t.Fatalf("WithRateLimit(0) 应关闭限速: %v", got)
	}
}

// ② 限流业务码（50011/50013/50026/50057）→ 包装 exchange.ErrRateLimited，且保留原始 code/msg。
func TestOKXRateLimitBusinessCodes(t *testing.T) {
	for _, code := range []string{"50011", "50013", "50026", "50057"} {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"code":"` + code + `","msg":"Rate limit reached"}`))
		})
		c := newMockClient(t, mux)
		_, err := c.GetTicker(context.Background(), "BTC-USDT")
		if err == nil || !exchange.IsRateLimitError(err) {
			t.Fatalf("业务码 %s 必须识别为限流: %v", code, err)
		}
		if !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), "Rate limit reached") {
			t.Fatalf("错误文本须保留原始 code/msg: %v", err)
		}
	}
}

// ② HTTP 429 → 限流。
func TestOKXRateLimitHTTP429(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`too many requests`))
	})
	c := newMockClient(t, mux)
	_, err := c.GetTicker(context.Background(), "BTC-USDT")
	if err == nil || !exchange.IsRateLimitError(err) {
		t.Fatalf("HTTP 429 必须识别为限流: %v", err)
	}
}

// 非限流业务码/HTTP 错误不得误标限流。
func TestOKXOtherErrorsNotRateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"51001","msg":"Instrument ID does not exist"}`))
	})
	mux.HandleFunc("/api/v5/market/books", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":"0"}`))
	})
	c := newMockClient(t, mux)
	if _, err := c.GetTicker(context.Background(), "BAD-PAIR"); err == nil || exchange.IsRateLimitError(err) {
		t.Fatalf("51001 不应识别为限流: %v", err)
	}
	if _, err := c.GetOrderBook(context.Background(), "BTC-USDT", 5); err == nil || exchange.IsRateLimitError(err) {
		t.Fatalf("HTTP 403 不应识别为限流: %v", err)
	}
}
