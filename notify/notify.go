// Package notify 告警通道：Kill Switch / 行情断流 / 连续拒单 / 复盘严重项推送到 Telegram。
// 纪律：密钥走环境变量（TG_BOT_TOKEN / TG_CHAT_ID），不入配置文件；
// 发送失败只留痕不阻塞交易主流程（告警是感知层，不能成为新故障源）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/config"
	"github.com/HarveyBase/QuantForge/review"
)

// Notifier 告警发送器。
type Notifier interface {
	Send(text string)
	Enabled() bool
}

// telegram Telegram Bot 实现（sendMessage API，串行发送 + 防刷屏 + 失败退避）。
type telegram struct {
	mu      sync.Mutex
	token   string
	chatID  string
	apiBase string
	http    *http.Client
	pending chan string
}

// NewFromEnv 从环境变量构造（TG_BOT_TOKEN/TG_CHAT_ID 任一为空则返回 log 兜底实现）。
func NewFromEnv(mode config.Mode, symbol string) Notifier {
	token := os.Getenv("TG_BOT_TOKEN")
	chatID := os.Getenv("TG_CHAT_ID")
	if token == "" || chatID == "" {
		return &logSink{}
	}
	t := newTelegram("https://api.telegram.org", token, chatID)
	go t.loop(mode, symbol)
	return t
}

// newTelegram 构造（apiBase 可注入，测试用）。
func newTelegram(apiBase, token, chatID string) *telegram {
	return &telegram{
		token: token, chatID: chatID, apiBase: apiBase,
		http:    &http.Client{Timeout: 10 * time.Second},
		pending: make(chan string, 100),
	}
}

// loop 串行消费告警。
func (t *telegram) loop(mode config.Mode, symbol string) {
	for text := range t.pending {
		if err := t.post(mode, symbol, text); err != nil {
			log.Printf("notify: Telegram 发送失败（已留日志）: %v | %s", err, text)
			time.Sleep(3 * time.Second)
		} else {
			time.Sleep(time.Second) // 防刷屏
		}
	}
}

func (t *telegram) post(mode config.Mode, symbol, text string) error {
	payload := map[string]string{
		"chat_id": t.chatID,
		"text":    fmt.Sprintf("[QuantForge %s %s]\n%s", mode, symbol, text),
	}
	b, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.apiBase+"/bot"+t.token+"/sendMessage", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Send 入队一条告警（非阻塞；队列满丢弃留痕）。
func (t *telegram) Send(text string) {
	select {
	case t.pending <- text:
	default:
		log.Printf("notify: 告警队列满，丢弃: %s", text)
	}
}

// Enabled 是否启用 Telegram。
func (t *telegram) Enabled() bool { return true }

// logSink 无环境变量时的兜底：告警只进日志。
type logSink struct{}

func (l *logSink) Send(text string) { log.Printf("notify(仅日志): %s", text) }
func (l *logSink) Enabled() bool    { return false }

// Critical 复盘记录中的严重项筛选——告警只发值得叫醒人的：
// Kill 触发、行情断流、交易过频、连续拒单（≥5 笔）。
func Critical(rec review.Record) []string {
	var out []string
	if rec.KillTripped {
		out = append(out, fmt.Sprintf("🔴 Kill Switch 触发：%s（禁止一切新下单，需人工复位）", rec.KillReason))
	}
	for _, n := range rec.Notes {
		if strings.Contains(n, "数据连续性异常") || strings.Contains(n, "交易过频") {
			out = append(out, "🟠 "+n)
		}
	}
	if n := len(rec.Rejections); n >= 5 {
		out = append(out, fmt.Sprintf("🟠 窗口内风控拒单 %d 笔（连续被拦需排查资金/限额配置）", n))
	}
	return out
}
