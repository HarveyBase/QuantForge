// clock_test.go OKX 时钟偏移校准测试：
// ④ httptest 假 /public/time 返回固定偏移的服务器时间，断言 offset 与 now() 修正；
// 并覆盖自动校准（首次签名请求前触发、失败忽略不阻断请求）。
package okx

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// newClockMock 返回 mock 客户端：/api/v5/public/time 返回 now+deltaMS（模拟服务器时钟领先 deltaMS）。
func newClockMock(t *testing.T, deltaMS int64) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/time", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"0","msg":"","data":[{"ts":"%d"}]}`, time.Now().UnixMilli()+deltaMS)
	})
	return newMockClient(t, mux)
}

// ④ CalibrateClock：offset ≈ delta，now() 被修正 delta 毫秒。
func TestCalibrateClockAppliesOffset(t *testing.T) {
	const delta = int64(2500) // 模拟本地时钟落后服务器 2500ms（>5s 签名窗口的一半，必须校准）
	c := newClockMock(t, delta)
	if c.ClockOffsetMS() != 0 {
		t.Fatal("未校准前 offset 应为 0（与现状一致）")
	}
	if err := c.CalibrateClock(context.Background()); err != nil {
		t.Fatal(err)
	}
	off := c.ClockOffsetMS()
	if off < delta-500 || off > delta+500 {
		t.Fatalf("offset 应≈%dms（允许网络抖动），实际 %dms", delta, off)
	}
	local := time.Now().UTC().UnixMilli()
	if got := c.now().UnixMilli() - local; got < delta-500 || got > delta+500 {
		t.Fatalf("now() 应被修正约 %dms，偏差 %dms", delta, got)
	}
}

// 校准失败（端点 500）：返回错误且不改变现有 offset。
func TestCalibrateClockFailureKeepsOffset(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/time", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := newMockClient(t, mux)
	if err := c.CalibrateClock(context.Background()); err == nil {
		t.Fatal("校准失败必须返回错误（可识别、可留痕）")
	}
	if c.ClockOffsetMS() != 0 {
		t.Fatalf("校准失败不应改变 offset: %d", c.ClockOffsetMS())
	}
}

// 自动校准（首次签名请求前触发）：mock 无 /public/time 端点 → 校准失败被忽略，请求本身不受阻断。
func TestLazyCalibrationFailureIgnored(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"ordId":"1","sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err != nil {
		t.Fatalf("时钟校准失败不应阻断签名请求: %v", err)
	}
	if c.ClockOffsetMS() != 0 {
		t.Fatalf("校准失败 offset 应保持 0: %d", c.ClockOffsetMS())
	}
}

// gotTS 捕获最近一次签名请求的 OK-ACCESS-TIMESTAMP（跨 handler 传递给断言）。
var gotTS string

// 自动校准成功路径：首次签名请求前完成校准，签名时间戳使用校准后的时钟。
func TestLazyCalibrationOnFirstSignedRequest(t *testing.T) {
	const delta = int64(1234)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/time", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"0","msg":"","data":[{"ts":"%d"}]}`, time.Now().UnixMilli()+delta)
	})
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		gotTS = r.Header.Get("OK-ACCESS-TIMESTAMP")
		w.Write([]byte(`{"code":"0","data":[{"ordId":"1","sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err != nil {
		t.Fatal(err)
	}
	if !c.calibrated.Load() {
		t.Fatal("首次签名请求应自动完成时钟校准")
	}
	if off := c.ClockOffsetMS(); off < delta-500 || off > delta+500 {
		t.Fatalf("自动校准 offset 应≈%d，实际 %d", delta, off)
	}
	// 签名时间戳 = c.now() ≈ 本地时间 + delta
	sigTS, err := time.Parse("2006-01-02T15:04:05.000Z", gotTS)
	if err != nil {
		t.Fatalf("签名时间戳格式错误: %v", err)
	}
	if d := sigTS.UnixMilli() - time.Now().UnixMilli(); d < delta-500 || d > delta+500 {
		t.Fatalf("签名时间戳应使用校准后时钟（+%dms），偏差 %dms", delta, d)
	}
}

// RISK-3：lazy 校准失败不消费——下次签名请求重新尝试自动校准并成功
// （保持"do() 内 lazy 校准最多一次并发"语义：CAS 抢占，完成后置回）。
func TestLazyCalibrationRetriesAfterFailure(t *testing.T) {
	const delta = int64(1500)
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/time", func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // 首次校准失败
			return
		}
		fmt.Fprintf(w, `{"code":"0","msg":"","data":[{"ts":"%d"}]}`, time.Now().UnixMilli()+delta)
	})
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"0","data":[{"ordId":"1","sCode":"0"}]}`))
	})
	c := newMockClient(t, mux)
	c.APIKey, c.Secret, c.Passphrase = "k", "s", "p"

	// 第一次签名请求：自动校准失败（忽略、不阻断），calibrating 必须已释放
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err != nil {
		t.Fatalf("校准失败不应阻断签名请求: %v", err)
	}
	if c.calibrated.Load() {
		t.Fatal("首次校准失败后不应标记已校准")
	}
	if c.calibrating.Load() {
		t.Fatal("校准失败后必须释放 calibrating 标记（下次才能重试）")
	}

	// 第二次签名请求：重新尝试校准并成功
	if _, err := c.PlaceOrder(context.Background(), mustReq()); err != nil {
		t.Fatal(err)
	}
	if !c.calibrated.Load() {
		t.Fatal("校准失败后第二次签名请求应重新尝试并完成校准")
	}
	if off := c.ClockOffsetMS(); off < delta-500 || off > delta+500 {
		t.Fatalf("重试校准 offset 应≈%dms, got %dms", delta, off)
	}
	if hits.Load() != 1+clockCalibrateSamples {
		t.Fatalf("应重试校准（1 次失败 + %d 次采样）, got %d 次请求", clockCalibrateSamples, hits.Load())
	}
}
