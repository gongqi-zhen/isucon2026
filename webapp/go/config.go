package isureal

import (
	"net"
	"net/url"
	"os"
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
}

func ConfigFromEnv() *Config {
	return &Config{
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
