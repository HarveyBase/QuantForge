package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadMissingReturnsDefault(t *testing.T) {
	s := New(t.TempDir())
	v, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != Version || v.LastCandle != 0 || v.KillTripped {
		t.Fatalf("无状态文件应返回默认值: %+v", v)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	want := Runtime{LastCandle: 1700000000000, Trials: 3, KillTripped: true, KillReason: "演练"}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version {
		t.Fatalf("Save 应强制写入当前版本号: %d", got.Version)
	}
	if got.LastCandle != want.LastCandle || got.Trials != want.Trials ||
		got.KillTripped != want.KillTripped || got.KillReason != want.KillReason {
		t.Fatalf("往返数据不一致: %+v vs %+v", got, want)
	}
	// 归档文件应落在 dataDir/state/runtime.json
	if _, err := os.Stat(filepath.Join(dir, "state", "runtime.json")); err != nil {
		t.Fatalf("状态文件路径错误: %v", err)
	}
}

func TestLoadCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	os.WriteFile(filepath.Join(dir, "state", "runtime.json"), []byte("{bad"), 0o600)
	if _, err := New(dir).Load(); err == nil || !strings.Contains(err.Error(), "解析失败") {
		t.Fatalf("损坏 JSON 必须报错: %v", err)
	}
}

func TestLoadUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	os.WriteFile(filepath.Join(dir, "state", "runtime.json"), []byte(`{"version":99}`), 0o600)
	if _, err := New(dir).Load(); err == nil || !strings.Contains(err.Error(), "不支持的版本") {
		t.Fatalf("版本不匹配必须报错: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Save(Runtime{LastCandle: int64(i), Trials: i})
			_, _ = s.Load()
		}(i)
	}
	wg.Wait()
}

func TestSaveMkdirFailure(t *testing.T) {
	// 把 dataDir 指向一个已存在的文件：MkdirAll 必然失败
	file := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(file, []byte("x"), 0o600)
	s := New(file)
	if err := s.Save(Runtime{}); err == nil || !strings.Contains(err.Error(), "建目录失败") {
		t.Fatalf("建目录失败必须报错: %v", err)
	}
}
