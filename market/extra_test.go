package market

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestValidateSortsOutOfOrder(t *testing.T) {
	in := []exchange.Candle{mk(102, 3, true), mk(60000, 1, true), mk(120000, 2, true)}
	out, err := Validate(in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !sort.SliceIsSorted(out, func(i, j int) bool { return out[i].OpenTime < out[j].OpenTime }) {
		t.Fatalf("输出必须升序: %+v", out)
	}
}

func TestValidateAllUnconfirmed(t *testing.T) {
	in := []exchange.Candle{mk(100, 1, false), mk(101, 2, false)}
	if _, err := Validate(in, 1); err == nil {
		t.Fatal("全部未收盘必须报无可用 K 线")
	}
}

func TestValidateNonPositivePrice(t *testing.T) {
	bad := mk(100, 1, true)
	bad.Close = 0
	if _, err := Validate([]exchange.Candle{bad}, 1); err == nil {
		t.Fatal("非正价格必须报错")
	}
}

func TestValidateBadTimestamp(t *testing.T) {
	bad := mk(0, 1, true)
	if _, err := Validate([]exchange.Candle{bad}, 1); err == nil {
		t.Fatal("零时间戳必须报错")
	}
}

func TestValidateKeepsLongestRun(t *testing.T) {
	// 两段：[100,101]（2 根）与 [103,104,105]（3 根）→ 保留后者
	in := []exchange.Candle{mk(60000, 1, true), mk(120000, 2, true), mk(103, 4, true), mk(104, 5, true), mk(105, 6, true)}
	out, err := Validate(in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].OpenTime != 103 {
		t.Fatalf("应保留最长连续段: %+v", out)
	}
}

func TestValidateSingleCandle(t *testing.T) {
	out, err := Validate([]exchange.Candle{mk(100, 1, true)}, 1)
	if err != nil || len(out) != 1 {
		t.Fatalf("单根 K 线应通过: %v", err)
	}
}

func TestIntervalMsMore(t *testing.T) {
	cases := map[string]int64{
		"2m": 120_000, "30m": 1_800_000, "2H": 7_200_000, "12H": 43_200_000, "3D": 259_200_000,
		"1": 0, "": 0, "m": 0, "1x": 0, "x1": 0, "01m": 60_000,
	}
	for in, want := range cases {
		if got := IntervalMs(in); got != want {
			t.Errorf("IntervalMs(%q)=%d want %d", in, got, want)
		}
	}
}

// stubEx 行情替身。
type stubEx struct {
	candles []exchange.Candle
	err     error
}

func (s *stubEx) Name() string { return "stub" }
func (s *stubEx) GetCandles(ctx context.Context, symbol, interval string, limit int) ([]exchange.Candle, error) {
	return s.candles, s.err
}
func (s *stubEx) GetTicker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	return exchange.Ticker{}, nil
}
func (s *stubEx) GetInstrument(ctx context.Context, instID string) (exchange.Instrument, error) {
	return exchange.Instrument{}, nil
}
func (s *stubEx) GetBalances(ctx context.Context) ([]exchange.Balance, error) { return nil, nil }
func (s *stubEx) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	return exchange.Order{}, nil
}
func (s *stubEx) CancelOrder(ctx context.Context, symbol, orderID string) error { return nil }
func (s *stubEx) GetOrder(ctx context.Context, symbol, orderID string) (exchange.Order, error) {
	return exchange.Order{}, nil
}
func (s *stubEx) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (exchange.Order, error) {
	return exchange.Order{}, nil
}
func (s *stubEx) GetOpenOrders(ctx context.Context, symbol string) ([]exchange.Order, error) {
	return nil, nil
}

func TestPollerFetchOnce(t *testing.T) {
	called := false
	p := &Poller{
		Ex:     &stubEx{candles: []exchange.Candle{mk(60000, 1, true), mk(120000, 2, true)}},
		Symbol: "BTC-USDT", Interval: "1m",
		OnUpdate: func(cs []exchange.Candle) { called = true },
	}
	cs, err := p.FetchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || !called {
		t.Fatalf("FetchOnce 应回调并返回清洗序列: %d %v", len(cs), called)
	}
}

func TestPollerFetchOnceError(t *testing.T) {
	p := &Poller{Ex: &stubEx{err: errors.New("net down")}, Symbol: "BTC-USDT", Interval: "1m"}
	errSeen := false
	p.OnError = func(err error) { errSeen = true }
	// FetchOnce 直接返回错误（Run 内部才回调 OnError）
	if _, err := p.FetchOnce(context.Background(), 10); err == nil {
		t.Fatal("拉取失败必须返回错误")
	}
	_ = errSeen
}

func TestPollerFetchOnceUnknownInterval(t *testing.T) {
	p := &Poller{Ex: &stubEx{candles: []exchange.Candle{mk(100, 1, true)}}, Symbol: "BTC-USDT", Interval: "7x"}
	if _, err := p.FetchOnce(context.Background(), 10); err == nil {
		t.Fatal("未知周期必须报错")
	}
}

func TestPollerRunStopsOnErrorAndCancel(t *testing.T) {
	var errCount atomic.Int32
	p := &Poller{
		Ex: &stubEx{err: errors.New("net down")}, Symbol: "BTC-USDT", Interval: "1m",
		OnError: func(err error) { errCount.Add(1) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx, 10, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	if errCount.Load() == 0 {
		t.Fatal("轮询失败必须回调 OnError（错误不静默）")
	}
}

func TestSnapshotLoadMissing(t *testing.T) {
	s := NewSnapshotStore(t.TempDir())
	if _, err := s.LoadLatest("none"); err == nil {
		t.Fatal("无快照必须报错")
	}
}

func TestSnapshotLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := NewSnapshotStore(dir)
	s.Save("x", []exchange.Candle{mk(100, 1, true)})
	// 破坏指针文件
	os.WriteFile(filepath.Join(dir, "snapshots", "x", "latest.json"), []byte("{bad"), 0o644)
	if _, err := s.LoadLatest("x"); err == nil {
		t.Fatal("损坏指针必须报错")
	}
}
