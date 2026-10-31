package isureal

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Region     string
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string
	LogLevel   string
	PprofAddr  string
	ListenAddr string
	DBMaxConns int
	// 接続を閉じるまでのアイドルの時間と、作ってからの時間。0 は閉じない（#70）。
	DBConnMaxIdleTime time.Duration
	DBConnMaxLifetime time.Duration
	// 起動したときに作っておく接続の数。0 は作らない（#70）。
	DBWarmConns int
	// プールの接続を使う前に Ping する、アイドルの時間の閾値。0 は pgx の既定（1秒）のまま（#70、db_pool.go）。
	DBPingIdle time.Duration
}

func ConfigFromEnv() *Config {
	config := &Config{
		Region:     envOrDefault("ISUREAL_REGION", "asia"),
		DBHost:     envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:     envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:     envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword: envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:     envOrDefault("ISUREAL_DB_NAME", "isureal"),
		// TLS ハンドシェイクと暗号化を省く。サーバ間はルーター経由の閉じたネットワーク。
		DBSSLMode: envOrDefault("ISUREAL_DB_SSLMODE", "disable"),
		LogLevel:  envOrDefault("ISUREAL_LOG_LEVEL", "debug"),
		// pprof の待ち受けアドレス。"off" で無効。
		PprofAddr: envOrDefault("ISUREAL_PPROF_ADDR", "127.0.0.1:6060"),
		// asia2 で動かすときは、VPC 内のアドレスだけで待ち受ける（docs/asia-role-split.md）。
		ListenAddr: envOrDefault("ISUREAL_LISTEN_ADDR", ":8080"),
		// 全台の合計が PostgreSQL の max_connections を超えないように、台ごとに変えられるようにする。
		DBMaxConns: int(envInt64("ISUREAL_DB_MAX_CONNS", 64)),
	}
	// europe と america から asia2 に接続を作るには約 840ms かかるので、既定では閉じず、起動時に全部作っておく（#70）。
	config.DBConnMaxIdleTime = envDuration("ISUREAL_DB_CONN_MAX_IDLE_TIME", 0)
	config.DBConnMaxLifetime = envDuration("ISUREAL_DB_CONN_MAX_LIFETIME", 0)
	config.DBWarmConns = envNonNegativeInt("ISUREAL_DB_WARM_CONNS", config.DBMaxConns)
	config.DBPingIdle = envDuration("ISUREAL_DB_PING_IDLE", defaultDBPingIdle(config.Region))
	return config
}

// defaultDBPingIdle は、DB（asia2）と同じリージョンなら pgx の既定のままにする。asia では Ping の往復は約 1ms しかかからない。
// ほかのリージョンでは1往復が約 100ms なので、Ping は1分を超えてアイドルだった接続だけにする。
func defaultDBPingIdle(region string) time.Duration {
	if region == "asia" {
		return 0
	}
	return time.Minute
}

func (c *Config) DatabaseDSN() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.DBUser, c.DBPassword),
		Host:     net.JoinHostPort(c.DBHost, c.DBPort),
		Path:     "/" + c.DBName,
		RawQuery: url.Values{"sslmode": {c.DBSSLMode}}.Encode(),
	}
	return u.String()
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// envDuration は time.ParseDuration の形式（例："30s"、"0"）で読む。空、不正な値、負の値は既定値にする。
func envDuration(key string, defaultValue time.Duration) time.Duration {
	if value, err := time.ParseDuration(os.Getenv(key)); err == nil && value >= 0 {
		return value
	}
	return defaultValue
}

// envNonNegativeInt は 0 以上の整数を読む（0 も有効な値として扱う）。空、不正な値、負の値は既定値にする。
func envNonNegativeInt(key string, defaultValue int) int {
	if value, err := strconv.Atoi(os.Getenv(key)); err == nil && value >= 0 {
		return value
	}
	return defaultValue
}
