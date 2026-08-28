package binance

import (
	"context"
	"errors"
	"testing"

	"github.com/HarveyBase/QuantForge/exchange"
)

func TestPlaceholderReturnsNotImplemented(t *testing.T) {
	c := New()
	ctx := context.Background()
	if c.Name() != "binance" {
		t.Fatalf("适配器名错误: %s", c.Name())
	}
	if _, err := c.GetCandles(ctx, "BTCUSDT", "1h", 10); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetCandles 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.GetTicker(ctx, "BTCUSDT"); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetTicker 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.GetInstrument(ctx, "BTCUSDT"); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetInstrument 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.GetBalances(ctx); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetBalances 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.PlaceOrder(ctx, exchange.OrderRequest{}); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("PlaceOrder 应返回 ErrNotImplemented: %v", err)
	}
	if err := c.CancelOrder(ctx, "BTCUSDT", "1"); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("CancelOrder 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.GetOrder(ctx, "BTCUSDT", "1"); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetOrder 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.GetOrderByClientID(ctx, "BTCUSDT", "c1"); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetOrderByClientID 应返回 ErrNotImplemented: %v", err)
	}
	if _, err := c.GetOpenOrders(ctx, "BTCUSDT"); !errors.Is(err, exchange.ErrNotImplemented) {
		t.Errorf("GetOpenOrders 应返回 ErrNotImplemented: %v", err)
	}
}
