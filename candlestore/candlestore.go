// Package candlestore SQLite K 线缓存库（docs/02 数据层持久化）：
// fetch 拉取的历史与 serve 实时收盘增量统一入库，图表/回测从库读全历史。
// 唯一键 exchange|symbol|interval|open_time（与 market.Validate 同口径），upsert 幂等。
package candlestore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // 纯 Go 驱动（无 cgo，跨平台构建）

	"github.com/HarveyBase/QuantForge/exchange"
)

// Store K 线库（线程安全；单写多读由 SQLite 串行保证 + 进程内互斥）。
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

// Open 打开/创建 dataDir/candles.db。
func Open(dataDir string) (*Store, error) {
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "candles.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("candlestore: 打开失败: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS candles (
		exchange TEXT NOT NULL,
		symbol   TEXT NOT NULL,
		interval TEXT NOT NULL,
		open_time INTEGER NOT NULL,
		open REAL, high REAL, low REAL, close REAL, volume REAL, confirmed INTEGER,
		PRIMARY KEY (exchange, symbol, interval, open_time)
	) WITHOUT ROWID`); err != nil {
		db.Close()
		return nil, fmt.Errorf("candlestore: 建表失败: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_candles_lookup ON candles(exchange, symbol, interval, open_time DESC)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("candlestore: 建索引失败: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭。
func (s *Store) Close() error { return s.db.Close() }

// Upsert 批量写入（同键覆盖：未收盘→已收盘的修正天然幂等）。
func (s *Store) Upsert(candles []exchange.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO candles
		(exchange, symbol, interval, open_time, open, high, low, close, volume, confirmed)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(exchange, symbol, interval, open_time) DO UPDATE SET
		open=excluded.open, high=excluded.high, low=excluded.low,
		close=excluded.close, volume=excluded.volume, confirmed=excluded.confirmed`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, c := range candles {
		if _, err := stmt.Exec(c.Exchange, c.Symbol, c.Interval, c.OpenTime,
			c.Open, c.High, c.Low, c.Close, c.Volume, boolInt(c.Confirmed)); err != nil {
			tx.Rollback()
			return fmt.Errorf("candlestore: 写入失败 %s: %w", c.Key(), err)
		}
	}
	return tx.Commit()
}

// Latest 最近 n 根（时间升序返回）。
func (s *Store) Latest(exName, symbol, interval string, n int) ([]exchange.Candle, error) {
	rows, err := s.db.Query(`SELECT exchange, symbol, interval, open_time, open, high, low, close, volume, confirmed
		FROM candles WHERE exchange=? AND symbol=? AND interval=?
		ORDER BY open_time DESC LIMIT ?`, exName, symbol, interval, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []exchange.Candle
	for rows.Next() {
		c, ok := scanCandle(rows)
		if !ok {
			return nil, rows.Err()
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// DESC 取出后翻转为升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Count 区间内根数（数据连续性诊断用）。
func (s *Store) Count(exName, symbol, interval string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM candles WHERE exchange=? AND symbol=? AND interval=?`,
		exName, symbol, interval).Scan(&n)
	return n, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanCandle(rows *sql.Rows) (exchange.Candle, bool) {
	var c exchange.Candle
	var confirmed int
	if err := rows.Scan(&c.Exchange, &c.Symbol, &c.Interval, &c.OpenTime,
		&c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &confirmed); err != nil {
		return c, false
	}
	c.Confirmed = confirmed == 1
	return c, true
}
