package isureal

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// #70 の接続プール。DB を使うテストは ISUREAL_TEST_DSN を指定したときだけ実行する（空の DB でよい。データは変えない）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55470/isureal?sslmode=disable' go test -run 'TestDBPool' -v ./...

func TestShouldPingBeforeReuse(t *testing.T) {
	for _, tc := range []struct {
		idle time.Duration
		want bool
	}{
		{idle: 0, want: false},
		{idle: 30 * time.Second, want: false},
		{idle: time.Minute, want: false},
		{idle: time.Minute + time.Millisecond, want: true},
	} {
		if got := shouldPingBeforeReuse(tc.idle, time.Minute); got != tc.want {
			t.Errorf("shouldPingBeforeReuse(%s, 1m) = %v, want %v", tc.idle, got, tc.want)
		}
	}
}

func TestConfigFromEnvDBPool(t *testing.T) {
	keys := []string{"ISUREAL_REGION", "ISUREAL_DB_MAX_CONNS", "ISUREAL_DB_CONN_MAX_IDLE_TIME", "ISUREAL_DB_CONN_MAX_LIFETIME", "ISUREAL_DB_WARM_CONNS", "ISUREAL_DB_PING_IDLE"}
	for _, tc := range []struct {
		name         string
		env          map[string]string
		wantIdle     time.Duration
		wantLifetime time.Duration
		wantWarm     int
		wantPingIdle time.Duration
	}{
		{name: "europe の既定", env: map[string]string{"ISUREAL_REGION": "europe"}, wantWarm: 64, wantPingIdle: time.Minute},
		{name: "america の既定", env: map[string]string{"ISUREAL_REGION": "america"}, wantWarm: 64, wantPingIdle: time.Minute},
		{name: "asia は pgx の既定の Ping", env: map[string]string{"ISUREAL_REGION": "asia"}, wantWarm: 64, wantPingIdle: 0},
		{name: "温める本数は MaxConns に合わせる", env: map[string]string{"ISUREAL_REGION": "asia", "ISUREAL_DB_MAX_CONNS": "48"}, wantWarm: 48, wantPingIdle: 0},
		{
			name: "環境変数で変えられる",
			env: map[string]string{
				"ISUREAL_REGION": "europe", "ISUREAL_DB_CONN_MAX_IDLE_TIME": "5m", "ISUREAL_DB_CONN_MAX_LIFETIME": "1h",
				"ISUREAL_DB_WARM_CONNS": "0", "ISUREAL_DB_PING_IDLE": "1s",
			},
			wantIdle: 5 * time.Minute, wantLifetime: time.Hour, wantWarm: 0, wantPingIdle: time.Second,
		},
		{
			name:     "0 は pgx の既定の Ping",
			env:      map[string]string{"ISUREAL_REGION": "europe", "ISUREAL_DB_PING_IDLE": "0"},
			wantWarm: 64, wantPingIdle: 0,
		},
		{
			name: "不正な値と負の値は既定値",
			env: map[string]string{
				"ISUREAL_REGION": "europe", "ISUREAL_DB_CONN_MAX_IDLE_TIME": "abc", "ISUREAL_DB_CONN_MAX_LIFETIME": "-1m",
				"ISUREAL_DB_WARM_CONNS": "-3", "ISUREAL_DB_PING_IDLE": "10",
			},
			wantWarm: 64, wantPingIdle: time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range keys {
				t.Setenv(key, tc.env[key])
			}
			config := ConfigFromEnv()
			if config.DBConnMaxIdleTime != tc.wantIdle || config.DBConnMaxLifetime != tc.wantLifetime ||
				config.DBWarmConns != tc.wantWarm || config.DBPingIdle != tc.wantPingIdle {
				t.Fatalf("idle=%s lifetime=%s warm=%d pingIdle=%s, want idle=%s lifetime=%s warm=%d pingIdle=%s",
					config.DBConnMaxIdleTime, config.DBConnMaxLifetime, config.DBWarmConns, config.DBPingIdle,
					tc.wantIdle, tc.wantLifetime, tc.wantWarm, tc.wantPingIdle)
			}
		})
	}
}

// closedPort は、今は誰も待ち受けていない 127.0.0.1 のポートを返す。
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	return port
}

// DB に接続できなくても、温める処理はエラーを返すだけで、止まったり panic したりしない。
func TestDBPoolWarmupUnreachable(t *testing.T) {
	db, err := openDB(&Config{DBHost: "127.0.0.1", DBPort: closedPort(t), DBUser: "isureal", DBPassword: "isureal", DBName: "isureal", DBSSLMode: "disable", DBPingIdle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// MaxOpenConns を超える本数を頼んでも、MaxOpenConns に切り詰めるので timeout まで待たない。
	db.SetMaxOpenConns(2)
	start := time.Now()
	held, err := warmDBPool(context.Background(), db.DB, 5, 10*time.Second)
	if err == nil || held != 0 {
		t.Fatalf("held=%d err=%v, want held=0 and an error", held, err)
	}
	if !strings.Contains(err.Error(), "2 of 2 connections failed") {
		t.Fatalf("err=%v, want the count capped at MaxOpenConns", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("warmDBPool took %s", elapsed)
	}
	// startDBPoolWarmup は呼び出し元を待たせない。
	start = time.Now()
	startDBPoolWarmup(db.DB, 2)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("startDBPoolWarmup blocked for %s", elapsed)
	}
}

func dbPoolTestConfig(t *testing.T, pingIdle time.Duration) *Config {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	return &Config{
		DBHost: u.Hostname(), DBPort: u.Port(), DBUser: u.User.Username(), DBPassword: password,
		DBName: strings.TrimPrefix(u.Path, "/"), DBSSLMode: "disable", DBPingIdle: pingIdle,
	}
}

func TestDBPoolWarmupDB(t *testing.T) {
	db, err := openDB(dbPoolTestConfig(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.Rebind("SELECT ?"); got != "SELECT $1" {
		t.Fatalf("Rebind = %q, want the same bind type as sqlx.Open(\"pgx\")", got)
	}
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(6)
	held, err := warmDBPool(context.Background(), db.DB, 6, 10*time.Second)
	if err != nil || held != 6 {
		t.Fatalf("held=%d err=%v, want 6 connections", held, err)
	}
	if stats := db.Stats(); stats.OpenConnections != 6 || stats.Idle != 6 {
		t.Fatalf("open=%d idle=%d, want 6 idle connections kept in the pool", stats.OpenConnections, stats.Idle)
	}
	// 温めた接続はそのまま使える。
	var one int
	if err := db.Get(&one, `SELECT 1`); err != nil || one != 1 {
		t.Fatalf("SELECT 1 = %d, %v", one, err)
	}
}

// terminateAndQuery は、プールの1本だけの接続を DB 側で切ってから、すぐにもう一度クエリを送る。
func terminateAndQuery(t *testing.T, db *sqlx.DB) (oldPID, newPID int, err error) {
	t.Helper()
	admin, openErr := sqlx.Open("pgx", os.Getenv("ISUREAL_TEST_DSN"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer admin.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Get(&oldPID, `SELECT pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := admin.Get(&terminated, `SELECT pg_terminate_backend($1)`, oldPID); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend = %v, %v", terminated, err)
	}
	// サーバが接続を閉じたことが、こちらに届くのを待つ。前回使ってから1秒未満のうちにクエリを送る。
	time.Sleep(200 * time.Millisecond)
	err = db.Get(&newPID, `SELECT pg_backend_pid()`)
	return oldPID, newPID, err
}

// DB の再起動などでサーバが閉じた接続にクエリを送っても、Ping を省いたまま、別の接続でやり直して成功する。
// Linux のループバックでも同じことを確かめた（テストのバイナリを postgres:16 のコンテナの中で実行）。
func TestDBPoolTerminatedConnIsReplaced(t *testing.T) {
	db, err := openDB(dbPoolTestConfig(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldPID, newPID, err := terminateAndQuery(t, db)
	if err != nil {
		t.Fatalf("query after pg_terminate_backend failed: %v", err)
	}
	if newPID == oldPID {
		t.Fatalf("pid %d was reused after it was terminated", newPID)
	}
}
