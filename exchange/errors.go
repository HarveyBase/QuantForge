package exchange

import "errors"

// ErrRateLimited 适配器在收到交易所限流信号（HTTP 429 或限流业务码）时返回。
// execution 层据此进入可重试分支并使用更长退避，而非立即失败。
var ErrRateLimited = errors.New("exchange: 触发交易所限流")

// IsRateLimitError 判断错误链中是否携带限流信号。
func IsRateLimitError(err error) bool { return errors.Is(err, ErrRateLimited) }
