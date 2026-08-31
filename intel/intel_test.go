package intel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSources(t *testing.T, dir string, feedURL string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "intel"), 0o755)
	os.WriteFile(filepath.Join(dir, "intel", "sources.json"),
		[]byte(`{"sources":[{"name":"test","type":"rss","url":"`+feedURL+`","tags":["测试"],"enabled":true},
			{"name":"off","type":"rss","url":"http://invalid","enabled":false}]}`), 0o644)
}

func TestFetchOnceDedupAndPersist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss><channel>
			<item><title>BTC 突破关键阻力</title><link>https://e.test/a</link><pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate></item>
			<item><title>市场震荡</title><link>https://e.test/b</link><pubDate>Mon, 02 Jan 2006 15:05:05 GMT</pubDate></item>
		</channel></rss>`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeSources(t, dir, srv.URL)
	c := New(dir)

	items, err := c.FetchOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("首轮应 2 条: %d", len(items))
	}
	if items[0].Title != "市场震荡" || items[0].Source != "test" || len(items[0].Tags) != 1 {
		t.Fatalf("条目字段错误: %+v", items[0])
	}
	// 二轮去重
	items2, _ := c.FetchOnce(context.Background())
	if len(items2) != 0 {
		t.Fatalf("二轮应全去重: %d", len(items2))
	}
	// 落盘可读回
	latest := c.Latest(10)
	if len(latest) != 2 {
		t.Fatalf("Latest 应读回 2 条: %d", len(latest))
	}
	// JSONL 行格式
	b, _ := os.ReadFile(filepath.Join(dir, "intel", "items.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL 应 2 行: %d", len(lines))
	}
	var it Item
	if err := json.Unmarshal([]byte(lines[0]), &it); err != nil {
		t.Fatalf("行必须是 JSON: %v", err)
	}
}

func TestAtomFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<feed><entry><title>Atom 条目</title>
			<link rel="alternate" href="https://a.test/x"/><updated>2006-01-02T15:04:05Z</updated></entry></feed>`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeSources(t, dir, srv.URL)
	items, err := New(dir).FetchOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Link != "https://a.test/x" || items[0].Title != "Atom 条目" {
		t.Fatalf("Atom 解析错误: %+v", items)
	}
}

func TestSingleSourceFailureSkipped(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "intel"), 0o755)
	// 一个死源 + 一个活源
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<rss><channel><item><title>ok</title><link>https://x.test/1</link></item></channel></rss>`))
	}))
	defer srv.Close()
	os.WriteFile(filepath.Join(dir, "intel", "sources.json"),
		[]byte(`{"sources":[{"name":"dead","type":"rss","url":"http://127.0.0.1:1/x","enabled":true},
			{"name":"alive","type":"rss","url":"`+srv.URL+`","enabled":true}]}`), 0o644)
	items, err := New(dir).FetchOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Source != "alive" {
		t.Fatalf("单源失败应跳过不中断: %+v", items)
	}
}

func TestLoopStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<rss><channel></channel></rss>`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeSources(t, dir, srv.URL)
	c := New(dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Loop(ctx, 10*time.Millisecond, nil, nil); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消必须终止采集循环")
	}
}
