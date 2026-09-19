// limiter.go Binance REST 客户端限速器与限流信号识别：
// 以最小间隔令牌桶限制请求频率，防止触发交易所限流（HTTP 429 / 业务码 -1003 等）。
// 仅约束 REST（do 入口 acquire）；WS 不受限。
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

// defaultRatePerSec 默认 REST 限速（保守值：10 请求/秒）。
const defaultRatePerSec = 10

// binanceRateLimitCodes Binance 限流类业务码（官方错误码表）。
var binanceRateLimitCodes = map[int]bool{
	-1003: true, // 请求超限（TOO_MANY_REQUESTS）
	-1015: true, // 下单频率超限（TOO_MANY_ORDERS）
	-1021: true, // timestamp 超出 recvWindow（时间戳相关，可重试）
}

// Option 客户端可选配置（构造参数）。
type Option func(*Client)

// WithRateLimit 覆盖默认 REST 限速（次/秒；<=0 表示关闭限速）。
func WithRateLimit(reqPerSec float64) Option {
	return func(c *Client) { c.limiter = newRateLimiter(reqPerSec) }
}

// rateLimiter 简单令牌桶（最小间隔实现）：相邻两次请求至少间隔 interval，
// 超出速率的调用排队等待；空闲时基准自动贴合当前时间（不累积、无突发额度）。
// 直接构造的 Client（limiter 为 nil）不限速，便于测试注入 mock。
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration // 相邻请求最小间隔；<=0 表示不限速
	next     time.Time     // 下一个可发送时刻
}

// newRateLimiter 按每秒请求数构造限速器；reqPerSec <= 0 返回不限速的直通限速器。
func newRateLimiter(reqPerSec float64) *rateLimiter {
	if reqPerSec <= 0 {
		return &rateLimiter{}
	}
	return &rateLimiter{interval: time.Duration(float64(time.Second) / reqPerSec)}
}

// acquire 获取一个令牌（发起一次 REST 请求的资格）。令牌耗尽时排队等待，
// 等待期间 ctx 取消立即返回错误（绝不死等）。
func (l *rateLimiter) acquire(ctx context.Context) error {
	if l == nil || l.interval <= 0 {
		return nil // 未启用限速：直通
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("binance: 获取限速令牌时 ctx 已取消: %w", err)
		}
		l.mu.Lock()
		now := time.Now()
		if !now.Before(l.next) {
			l.next = now.Add(l.interval) // 占住当前槽位并预约下一个
			l.mu.Unlock()
			return nil
		}
		wait := l.next.Sub(now)
		l.mu.Unlock()
		if err := sleepCtx(ctx, wait); err != nil {
			return fmt.Errorf("binance: 等待限速令牌时 ctx 取消: %w", err)
		}
		// 醒来后重查：槽位可能已被并发请求占走，重算等待
	}
}

// sleepCtx 可被 ctx 取消的等待：d 到点返回 nil；ctx 先结束返回 ctx.Err()。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// httpStatusError 将非 200 响应转为错误：
//   - 429 或限流业务码（-1003/-1015/-1021）→ 包装 exchange.ErrRateLimited，execution 层据此重试；
//   - 418（IP 因持续超限被封禁）→ 普通错误并注明重试无效（重试只会延长封禁，不算普通限流）。
func httpStatusError(path string, status int, data []byte) error {
	var apiErr struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(data, &apiErr) // 无 JSON 错误体时按纯 HTTP 错误处理
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("binance: %s HTTP %d 触发限流: %.200s: %w", path, status, data, exchange.ErrRateLimited)
	case status == http.StatusTeapot: // 418：IP 封禁，持续超限的升级处罚
		return fmt.Errorf("binance: %s HTTP %d IP 已被封禁（持续触发限流所致，重试无效，需降低频率并等待解封）: %.200s", path, status, data)
	case binanceRateLimitCodes[apiErr.Code]:
		return fmt.Errorf("binance: %s HTTP %d 限流业务错误 code=%d msg=%s: %w", path, status, apiErr.Code, apiErr.Msg, exchange.ErrRateLimited)
	}
	return fmt.Errorf("binance: %s HTTP %d: %.200s", path, status, data)
}
