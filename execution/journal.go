// 事件溯源持久化：订单四张表的变更以 JSONL 追加落盘，进程重启后重放重建，
// 消除"内存表重启即丢 → 孤儿单与账本发散"的 P0 缺口。
package execution

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

// DefaultJournalPath 默认事件日志路径（调用方可传自定义路径覆盖）。
const DefaultJournalPath = "data/state/orders.jsonl"

// compactKeepTail 压缩时保留的最近事件条数。
const compactKeepTail = 2000

// JournalEvent 一行事件日志。Ev 取值：
// register / adopted / update / cancel / remove / freeze_release。
type JournalEvent struct {
	Ts    time.Time      `json:"ts"`
	Ev    string         `json:"ev"`
	Order exchange.Order `json:"order"`
}

// Journal append-only 事件日志。每次写入重新打开文件追加（下单事件频率极低，
// 换取压缩重写 rename 之后不残留悬空句柄）；不强制 fsync——崩溃至多丢最后一两条事件，
// 恢复逻辑可容错（重放语义幂等）。线程安全（mutex）。
type Journal struct {
	mu   sync.Mutex
	path string
}

// NewJournal 挂载事件日志（目录自动创建，文件不存在则创建）。
func NewJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("execution: 创建事件日志目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("execution: 打开事件日志失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("execution: 预检事件日志失败: %w", err)
	}
	return &Journal{path: path}, nil
}

// Append 追加一行事件（单次 write，不 fsync）。
func (j *Journal) Append(ev JournalEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("execution: 事件序列化失败: %w", err)
	}
	b = append(b, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("execution: 打开事件日志失败: %w", err)
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("execution: 写入事件日志失败: %w", werr)
	}
	return cerr
}

// Compact 压缩重写本日志（与 Append 互斥）；语义同包级 Compact。
func (j *Journal) Compact(keepNonTerminal bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Compact(j.path, keepNonTerminal)
}

// LoadJournal 读取全量事件；损坏行/空行跳过并计数返回（重放容错：坏行不阻断恢复）。
func LoadJournal(path string) (events []JournalEvent, skipped int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("execution: 打开事件日志失败: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			skipped++
			continue
		}
		var ev JournalEvent
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			skipped++
			continue
		}
		events = append(events, ev)
	}
	if serr := sc.Err(); serr != nil {
		return events, skipped, fmt.Errorf("execution: 读取事件日志失败: %w", serr)
	}
	return events, skipped, nil
}

// Compact 重写事件日志，只保留：全部非终态订单最新快照 + 全部 claimed 幂等键（去重）
// + 最近 compactKeepTail 条原始事件（先快照后尾部事件，尾部重放幂等收敛到同一终态）。
// keepNonTerminal=false 为维护模式：仅截断保留最近 compactKeepTail 条（可能丢失挂单与
// claimed 信息，正常压缩必须传 true）。原子重写：临时文件 + rename。
func Compact(path string, keepNonTerminal bool) error {
	events, _, err := LoadJournal(path)
	if err != nil {
		return err
	}
	var out []JournalEvent
	if keepNonTerminal {
		out = append(out, snapshotsFor(events)...)
	}
	if len(events) > compactKeepTail {
		events = events[len(events)-compactKeepTail:]
	}
	out = append(out, events...)
	if err := writeAll(path, out); err != nil {
		return err
	}
	return nil
}

// snapshotsFor 重放全量事件，产出压缩快照事件：存活挂单（register 形式，自带 claimed）
// 与"终态但仍 claimed"的幂等键（防重发必须跨压缩保留）。
func snapshotsFor(events []JournalEvent) []JournalEvent {
	replayer := &Executor{orders: map[string]exchange.Order{}, byClient: map[string]string{}, claimed: map[string]bool{}}
	replayer.RestoreFromEvents(events)
	// last 记录每个 OrderID 的最后已知状态（含已从 orders 删除的终态单），供终态 claimed 快照取值。
	last := map[string]exchange.Order{}
	for _, ev := range events {
		switch ev.Ev {
		case "register", "adopted", "update", "cancel":
			if ev.Order.OrderID != "" {
				last[ev.Order.OrderID] = ev.Order
			}
		}
	}
	now := time.Now()
	var out []JournalEvent
	for _, o := range replayer.orders {
		out = append(out, JournalEvent{Ts: now, Ev: "register", Order: o})
	}
	for coid := range replayer.claimed {
		oid := replayer.byClient[coid]
		if _, open := replayer.orders[oid]; open {
			continue // 存活挂单快照已覆盖该幂等键
		}
		o, ok := last[oid]
		if !ok {
			o = exchange.Order{OrderID: oid, ClientOrderID: coid, Status: exchange.StatusCancelled}
		}
		out = append(out, JournalEvent{Ts: now, Ev: "register", Order: o})
	}
	// 排序保证压缩输出可复现（map 迭代无序）。
	sort.Slice(out, func(i, j int) bool { return out[i].Order.OrderID < out[j].Order.OrderID })
	return out
}

// writeAll 原子重写日志：写临时文件后 rename（与 state 包同一套路）。
func writeAll(path string, events []JournalEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("execution: 创建事件日志目录失败: %w", err)
	}
	tmp := path + ".compact.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("execution: 打开压缩临时文件失败: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, ev := range events {
		b, merr := json.Marshal(ev)
		if merr != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("execution: 事件序列化失败: %w", merr)
		}
		if _, werr := w.Write(append(b, '\n')); werr != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("execution: 写入压缩临时文件失败: %w", werr)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("execution: 刷新压缩临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("execution: 关闭压缩临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("execution: 压缩替换失败: %w", err)
	}
	return nil
}

// AttachJournal 挂载事件日志。启动顺序建议：
// New → AttachJournal → LoadJournal → RestoreFromEvents → AdoptOpenOrders。
func (e *Executor) AttachJournal(j *Journal) {
	e.mu.Lock()
	e.journal = j
	e.mu.Unlock()
}

// CompactJournal 触发事件日志压缩（由调用方在空闲时调用，执行器不自动压缩）。
func (e *Executor) CompactJournal() error {
	e.mu.Lock()
	j := e.journal
	e.mu.Unlock()
	if j == nil {
		return fmt.Errorf("execution: 未挂载 journal，无法压缩")
	}
	return j.Compact(true)
}

// journalEvent 把内存表变更落一条事件（在持有 e.mu 的变更点调用，保证与内存变更同序）。
// 写失败仅告警不阻断交易：崩溃窗口丢尾部事件可接受，恢复逻辑可容错。
func (e *Executor) journalEvent(ev string, o exchange.Order) {
	if e.journal == nil {
		return
	}
	if err := e.journal.Append(JournalEvent{Ts: time.Now(), Ev: ev, Order: o}); err != nil {
		log.Printf("execution: journal 写入失败(%s %s): %v", ev, o.OrderID, err)
	}
}

// RestoreFromEvents 重放事件日志，重建 orders/byClient/claimed 三张表（inflight 不恢复：
// 重启后不存在进行中的提交）。重放不回写 journal、不触碰组合账本——资金冻结与持仓
// 由对账流程重建。重放语义（与内存路径逐点一致，保证写→重放→三张表一致）：
//   - register/adopted → claimed+byClient+orders 写入；下单即成交的终态单不入 orders，claimed 保留；
//   - update → 按 OrderID 覆盖，FilledQty 单调不减（回退的乱序回报整条忽略）；终态后从 orders 删除但 claimed 保留；
//   - cancel → 从 orders 删除（cancel 属终态，与内存 Cancel 一致 claimed 保留，幂等防重发跨重启）；
//   - remove → 删除并解除 claimed（register 冻结失败的回滚路径）。
func (e *Executor) RestoreFromEvents(events []JournalEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range events {
		o := ev.Order
		switch ev.Ev {
		case "register", "adopted":
			if o.OrderID == "" {
				continue
			}
			if o.ClientOrderID != "" {
				e.claimed[o.ClientOrderID] = true
				e.byClient[o.ClientOrderID] = o.OrderID
			}
			e.orders[o.OrderID] = o
			if o.Status.Terminal() && o.FilledQty > 0 {
				delete(e.orders, o.OrderID) // 下单即成交：内存路径 register 后 applyDelta 随即删除
			}
		case "update":
			old, ok := e.orders[o.OrderID]
			if !ok {
				continue
			}
			if o.FilledQty < old.FilledQty {
				continue // 乱序回报：FilledQty 回退，整条忽略（单调不减）
			}
			e.orders[o.OrderID] = o
			if o.Status.Terminal() {
				delete(e.orders, o.OrderID) // 终态离场，claimed 保留
			}
		case "cancel":
			delete(e.orders, o.OrderID) // claimed 保留：终态单防重发必须跨重启
		case "remove":
			if prev, ok := e.orders[o.OrderID]; ok {
				delete(e.claimed, prev.ClientOrderID)
			}
			if o.ClientOrderID != "" {
				delete(e.claimed, o.ClientOrderID)
			}
			delete(e.orders, o.OrderID)
		case "freeze_release":
			// 冻结释放只做审计留痕，重放不动组合账本
		default:
			// 未知事件类型：向前兼容跳过
		}
	}
	log.Printf("execution: 事件重放完成：%d 条事件，存活挂单 %d 张，claimed %d 个", len(events), len(e.orders), len(e.claimed))
}
