package isureal

import (
	"encoding/json"
	"expvar"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// readDebugVars は、/debug/vars と同じハンドラ（expvar.Handler）の応答を JSON として読む。
func readDebugVars(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	recorder := httptest.NewRecorder()
	expvar.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/vars", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &vars); err != nil {
		t.Fatalf("invalid JSON from /debug/vars: %v\n%s", err, recorder.Body.String())
	}
	return vars
}

func TestLRUCacheCountsHitsMissesAndEvictions(t *testing.T) {
	cache := newLRUCache[string, int](2)
	now := time.Now()
	cache.now = func() time.Time { return now }
	cache.Get("missing")
	cache.Add("a", 1, 1, time.Time{})
	cache.Add("b", 2, 1, time.Time{})
	cache.Get("a")
	cache.Add("a", 10, 1, time.Time{})         // 同じキーの置き換えは追い出しに数えない
	cache.Add("c", 3, 1, time.Time{})          // b が追い出される
	cache.Add("d", 4, 1, now.Add(time.Second)) // a が追い出される
	now = now.Add(2 * time.Second)
	cache.Get("d") // 期限切れは外れ（消えるが、追い出しには数えない）

	stats := cache.Stats()
	want := lruCacheStats{Len: 1, Cost: 1, MaxCost: 2, Hits: 1, Misses: 2, Evictions: 2}
	if stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
}

func TestSessionCacheCountsHitsAndMisses(t *testing.T) {
	now := time.Now()
	cache := &sessionCache{now: func() time.Time { return now }}
	cache.start.Do(func() {})
	cache.region, cache.enabled = "europe", true
	cache.entries = map[string]*sessionCacheEntry{}
	cache.get("missing")
	cache.put(Session{Region: "europe", UserID: "u1", TokenHash: "t1"}, now)
	if _, ok := cache.get("t1"); !ok {
		t.Fatal("t1 should be cached")
	}
	now = now.Add(sessionCacheTTL + time.Millisecond)
	if _, ok := cache.get("t1"); ok {
		t.Fatal("t1 should be stale")
	}
	stats := cache.stats()
	if !stats.Enabled || stats.Region != "europe" || stats.Entries != 1 || stats.Max != sessionCacheMax || stats.Hits != 1 || stats.Misses != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestNotificationHubWaiterCount(t *testing.T) {
	hub := newNotificationHub(&fakeNotificationSource{})
	if n := hub.waiterCount(); n != 0 {
		t.Fatalf("waiters = %d, want 0", n)
	}
	waiter := hub.subscribe("u1", "asia")
	if n := hub.waiterCount(); n != 1 {
		t.Fatalf("waiters = %d, want 1", n)
	}
	hub.unsubscribe(waiter)
	if n := hub.waiterCount(); n != 0 {
		t.Fatalf("waiters = %d, want 0", n)
	}
}

func TestDebugVarsExposeMetrics(t *testing.T) {
	publishMetrics(nil)
	publishMetrics(nil) // 2回目は登録し直さない（expvar.Publish の panic にならない）
	before := imageObjectCache.Stats()
	imageObjectCache.Get("metrics-test-missing")

	vars := readDebugVars(t)
	for _, name := range []string{"isureal_db", "isureal_caches", "isureal_session_cache", "isureal_notification_hub"} {
		if _, ok := vars[name]; !ok {
			t.Errorf("%s is missing from /debug/vars", name)
		}
	}
	var caches map[string]lruCacheStats
	if err := json.Unmarshal(vars["isureal_caches"], &caches); err != nil {
		t.Fatalf("isureal_caches: %v", err)
	}
	for _, name := range []string{"signed_url", "verified_signature", "image_owner_key", "image_object", "image_validity", "jpeg_verdict"} {
		if _, ok := caches[name]; !ok {
			t.Errorf("cache %s is missing", name)
		}
	}
	if caches["image_object"].Misses != before.Misses+1 {
		t.Errorf("image_object misses = %d, want %d", caches["image_object"].Misses, before.Misses+1)
	}
	var session sessionCacheStats
	if err := json.Unmarshal(vars["isureal_session_cache"], &session); err != nil {
		t.Fatalf("isureal_session_cache: %v", err)
	}
	var hub map[string]int
	if err := json.Unmarshal(vars["isureal_notification_hub"], &hub); err != nil {
		t.Fatalf("isureal_notification_hub: %v", err)
	}
	if _, ok := hub["waiters"]; !ok {
		t.Errorf("waiters is missing: %v", hub)
	}
}
