package market

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

// SnapshotStore 快照存储（docs/02）：双写 = 历史归档 + 当前指针，manifest 登记完整性。
// 追溯链：报告结论 → 数据集名称 → 当前指针 → history_id 归档 → manifest。
type SnapshotStore struct {
	root string // data/snapshots
	mu   sync.Mutex
}

func NewSnapshotStore(dataDir string) *SnapshotStore {
	return &SnapshotStore{root: filepath.Join(dataDir, "snapshots")}
}

// Save 写入一份 K 线快照：data/snapshots/<name>/<ts>.json + latest.json 指针 + manifest.json。
func (s *SnapshotStore) Save(name string, candles []exchange.Candle) (historyID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("snapshot: 建目录失败: %w", err)
	}
	historyID = time.Now().UTC().Format("20060102T150405.000000000")
	archive := filepath.Join(dir, historyID+".json")
	payload := struct {
		HistoryID string            `json:"history_id"`
		SavedAt   time.Time         `json:"saved_at"`
		Count     int               `json:"count"`
		Candles   []exchange.Candle `json:"candles"`
	}{historyID, time.Now().UTC(), len(candles), candles}
	b, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(archive, b, 0o644); err != nil {
		return "", fmt.Errorf("snapshot: 写归档失败: %w", err)
	}
	// 当前指针
	pointer := filepath.Join(dir, "latest.json")
	if err := os.WriteFile(pointer, b, 0o644); err != nil {
		return "", fmt.Errorf("snapshot: 写指针失败: %w", err)
	}
	// manifest 完整性登记
	if err := s.updateManifest(dir, historyID, len(candles)); err != nil {
		return "", err
	}
	return historyID, nil
}

// LoadLatest 从当前指针读取最近快照。
func (s *SnapshotStore) LoadLatest(name string) ([]exchange.Candle, error) {
	pointer := filepath.Join(s.root, name, "latest.json")
	b, err := os.ReadFile(pointer)
	if err != nil {
		return nil, fmt.Errorf("snapshot: 读取指针失败（无快照）: %w", err)
	}
	var payload struct {
		Candles []exchange.Candle `json:"candles"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("snapshot: 解析失败: %w", err)
	}
	return payload.Candles, nil
}

func (s *SnapshotStore) updateManifest(dir, historyID string, count int) error {
	path := filepath.Join(dir, "manifest.json")
	var m struct {
		Entries []struct {
			HistoryID string    `json:"history_id"`
			SavedAt   time.Time `json:"saved_at"`
			Count     int       `json:"count"`
			Complete  bool      `json:"complete"`
		} `json:"entries"`
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m.Entries = append(m.Entries, struct {
		HistoryID string    `json:"history_id"`
		SavedAt   time.Time `json:"saved_at"`
		Count     int       `json:"count"`
		Complete  bool      `json:"complete"`
	}{historyID, time.Now().UTC(), count, true})
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Poller 周期拉取并校验 K 线，产出"已确认序列"供策略与回测使用。
type Poller struct {
	Ex       exchange.Exchange
	Symbol   string
	Interval string
	OnUpdate func([]exchange.Candle) // 每次成功拉取校验后的回调
	OnError  func(error)             // 拉取或校验失败回调
	mu       sync.Mutex
}

// FetchOnce 拉一次 → 校验 → 回调。
func (p *Poller) FetchOnce(ctx context.Context, limit int) ([]exchange.Candle, error) {
	candles, err := p.Ex.GetCandles(ctx, p.Symbol, p.Interval, limit)
	if err != nil {
		return nil, err
	}
	ms := IntervalMs(p.Interval)
	if ms == 0 {
		return nil, fmt.Errorf("market: 未知周期 %q", p.Interval)
	}
	clean, err := Validate(candles, ms)
	if err != nil {
		return nil, err
	}
	if p.OnUpdate != nil {
		p.OnUpdate(clean)
	}
	return clean, nil
}

// Run 周期轮询直到 ctx 取消。错误不 panic、不静默：记入 Errs 供上层展示。
func (p *Poller) Run(ctx context.Context, limit int, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.FetchOnce(ctx, limit); err != nil {
				if p.OnError != nil {
					p.OnError(err)
				}
				continue
			}
		}
	}
}
