// limiter_test.go Binance REST 限速器与限流信号识别测试：
// 令牌耗尽排队、ctx 取消立即返回、429/-1003/-1021 打上 ErrRateLimited 标记、418 注明封禁。
package binance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

// 限速器：超速请求排队等待，且等待期间 ctx 取消立即返回错误（不死等）。
func TestRateLimiterQueuesAndRespectsCtx(t *testing.T) {
	l := newRateLimiter(100) // 10ms 间隔
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// 第二个请求必须等待约一个 interval
	start := time.Now()
	if err := l.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 5*time.Millisecond {
		t.Fatalf("超速请求应排队至少半个 interval, 实际 %v", d)
	}
	// 排队期间取消：立即返回错误
	ctx2, cancel2 := context.WithCancel(context.Background())
	if err := l.acquire(ctx2); err != nil {
		t.Fatal(err)
	}
	cancel2()
	if err := l.acquire(ctx2); err == nil {
		t.Fatal("ctx 取消后获取令牌必须立即失败，不得死等")
	}
}

// 限速器并发安全：并发 acquire 总耗时 >= 请求数 × interval（-race 下无竞争）。
func TestRateLimiterConcurrent(t *testing.T) {
	l := newRateLimiter(1000) // 1ms 间隔
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.acquire(context.Background())
		}()
	}
	wg.Wait()
	if d := time.Since(start); d < 15*time.Millisecond {
		t.Fatalf("20 个并发请求应串行化至少 ~19ms, 实际 %v", d)
	}
}

// WithRateLimit 选项：关闭限速（<=0）直通。
func TestWithRateLimitDisabled(t *testing.T) {
	c := New(WithRateLimit(0))
	if c.limiter == nil || c.limiter.interval != 0 {
		t.Fatal("<=0 应构造直通限速器")
	}
	for i := 0; i < 50; i++ {
		if err := c.limiter.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// newErrMock 返回按序返回指定 status/body 的 mock 客户端。
func newErrMock(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewWithURL(srv.URL, WithRateLimit(0)) // 测试关闭限速
}

// HTTP 429 → ErrRateLimited（execution 层据此退避重试）。
func TestHTTP429MarksRateLimited(t *testing.T) {
	c := newErrMock(t, http.StatusTooManyRequests, `{"code":-1003,"msg":"Too many requests"}`)
	_, err := c.GetTicker(context.Background(), "BTCUSDT")
	if !errors.Is(err, exchange.ErrRateLimited) {
		t.Fatalf("429 必须打上限流标记: %v", err)
	}
}

// 限流业务码 -1003 / -1015 / -1021 → ErrRateLimited。
func TestRateLimitBizCodesMarked(t *testing.T) {
	for _, code := range []int{-1003, -1015, -1021} {
		c := newErrMock(t, http.StatusBadRequest, fmt.Sprintf(`{"code":%d,"msg":"rate limited"}`, code))
		_, err := c.GetTicker(context.Background(), "BTCUSDT")
		if !errors.Is(err, exchange.ErrRateLimited) {
			t.Fatalf("业务码 %d 必须打上限流标记: %v", code, err)
		}
	}
}

// HTTP 418（IP 封禁）→ 普通错误（重试无效），不得打限流可重试标记。
func TestHTTP418NotRateLimited(t *testing.T) {
	c := newErrMock(t, http.StatusTeapot, `{"code":418,"msg":"banned"}`)
	_, err := c.GetTicker(context.Background(), "BTCUSDT")
	if err == nil {
		t.Fatal("418 必须报错")
	}
	if errors.Is(err, exchange.ErrRateLimited) {
		t.Fatal("418 是封禁不是普通限流，不得标记为可重试限流")
	}
}

// 普通业务错误（如 -1013 价格过滤）不打限流标记。
func TestNormalBizErrorNotRateLimited(t *testing.T) {
	c := newErrMock(t, http.StatusBadRequest, `{"code":-1013,"msg":"Price filter"}`)
	_, err := c.GetTicker(context.Background(), "BTCUSDT")
	if err == nil {
		t.Fatal("业务错误必须报错")
	}
	if errors.Is(err, exchange.ErrRateLimited) {
		t.Fatal("普通业务错误不得打限流标记")
	}
}
