package isureal

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// DB の接続プール（#70）。
//
// europe1 と america1 から asia2 の DB への1往復は約 100ms、接続を作るのは約 840ms かかる（docs/cross-region-db.md）。
// そこで次の3つを行う。
//
//  1. 接続をアイドルの時間や作ってからの時間で閉じない（server.go。Config.DBConnMaxIdleTime と DBConnMaxLifetime）。
//  2. 起動したときに、接続を Config.DBWarmConns 本まで作っておく（startDBPoolWarmup）。
//  3. pgx は、前回使ってから1秒を超えた接続を使う前に Ping する。europe と america では、そのたびに1往復増える。
//     Config.DBPingIdle が正なら、Ping するのはそれを超えてアイドルだった接続だけにする（openDB）。

const (
	dbWarmupTimeout    = 30 * time.Second
	dbWarmupAttempts   = 3
	dbWarmupRetryDelay = 5 * time.Second
)

// openDB は sqlx.Open("pgx", config.DatabaseDSN()) と同じ設定で DB を開く。違うのは、接続を使う前の Ping の条件だけ。
// プールの大きさや閉じるまでの時間は、呼び出し側（server.go）で設定する。
func openDB(config *Config) (*sqlx.DB, error) {
	connConfig, err := pgx.ParseConfig(config.DatabaseDSN())
	if err != nil {
		return nil, err
	}
	var options []stdlib.OptionOpenDB
	if threshold := config.DBPingIdle; threshold > 0 {
		options = append(options, stdlib.OptionShouldPing(func(_ context.Context, params stdlib.ShouldPingParams) bool {
			return shouldPingBeforeReuse(params.IdleDuration, threshold)
		}))
	}
	return sqlx.NewDb(stdlib.OpenDB(*connConfig, options...), "pgx"), nil
}

// shouldPingBeforeReuse は、プールの接続を使う前に Ping するかを返す（true なら Ping し、失敗した接続は捨てて別の接続を使う）。
// threshold を超えてアイドルだった接続だけを Ping する。
//
// Ping を省いても、DB 側で閉じられた接続（DB の再起動や pg_terminate_backend）にクエリを送った場合は、
// 別の接続でやり直され、クエリは成功する（db_pool_test.go で確かめている）。
// 相手から何も届かなくなった接続（ネットワークの断）は、Go の TCP keepalive（既定 15秒）で切れる。
// 切れる前に threshold 以内で使うとクエリが失敗しうるが、Ping しても同じく往復の失敗を待つことになる。
func shouldPingBeforeReuse(idle, threshold time.Duration) bool {
	return idle > threshold
}

// startDBPoolWarmup は、起動を待たせずに、接続を n 本まで作っておく。n が 0 以下なら何もしない。
// 走行の開始直後に同時に届くリクエストが、それぞれ接続を作る（europe と america では約 840ms）ことのないようにする。
// 失敗しても起動は止めず、ログに残して何回かやり直す。
func startDBPoolWarmup(db *sql.DB, n int) {
	if n <= 0 {
		return
	}
	go func() {
		for attempt := 1; ; attempt++ {
			start := time.Now()
			held, err := warmDBPool(context.Background(), db, n, dbWarmupTimeout)
			stats := db.Stats()
			if err == nil {
				log.Printf("db pool warmed: held %d connections, open %d, idle %d, in %s", held, stats.OpenConnections, stats.Idle, time.Since(start).Round(time.Millisecond))
				return
			}
			log.Printf("db pool warmup failed (attempt %d/%d): held %d of %d connections, open %d: %v", attempt, dbWarmupAttempts, held, n, stats.OpenConnections, err)
			if attempt >= dbWarmupAttempts {
				return
			}
			time.Sleep(dbWarmupRetryDelay)
		}
	}()
}

// warmDBPool は、プールから接続を n 本同時に借りてから、まとめて返す。借りた本数を返す。
// 同時に借りるので、空いている接続が足りない分は新しく作られる。返した接続は MaxIdleConns の範囲でプールに残る。
// n は MaxOpenConns を超えないように切り詰める（超えた分は借りられず、timeout まで待つことになるため）。
// 借りている間（europe と america では接続を作る約 840ms）、ほかのリクエストは空いた接続を待つ。起動直後だけに使うこと。
func warmDBPool(ctx context.Context, db *sql.DB, n int, timeout time.Duration) (int, error) {
	if maxOpen := db.Stats().MaxOpenConnections; maxOpen > 0 && n > maxOpen {
		n = maxOpen
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conns := make([]*sql.Conn, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conns[i], errs[i] = db.Conn(ctx)
		}()
	}
	wg.Wait()
	held := 0
	var firstErr error
	for i, conn := range conns {
		if conn != nil {
			held++
			conn.Close()
		} else if firstErr == nil {
			firstErr = errs[i]
		}
	}
	if firstErr != nil {
		return held, fmt.Errorf("%d of %d connections failed: %w", n-held, n, firstErr)
	}
	return held, nil
}
