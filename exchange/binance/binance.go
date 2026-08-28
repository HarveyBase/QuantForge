// Package binance Binance 适配器占位（二期实现）。
// 接口形状与 okx 对齐：现货 + USDT 本位永续，REST /api/v3 + /fapi/v1，HMAC SHA256 鉴权。
package binance

import (
	"context"

	"github.com/HarveyBase/QuantForge/exchange"
)

type Client struct{}

func New() *Client { return &Client{} }

func (c *Client) Name() string { return "binance" }

func (c *Client) GetCandles(ctx context.Context, symbol, interval string, limit int) ([]exchange.Candle, error) {
	return nil, exchange.ErrNotImplemented
}

func (c *Client) GetTicker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	return exchange.Ticker{}, exchange.ErrNotImplemented
}

func (c *Client) GetInstrument(ctx context.Context, instID string) (exchange.Instrument, error) {
	return exchange.Instrument{}, exchange.ErrNotImplemented
}

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	return nil, exchange.ErrNotImplemented
}

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (exchange.Order, error) {
	return exchange.Order{}, exchange.ErrNotImplemented
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	return exchange.ErrNotImplemented
}

func (c *Client) GetOrder(ctx context.Context, symbol, orderID string) (exchange.Order, error) {
	return exchange.Order{}, exchange.ErrNotImplemented
}

func (c *Client) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (exchange.Order, error) {
	return exchange.Order{}, exchange.ErrNotImplemented
}

func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]exchange.Order, error) {
	return nil, exchange.ErrNotImplemented
}
