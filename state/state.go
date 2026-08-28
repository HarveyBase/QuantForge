// Package state 提供运行态的原子 JSON 持久化。
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const Version = 1

// UmpCell UMP 拦截器单情境统计（JSON 兼容：数组键转结构体）。
type UmpCell struct {
	Key   [3]int `json:"key"` // RSI 桶/距前高桶/波动分位桶
	Wins  int    `json:"wins"`
	Total int    `json:"total"`
}

type Runtime struct {
	Version     int       `json:"version"`
	LastCandle  int64     `json:"last_candle"`
	Trials      int       `json:"trials"`
	KillTripped bool      `json:"kill_tripped"`
	KillReason  string    `json:"kill_reason"`
	UMP         []UmpCell `json:"ump,omitempty"` // UMP 拦截器统计快照（重启续用）
}

type Store struct {
	path string
	mu   sync.Mutex
}

func New(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "state", "runtime.json")}
}

func (s *Store) Load() (Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Runtime{Version: Version}, nil
	}
	if err != nil {
		return Runtime{}, fmt.Errorf("state: 读取失败: %w", err)
	}
	var v Runtime
	if err := json.Unmarshal(b, &v); err != nil {
		return Runtime{}, fmt.Errorf("state: 解析失败: %w", err)
	}
	if v.Version != Version {
		return Runtime{}, fmt.Errorf("state: 不支持的版本 %d", v.Version)
	}
	return v, nil
}

func (s *Store) Save(v Runtime) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.Version = Version
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("state: 建目录失败: %w", err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("state: 序列化失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("state: 写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: 原子替换失败: %w", err)
	}
	return nil
}
