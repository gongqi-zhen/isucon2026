package isureal

import (
	"database/sql"
	"expvar"
	"sync"
)

// 接続プールとキャッシュの指標（#108）。
//
// expvar を import すると、net/http/pprof と同じ http.DefaultServeMux に /debug/vars が登録される。
// pprof のサーバ（Config.PprofAddr、既定 127.0.0.1:6060）から JSON で読める：
//
//	curl -s 127.0.0.1:6060/debug/vars | jq '{isureal_db, isureal_caches, isureal_session_cache, isureal_notification_hub}'
//
// 値は読まれたときに計算する（expvar.Func）。リクエストの処理に足しているのは、各キャッシュのアトミックなカウンタの加算だけ。
// 読み方と、結果からの判断（asia1 の DB の接続数、画像キャッシュの上限）は docs/metrics.md にある。

var (
	metricsOnce sync.Once
	// metricsDB は、/debug/vars の isureal_db が読むプール。NewServer が設定する（テストでは nil のままでもよい）。
	metricsDB   *sql.DB
	metricsDBMu sync.Mutex
)

// publishMetrics は、/debug/vars に指標を登録する。expvar.Publish は同じ名前を2回登録すると panic するので、1回だけ行う。
// db は後から差し替えられる（NewServer をテストで何度も呼ぶため）。
func publishMetrics(db *sql.DB) {
	metricsDBMu.Lock()
	metricsDB = db
	metricsDBMu.Unlock()
	metricsOnce.Do(func() {
		expvar.Publish("isureal_db", expvar.Func(dbStats))
		expvar.Publish("isureal_caches", expvar.Func(cacheStats))
		expvar.Publish("isureal_session_cache", expvar.Func(func() any { return sessions.stats() }))
		expvar.Publish("isureal_notification_hub", expvar.Func(notificationHubStats))
	})
}

// dbStats は database/sql の接続プールの統計。WaitCount と WaitDuration が増えていれば、接続が足りずに待っている。
func dbStats() any {
	metricsDBMu.Lock()
	db := metricsDB
	metricsDBMu.Unlock()
	if db == nil {
		return nil
	}
	return db.Stats()
}

// cacheStats は、プロセス内の LRU キャッシュ（image_cache.go、jpeg_validate.go）の状態と累計のカウンタ。
func cacheStats() any {
	return map[string]lruCacheStats{
		"signed_url":         signedURLCache.Stats(),
		"verified_signature": verifiedSignatureCache.Stats(),
		"image_owner_key":    imageOwnerKeyCache.Stats(),
		"image_object":       imageObjectCache.Stats(),
		"image_validity":     imageValidityCache.Stats(),
		"jpeg_verdict":       jpegVerdictCache.Stats(),
	}
}

// notificationHubStats は、SSE の待機中の接続の数。ハブがまだ作られていなければ 0。
func notificationHubStats() any {
	hub := notificationHub.Load()
	if hub == nil {
		return map[string]int{"waiters": 0}
	}
	return map[string]int{"waiters": hub.waiterCount()}
}
