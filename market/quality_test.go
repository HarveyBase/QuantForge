package market

import (
	"encoding/json"
	"os"
	"path"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func mk(ot int64, close float64, confirmed bool) exchange.Candle {
	return exchange.Candle{Exchange: "okx", Symbol: "BTC-USDT", Interval: "1H",
		OpenTime: ot, Open: close, High: close * 1.02, Low: close * 0.98, Close: close, Volume: 1, Confirmed: confirmed}
}

func TestValidateDedupAndGap(t *testing.T) {
	// 100..104 连续 5 根 + 108 一根孤悬 + 100 重复
	in := []exchange.Candle{
		mk(100, 1, true), mk(101, 2, true), mk(102, 3, true),
		mk(103, 4, true), mk(104, 5, true),
		mk(108, 9, true),
		mk(100, 1, true), // 完全重复
	}
	out, err := Validate(in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 {
		t.Fatalf("应去重并保留最长连续段 5 根，得到 %d", len(out))
	}
}

func TestValidateConflictKeyRejected(t *testing.T) {
	in := []exchange.Candle{mk(100, 1, true), mk(100, 2, true)}
	if _, err := Validate(in, 1); err == nil {
		t.Fatal("同键不同 close 必须拒绝，不得静默修复")
	}
}

func TestValidateDropsUnconfirmed(t *testing.T) {
	in := []exchange.Candle{mk(100, 1, true), mk(101, 2, false)}
	out, err := Validate(in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].OpenTime != 100 {
		t.Fatal("未收盘 K 线必须剔除")
	}
}

func TestValidateIllegalOHLC(t *testing.T) {
	bad := mk(100, 1, true)
	bad.High = 0.5 // High < Low
	if _, err := Validate([]exchange.Candle{bad}, 1); err == nil {
		t.Fatal("OHLC 关系非法必须报错")
	}
}

func TestIntervalMs(t *testing.T) {
	cases := map[string]int64{"1m": 60_000, "15m": 900_000, "1H": 3_600_000, "4H": 14_400_000, "1D": 86_400_000, "xx": 0}
	for in, want := range cases {
		if got := IntervalMs(in); got != want {
			t.Errorf("IntervalMs(%q)=%d want %d", in, got, want)
		}
	}
}

func TestSnapshotDoubleWrite(t *testing.T) {
	dir := t.TempDir()
	s := NewSnapshotStore(dir)
	id1, err := s.Save("btc_1h", []exchange.Candle{mk(100, 1, true), mk(101, 2, true)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Save("btc_1h", []exchange.Candle{mk(100, 1, true), mk(101, 3, true)})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := s.LoadLatest("btc_1h")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[1].Close != 3 {
		t.Fatalf("指针应指向最新快照: %+v", cs)
	}
	if id1 == "" {
		t.Fatal("history_id 不能为空")
	}
	// manifest 应有两条登记
	b := readManifest(t, dir, "btc_1h")
	if b != 2 {
		t.Fatalf("manifest 应登记 2 条，得到 %d", b)
	}
}

func readManifest(t *testing.T, dir, name string) int {
	t.Helper()
	b, err := os.ReadFile(path.Join(dir, "snapshots", name, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Entries []any `json:"entries"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return len(m.Entries)
}
