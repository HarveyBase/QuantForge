package candlestore

import (
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func c(ot int64, closePx float64) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: closePx, High: closePx + 1, Low: closePx - 1, Close: closePx, Volume: 10, Confirmed: true}
}

func TestUpsertAndLatest(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Upsert([]exchange.Candle{c(3, 30), c(1, 10), c(2, 20)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Latest("okx", "BTC-USDT", "1H", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应 3 根: %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].OpenTime <= got[i-1].OpenTime {
			t.Fatal("必须升序")
		}
	}
	// limit 生效
	if g, _ := s.Latest("okx", "BTC-USDT", "1H", 2); len(g) != 2 || g[0].OpenTime != 2 {
		t.Fatalf("Latest(2) 应取最新两根升序: %+v", g)
	}
	// 同键 upsert 修正（未收盘 → 已收盘 + 价格修正）
	fixed := c(2, 22)
	if err := s.Upsert([]exchange.Candle{fixed}); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Latest("okx", "BTC-USDT", "1H", 10)
	if len(got2) != 3 || got2[1].Close != 22 {
		t.Fatalf("upsert 未生效: %+v", got2)
	}
	// 不同 interval / symbol 隔离
	s.Upsert([]exchange.Candle{c(1, 10)})
	other := c(1, 10)
	other.Interval = "4H"
	s.Upsert([]exchange.Candle{other})
	if n, _ := s.Count("okx", "BTC-USDT", "4H"); n != 1 {
		t.Fatal("interval 隔离失败")
	}
	if g, _ := s.Latest("okx", "BTC-USDT", "1H", 100); len(g) != 3 {
		t.Fatal("interval 隔离失败：1H 应仍 3 根")
	}
	// 空写入
	if err := s.Upsert(nil); err != nil {
		t.Fatal("空写入应无操作")
	}
	if g, _ := s.Latest("okx", "ETH-USDT", "1H", 10); len(g) != 0 {
		t.Fatal("无数据应返回空")
	}
}

func TestLargeBatch(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	var batch []exchange.Candle
	for i := int64(1); i <= 5000; i++ {
		batch = append(batch, c(i, float64(i)))
	}
	if err := s.Upsert(batch); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count("okx", "BTC-USDT", "1H"); n != 5000 {
		t.Fatalf("5000 根入库: %d", n)
	}
	got, _ := s.Latest("okx", "BTC-USDT", "1H", 2000)
	if len(got) != 2000 || got[0].OpenTime != 3001 || got[1999].OpenTime != 5000 {
		t.Fatalf("大批量读取错误: len=%d 首=%d 末=%d", len(got), got[0].OpenTime, got[1999].OpenTime)
	}
}
