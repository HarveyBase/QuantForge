// state_test.go 网格运行态持久化测试（T6）：
// 往返等价（重启后行为 = 从未重启）、配置漂移拒绝、损坏/越界状态拒绝、失败导入不落半态。
package grid

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
	"github.com/HarveyBase/QuantForge/strategy"
)

// ctxAtPos 指定价 + 账本持仓的驱动上下文（两实例同口径喂入）。
func ctxAtPos(price, position float64) *strategy.Context {
	c := ctxAt(price)
	c.Position = position
	return c
}

// TestGridStateRoundTripEquivalence 核心等价性：运行中途 Export → 新实例 Import →
// 对相同后续 K 线序列产出相同 OrderIntent（重启等价于从未重启）。
// 拆分点语义与真实崩溃一致：第 split 根已完整处理（含落盘），重启后从 split+1 根继续。
func TestGridStateRoundTripEquivalence(t *testing.T) {
	params := Params{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0.1, Spacing: "arith", StopOnBreak: true}
	prices := []float64{150, 140, 155, 90, 85, 120, 160, 165, 140, 175, 185}
	split := 6 // 崩溃发生在第 6 根（下标 5，价 120）处理完并落盘之后
	position := 0.3

	a, err := New(params) // 基准：从未重启
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(params) // 重启实例
	if err != nil {
		t.Fatal(err)
	}

	for i, px := range prices {
		ctx := ctxAtPos(px, position)
		if i < split {
			// 重启前：只驱动基准实例（成交镜像同样只打到它，导出态已含）
			for _, it := range a.OnCandle(ctx) {
				a.ApplyFill(it.Side, it.Qty, it.Price)
			}
			if i == split-1 {
				raw, err := a.ExportState()
				if err != nil {
					t.Fatalf("导出失败: %v", err)
				}
				if err := b.ImportState(raw); err != nil {
					t.Fatalf("导入失败: %v", err)
				}
			}
			continue
		}
		outA, outB := a.OnCandle(ctx), b.OnCandle(ctx)
		if !reflect.DeepEqual(outA, outB) {
			t.Fatalf("第 %d 根（价 %.0f）重启前后产出不一致:\n从未重启: %+v\n重启恢复: %+v", i, px, outA, outB)
		}
		for _, it := range outA { // 成交镜像同口径
			a.ApplyFill(it.Side, it.Qty, it.Price)
			b.ApplyFill(it.Side, it.Qty, it.Price)
		}
	}
	if !reflect.DeepEqual(a.Stats(), b.Stats()) {
		t.Fatalf("运行统计应一致: %+v vs %+v", a.Stats(), b.Stats())
	}
	if a.Stats().Rounds == 0 {
		t.Fatal("前置失败：样本应至少产生一轮成交（否则等价性验证无信息量）")
	}
}

// TestGridRestorePreventsDuplicateBootstrap 恢复的直接价值：同价重复收盘时，
// 恢复实例不再重新自举（冷启动会再发一张初始建仓单，与交易所遗留挂单重复）。
func TestGridRestorePreventsDuplicateBootstrap(t *testing.T) {
	params := Params{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0.1, Spacing: "arith", StopOnBreak: true}
	a, _ := New(params)
	a.OnCandle(ctxAtPos(160, 0)) // 启动：idx=2，初始建仓单
	raw, _ := a.ExportState()

	b, _ := New(params)
	if err := b.ImportState(raw); err != nil {
		t.Fatal(err)
	}
	// 同价再来一根：恢复实例无跨格 → 无订单
	if out := b.OnCandle(ctxAtPos(160, 0.1)); len(out) != 0 {
		t.Fatalf("恢复后同价收盘不应重复建仓: %+v", out)
	}
	// 对照：冷启动实例会重新自举，发初始建仓单（即生产环境的重复挂格问题）
	c, _ := New(params)
	out := c.OnCandle(ctxAtPos(160, 0.1))
	if len(out) != 1 || out[0].Side != exchange.Buy || !strings.Contains(out[0].Note, "初始建仓") {
		t.Fatalf("前置失败：冷启动应重新自举发初始建仓单: %+v", out)
	}
}

// TestGridImportConfigDrift 配置漂移（格数变化）必须报错，且失败导入不落半态。
func TestGridImportConfigDrift(t *testing.T) {
	src, _ := New(Params{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0.1, Spacing: "arith", StopOnBreak: true})
	src.OnCandle(ctxAtPos(150, 0))
	src.ApplyFill(exchange.Sell, 0.1, 160) // 制造 1 轮运行统计
	raw, _ := src.ExportState()

	dst, _ := New(Params{Lower: 100, Upper: 200, Grids: 6, QtyPerGrid: 0.1, Spacing: "arith", StopOnBreak: true})
	err := dst.ImportState(raw)
	if err == nil || !strings.Contains(err.Error(), "配置漂移") || !strings.Contains(err.Error(), "grids") {
		t.Fatalf("格数漂移必须明确报错: %v", err)
	}
	// 失败导入不落半态：仍按冷启动行为（重新自举建仓）
	if s := dst.Stats(); s.Rounds != 0 || s.Broke {
		t.Fatalf("失败导入不得部分生效: %+v", s)
	}
	if out := dst.OnCandle(ctxAtPos(150, 0)); len(out) != 1 || !strings.Contains(out[0].Note, "初始建仓") {
		t.Fatalf("失败导入后应保持冷启动行为: %+v", out)
	}
}

// TestGridImportMalformed 损坏 JSON / 版本不兼容 / last_idx 越界。
func TestGridImportMalformed(t *testing.T) {
	g, _ := New(Params{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0.1, Spacing: "arith", StopOnBreak: true})
	if err := g.ImportState(json.RawMessage(`{bad`)); err == nil || !strings.Contains(err.Error(), "解析失败") {
		t.Fatalf("损坏 JSON 必须报错: %v", err)
	}
	if err := g.ImportState(json.RawMessage(`{"version":99,"config":{"Lower":100,"Upper":200,"Grids":4,"QtyPerGrid":0.1,"Spacing":"arith","StopOnBreak":true}}`)); err == nil || !strings.Contains(err.Error(), "版本不兼容") {
		t.Fatalf("未知版本必须报错: %v", err)
	}
	if err := g.ImportState(json.RawMessage(`{"version":1,"config":{"Lower":100,"Upper":200,"Grids":4,"QtyPerGrid":0.1,"Spacing":"arith","StopOnBreak":true},"started":true,"last_idx":9}`)); err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("last_idx 越界必须报错: %v", err)
	}
}

// TestGridExportOmitsNothingOfRuntime 导出为合法 JSON 且含运行态字段（审计可读）。
func TestGridExportOmitsNothingOfRuntime(t *testing.T) {
	g, _ := New(Params{Lower: 100, Upper: 200, Grids: 4, QtyPerGrid: 0.1, Spacing: "arith", StopOnBreak: true})
	g.OnCandle(ctxAtPos(150, 0))
	raw, err := g.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("导出必须是合法 JSON: %v", err)
	}
	for _, key := range []string{"version", "started", "last_idx", "broke", "rounds", "realized", "config"} {
		if _, ok := probe[key]; !ok {
			t.Fatalf("导出缺少运行态字段 %q: %s", key, raw)
		}
	}
}
