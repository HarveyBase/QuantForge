// Package intel 情报采集（Charles 角色的数据面，docs/09）：
// 轮询 data/intel/sources.json 声明的公开 RSS 源，新条目去重后落盘 JSONL。
// 纪律：所有源必须无需登录凭据（sources.json 声明）；采集失败留痕不中断。
package intel

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source 情报源声明（data/intel/sources.json）。
type Source struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // rss
	URL     string   `json:"url"`
	Tags    []string `json:"tags"`
	Enabled bool     `json:"enabled"`
}

// Item 采集到的一条情报。
type Item struct {
	Source    string    `json:"source"`
	Tags      []string  `json:"tags,omitempty"`
	Title     string    `json:"title"`
	Link      string    `json:"link"`
	Published time.Time `json:"published"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Collector 情报采集器（去重状态持久化于 items.jsonl，重启续用）。
type Collector struct {
	dir     string
	http    *http.Client
	mu      sync.Mutex
	seen    map[string]bool
	fetched int
}

// New 构造（dataDir 下读 sources.json、写 intel/items.jsonl）。
func New(dataDir string) *Collector {
	return &Collector{
		dir:  filepath.Join(dataDir, "intel"),
		http: &http.Client{Timeout: 20 * time.Second},
		seen: map[string]bool{},
	}
}

// LoadSources 读取情报源声明。
func (c *Collector) LoadSources() ([]Source, error) {
	b, err := os.ReadFile(filepath.Join(c.dir, "sources.json"))
	if err != nil {
		return nil, fmt.Errorf("intel: 读取情报源失败: %w", err)
	}
	var srcs struct {
		Sources []Source `json:"sources"`
	}
	if err := json.Unmarshal(b, &srcs); err != nil {
		return nil, fmt.Errorf("intel: 解析情报源失败: %w", err)
	}
	return srcs.Sources, nil
}

// loadSeen 启动时重建去重集合（读 items.jsonl 尾部）。
func (c *Collector) loadSeen() {
	f, err := os.Open(filepath.Join(c.dir, "items.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var it Item
		if err := dec.Decode(&it); err != nil {
			break
		}
		c.seen[it.Link] = true
	}
}

// FetchOnce 拉一轮全部启用源，返回新条目（已去重并落盘）。
func (c *Collector) FetchOnce(ctx context.Context) ([]Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen != nil && len(c.seen) == 0 && c.fetched == 0 {
		c.loadSeen()
	}
	c.fetched++
	srcs, err := c.LoadSources()
	if err != nil {
		return nil, err
	}
	var fresh []Item
	for _, src := range srcs {
		if !src.Enabled || src.Type != "rss" {
			continue
		}
		items, err := c.fetchRSS(ctx, src)
		if err != nil {
			continue // 单源失败留痕跳过（不中断整轮）
		}
		for _, it := range items {
			if it.Link == "" || c.seen[it.Link] {
				continue
			}
			c.seen[it.Link] = true
			fresh = append(fresh, it)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Published.After(fresh[j].Published) })
	if len(fresh) > 0 {
		if err := c.append(fresh); err != nil {
			return fresh, fmt.Errorf("intel: 落盘失败: %w", err)
		}
	}
	return fresh, nil
}

type rssDoc struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
	Entries []struct { // Atom 兼容
		Title string `xml:"title"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
	} `xml:"entry"`
}

func (c *Collector) fetchRSS(ctx context.Context, src Source) ([]Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "QuantForge-Intel/0.1")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var doc rssDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 RSS 失败: %w", err)
	}
	now := time.Now().UTC()
	var out []Item
	for _, it := range doc.Channel.Items {
		pub, _ := time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate))
		if pub.IsZero() {
			pub, _ = time.Parse(time.RFC1123, strings.TrimSpace(it.PubDate))
		}
		out = append(out, Item{Source: src.Name, Tags: src.Tags, Title: strings.TrimSpace(it.Title), Link: it.Link, Published: pub, FetchedAt: now})
	}
	for _, e := range doc.Entries { // Atom
		link := ""
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		pub, _ := time.Parse(time.RFC3339, e.Published)
		if pub.IsZero() {
			pub, _ = time.Parse(time.RFC3339, e.Updated)
		}
		out = append(out, Item{Source: src.Name, Tags: src.Tags, Title: strings.TrimSpace(e.Title), Link: link, Published: pub, FetchedAt: now})
	}
	return out, nil
}

func (c *Collector) append(items []Item) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.dir, "items.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		f.Write(append(b, '\n'))
	}
	return nil
}

// Latest 最近 n 条（时间倒序；读 items.jsonl）。
func (c *Collector) Latest(n int) []Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(c.dir, "items.jsonl"))
	if err != nil {
		return nil
	}
	var items []Item
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var it Item
		if json.Unmarshal([]byte(line), &it) == nil {
			items = append(items, it)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Published.After(items[j].Published) })
	if n > 0 && len(items) > n {
		items = items[:n]
	}
	return items
}

// Loop 周期采集直到 ctx 取消（默认 15 分钟一轮）。
func (c *Collector) Loop(ctx context.Context, every time.Duration, onItems func([]Item), onErr func(error)) {
	if every <= 0 {
		every = 15 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			items, err := c.FetchOnce(ctx)
			if err != nil && onErr != nil {
				onErr(err)
			}
			if len(items) > 0 && onItems != nil {
				onItems(items)
			}
		}
	}
}
