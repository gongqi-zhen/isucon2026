package isureal

import (
	"context"
	"testing"
	"time"
)

// #109：asia でもセッションキャッシュを有効にしたとき（ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america、
// ISUREAL_SESSION_CACHE_SOLE=1）に、europe と同じ約束がそのまま成り立つことを、DB なしで確かめる。
// 既定の設定（asia はキャッシュしない）は変えないので、テストの間だけ sessionCacheRegions を差し替える。

// withAllRegionsCached は、このテストの間だけ3リージョンすべてをキャッシュする設定にする。
func withAllRegionsCached(t *testing.T) {
	t.Helper()
	prev := sessionCacheRegions
	sessionCacheRegions = parseSessionCacheRegions("asia,europe,america")
	t.Cleanup(func() { sessionCacheRegions = prev })
}

func TestSessionCacheAsiaEnabledByRegionsSetting(t *testing.T) {
	withAllRegionsCached(t)
	if !sessionInvalidationNeedsWait("asia") {
		t.Fatal("with asia in ISUREAL_SESSION_CACHE_REGIONS, writers must wait for asia users (unless sole)")
	}
	c, clock := newTestSessionCache("asia")
	if !c.caching() {
		t.Fatal("an asia process must cache when asia is in the regions setting")
	}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "asia"}, clock.now())
	if s, ok := c.get("t1"); !ok || s.UserID != "u1" {
		t.Fatalf("get = %v, %v; want the asia session cached", s, ok)
	}
	// ほかのリージョンの利用者は、今まで通り入れない（RegionMiddleware で弾かれるので、引き直してよい）。
	c.put(Session{UserID: "u2", TokenHash: "t2", Region: "europe"}, clock.now())
	if _, ok := c.get("t2"); ok {
		t.Fatal("an asia process must not cache a europe user")
	}
}

// asia のプロセスが asia の唯一のキャッシュなら、asia の利用者の無効化は待たず、ほかのリージョンの利用者は待つ。
func TestSessionCacheAsiaSoleSkipsWaitOnlyForAsia(t *testing.T) {
	withAllRegionsCached(t)
	asia, _ := newTestSessionCache("asia")
	asia.sole = true
	if elapsed := waitedFor(asia, "asia"); elapsed >= sessionCacheTTL/2 {
		t.Fatalf("the sole cache of asia waited %v for an asia user, want no wait", elapsed)
	}
	for _, region := range []string{"europe", "america"} {
		if elapsed := waitedFor(asia, region); elapsed < sessionCacheTTL {
			t.Fatalf("asia waited %v for a %s user, want at least %v (%s1 caches it)", elapsed, region, sessionCacheTTL, region)
		}
	}
	// sole を付けていなければ（asia2 のアプリも動いているとき）、asia の利用者でも待つ。
	asia.sole = false
	if elapsed := waitedFor(asia, "asia"); elapsed < sessionCacheTTL {
		t.Fatalf("without ISUREAL_SESSION_CACHE_SOLE, asia waited %v, want at least %v", elapsed, sessionCacheTTL)
	}
	// europe のプロセスから見ると、asia の利用者は（asia1 がキャッシュしうるので）待つ対象になる。
	europe, _ := newTestSessionCache("europe")
	europe.sole = true
	if elapsed := waitedFor(europe, "asia"); elapsed < sessionCacheTTL {
		t.Fatalf("europe waited %v for an asia user, want at least %v once asia is cached", elapsed, sessionCacheTTL)
	}
}

// 約束3（evictedAt より前に送ったクエリの結果は残らない）は、asia でも同じ。
func TestSessionCacheAsiaRejectsPutFromQuerySentBeforeEviction(t *testing.T) {
	withAllRegionsCached(t)
	c, clock := newTestSessionCache("asia")
	c.sole = true
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "asia"}, clock.now())
	clock.advance(10 * time.Millisecond)
	sentBeforeCommit := clock.now()
	clock.advance(50 * time.Millisecond) // ここで asia1 自身がログアウトをコミットした
	c.waitInvalidation(context.Background(), "asia", "u1")
	if _, ok := c.get("t1"); ok {
		t.Fatal("the evicted token must not be usable")
	}
	clock.advance(20 * time.Millisecond)
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "asia"}, sentBeforeCommit)
	if _, ok := c.get("t1"); ok {
		t.Fatal("a session validated before the eviction must not be cached again")
	}
	c.put(Session{UserID: "u1", TokenHash: "t2", Region: "asia"}, clock.now())
	if s, ok := c.get("t2"); !ok || s.UserID != "u1" {
		t.Fatalf("get(t2) = %v, %v; want the session validated after the eviction", s, ok)
	}
}

// 初期化（resetSessionCache）は、asia を足しても今までと同じく、自分のキャッシュを消してから TTL 待つ。
func TestSessionCacheAsiaResetClearsEntries(t *testing.T) {
	withAllRegionsCached(t)
	c, clock := newTestSessionCache("asia")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "asia"}, clock.now())
	c.reset()
	if _, ok := c.get("t1"); ok {
		t.Fatal("reset must drop every entry")
	}
	if len(sessionCacheRegions) == 0 {
		t.Fatal("regions must not be empty, so resetSessionCache waits for the other processes")
	}
}
