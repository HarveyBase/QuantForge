// 复盘→风控日报（Kriss 角色的数据面，docs/09）：
// 聚合最近 N 份小时复盘为一份结构化日报文本，供 Telegram 推送与人工审阅。
package review

import (
	"fmt"
	"strings"
	"time"
)

// DailyDigest 聚合复盘记录为日报文本（输入须按时间倒序，即 Recent 的顺序）。
// 口径：阶段标注、盈亏如实、异常项前置（风控日报先看风险再看收益）。
func DailyDigest(recs []Record) string {
	if len(recs) == 0 {
		return "复盘日报：无记录（复盘器未运行或刚启动）"
	}
	oldest, newest := recs[len(recs)-1], recs[0]
	span := newest.Ts.Sub(oldest.WindowFrom)
	fills, rejections, umpBlocked := 0, 0, 0
	for _, r := range recs {
		fills += len(r.Fills)
		rejections += len(r.Rejections)
		umpBlocked += r.UMPBlocked
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📋 复盘日报（%s ~ %s，约 %.0f 小时，%d 份）\n",
		oldest.WindowFrom.Local().Format("01-02 15:04"), newest.Ts.Local().Format("01-02 15:04"),
		span.Hours(), len(recs))
	fmt.Fprintf(&b, "阶段: %s | 品种: %s %s\n", newest.Stage, newest.Symbol, newest.Interval)
	// 风险项前置
	var risks []string
	if newest.KillTripped {
		risks = append(risks, fmt.Sprintf("🔴 Kill Switch 触发中：%s", newest.KillReason))
	}
	byRule := map[string]int{}
	for _, r := range recs {
		for _, rej := range r.Rejections {
			byRule[rej.RuleID]++
		}
	}
	if rejections > 0 {
		var parts []string
		for k, v := range byRule {
			parts = append(parts, fmt.Sprintf("%s×%d", k, v))
		}
		risks = append(risks, fmt.Sprintf("🟠 风控拒单 %d 笔（%s）", rejections, strings.Join(parts, ",")))
	}
	if umpBlocked > 0 {
		risks = append(risks, fmt.Sprintf("UMP 拦截 %d 个买入信号（历史低胜率情境）", umpBlocked))
	}
	for _, r := range recs {
		for _, n := range r.Notes {
			if strings.Contains(n, "数据连续性异常") || strings.Contains(n, "交易过频") {
				risks = append(risks, "🟠 "+n)
				break
			}
		}
	}
	if len(risks) == 0 {
		b.WriteString("风险项：无（无 Kill/拒单/断流/过频）\n")
	} else {
		b.WriteString("风险项：\n")
		for _, r := range risks {
			b.WriteString("  " + r + "\n")
		}
	}
	// 收益与操作
	fmt.Fprintf(&b, "最新权益: %.2f（窗口收益 %.2f%% vs 买入持有 %.2f%%）\n",
		newest.Equity, newest.WindowRetPct, newest.PriceChgPct)
	fmt.Fprintf(&b, "成交 %d 笔 | 挂单 %d | 策略: %s\n", fills, newest.OpenOrders, newest.Strategy)
	// 最近一份诊断
	if len(newest.Notes) > 0 {
		b.WriteString("最近诊断: " + newest.Notes[0] + "\n")
	}
	b.WriteString("（复盘口径见 docs/01；本日报不构成投资建议）")
	return b.String()
}

// DigestSince 取指定时长内的复盘聚合（records 为倒序输入）。
func DigestSince(recs []Record, window time.Duration) string {
	cutoff := time.Now().UTC().Add(-window)
	var kept []Record
	for _, r := range recs { // recs 倒序：新→旧
		if r.Ts.Before(cutoff) {
			break
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		kept = recs[:min(1, len(recs))]
	}
	return DailyDigest(kept)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
