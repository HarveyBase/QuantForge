// clock.go OKX 时钟偏移校准：签名时间戳统一走 c.now() = 本地 UTC + offset。
// 本地时钟漂移超过签名窗口（约 5s）会导致所有私有接口签名校验失败（静默拒绝），
// 启动后应调用 CalibrateClock 校准一次；构造函数不做网络 IO，
// 首次私有（签名）请求前会自动尝试校准一次（尽力而为，失败忽略）。
package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// clockCalibrateSamples 校准采样次数（取中位数，抗单次网络抖动）。
const clockCalibrateSamples = 3

// CalibrateClock 采样 /api/v5/public/time（公开接口，无需签名）校准时钟偏移。
// offset = 服务器时间 - 本地时间（毫秒），多次采样取中位数；失败返回错误且不改变现有 offset。
func (c *Client) CalibrateClock(ctx context.Context) error {
	offsets := make([]int64, 0, clockCalibrateSamples)
	var lastErr error
	for i := 0; i < clockCalibrateSamples; i++ {
		offset, err := c.sampleServerClock(ctx)
		if err != nil {
			lastErr = err
			break // 网络已不可靠，继续采样无意义
		}
		offsets = append(offsets, offset)
	}
	if len(offsets) == 0 {
		return fmt.Errorf("okx: 时钟校准失败: %w", lastErr)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	c.clockOffset.Store(offsets[len(offsets)/2])
	c.calibrated.Store(true)
	return nil
}

// sampleServerClock 单次采样：把 RTT 均摊到两端，服务器生成时刻 ≈ 请求中点本地时刻。
func (c *Client) sampleServerClock(ctx context.Context) (int64, error) {
	start := time.Now()
	data, err := c.do(ctx, http.MethodGet, "/api/v5/public/time", nil, nil, false)
	if err != nil {
		return 0, err
	}
	rtt := time.Since(start)
	var env struct {
		Data []struct {
			Ts string `json:"ts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Data) == 0 {
		return 0, fmt.Errorf("okx: 服务器时间响应解析失败: %.80s", data)
	}
	serverTs := toInt64(env.Data[0].Ts)
	if serverTs <= 0 {
		return 0, fmt.Errorf("okx: 服务器时间戳非法: %s", env.Data[0].Ts)
	}
	mid := start.Add(rtt / 2) // 请求中点 ≈ 服务器生成时刻
	return serverTs - mid.UnixMilli(), nil
}

// now 签名用当前时刻：本地 UTC + 校准偏移。未校准时 offset=0，与本地时钟行为一致。
func (c *Client) now() time.Time {
	return time.Now().UTC().Add(time.Duration(c.clockOffset.Load()) * time.Millisecond)
}

// ClockOffsetMS 当前与服务器时钟偏移（毫秒）。调用方可据此在启动时打日志/告警
// （建议阈值：|offset| > 3000ms 视为偏移过大）。
func (c *Client) ClockOffsetMS() int64 { return c.clockOffset.Load() }

// ensureClockCalibrated 首次私有（签名）请求前自动校准一次，尽力而为：
// 失败仅放弃本次尝试（offset 保持 0，与未校准现状一致），不阻断请求；
// 手动 CalibrateClock 成功后不再触发。
func (c *Client) ensureClockCalibrated() {
	if c.calibrated.Load() {
		return
	}
	c.calibrateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.CalibrateClock(ctx) // 失败忽略：是否重试由调用方决定
	})
}
