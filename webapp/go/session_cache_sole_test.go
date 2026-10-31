package isureal

import (
	"context"
	"testing"
	"time"
)

// #62：リージョンをキャッシュするのがこのプロセスだけのとき、待ちを省く。
// その代わりに、消す前に送ったクエリの結果が残らないこと（約束3）を確かめる。

type hookSessionSource struct {
	valid  map[string]bool
	users  map[string]User
	during func()
}

func (s *hookSessionSource) validate(_ context.Context, _ string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error) {
	if s.during != nil {
		s.during()
	}
	return fakeValidate(s.valid, s.users, checks), nil
}

func TestParseSessionCacheSole(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "0": false, "false": false, "no": false, "yes": false,
		"1": true, "true": true, "TRUE": true, " 1 ": true,
	} {
		if got := parseSessionCacheSole(value); got != want {
			t.Errorf("parseSessionCacheSole(%q) = %v, want %v", value, got, want)
		}
	}
}

func waitedFor(c *sessionCache, region string) time.Duration {
	start := time.Now()
	c.waitInvalidation(context.Background(), region, "nobody")
	return time.Since(start)
}

func TestSessionCacheSoleSkipsWaitOnlyForOwnRegion(t *testing.T) {
	europe, _ := newTestSessionCache("europe")
	europe.sole = true
	if elapsed := waitedFor(europe, "europe"); elapsed >= sessionCacheTTL/2 {
		t.Fatalf("the sole cache of europe waited %v for a europe user, want no wait", elapsed)
	}
	if elapsed := waitedFor(europe, "america"); elapsed < sessionCacheTTL {
		t.Fatalf("europe waited %v for an america user, want at least %v (america1 may cache it)", elapsed, sessionCacheTTL)
	}
	if elapsed := waitedFor(europe, "asia"); elapsed >= sessionCacheTTL/2 {
		t.Fatalf("asia is not cached, but waited %v", elapsed)
	}

	europe.sole = false
	if elapsed := waitedFor(europe, "europe"); elapsed < sessionCacheTTL {
		t.Fatalf("without ISUREAL_SESSION_CACHE_SOLE, europe waited %v, want at least %v", elapsed, sessionCacheTTL)
	}

	// asia はキャッシュしないので、sole を設定しても europe の利用者を待たずに済ませてはいけない。
	asia, _ := newTestSessionCache("asia")
	asia.sole = true
	if elapsed := waitedFor(asia, "europe"); elapsed < sessionCacheTTL {
		t.Fatalf("asia waited %v for a europe user, want at least %v", elapsed, sessionCacheTTL)
	}

	// まだ初期化していない（リージョンが決まっていない）プロセスは、待つ。
	uninitialized := &sessionCache{sole: true}
	if elapsed := waitedFor(uninitialized, "europe"); elapsed < sessionCacheTTL {
		t.Fatalf("an uninitialized cache waited %v, want at least %v", elapsed, sessionCacheTTL)
	}
}

// コミットより前に送ったセッションの確認が、evictUser の後に put しても、古いトークンは使われない。
// 待ちを省いたときは、書く側の応答の直後にリクエストが届くので、TTL を待たずに使われないことが必要。
func TestSessionCacheRejectsPutFromQuerySentBeforeEviction(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	clock.advance(10 * time.Millisecond)
	sentBeforeCommit := clock.now()
	clock.advance(50 * time.Millisecond) // ここで書く側がコミットした
	clock.advance(time.Millisecond)
	c.evictUser("u1")
	evictedAt := clock.now()
	clock.advance(20 * time.Millisecond) // 遅れていた読み取りが、コミットより前の結果を入れようとする
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, sentBeforeCommit)
	if _, ok := c.get("t1"); ok {
		t.Fatal("a session validated before the eviction must not be cached again")
	}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, evictedAt)
	if _, ok := c.get("t1"); ok {
		t.Fatal("a query sent at the eviction time may have been sent before the commit")
	}

	// 消した後に送ったクエリ（新しいログインのトークン）は、今まで通り入る。
	c.put(Session{UserID: "u1", TokenHash: "t2", Region: "europe"}, clock.now())
	if s, ok := c.get("t2"); !ok || s.UserID != "u1" {
		t.Fatalf("get(t2) = %v, %v; want the session validated after the eviction", s, ok)
	}
	// ほかの利用者には影響しない。
	c.put(Session{UserID: "u2", TokenHash: "t3", Region: "europe"}, sentBeforeCommit)
	if _, ok := c.get("t3"); !ok {
		t.Fatal("eviction of u1 must not affect u2")
	}
}

// 確かめ直しのクエリが消す前に送られ、その間に書く側がコミットして消した場合も、古いトークンは使われない。
// 同じ間に、遅れていた読み取りが古い結果を入れようとしても同じ。
func TestSessionCacheRevalidateSentBeforeEvictionDoesNotResurrect(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	lateReaderSentAt := clock.now()
	source := &hookSessionSource{
		valid: map[string]bool{"t1": true}, // クエリはコミットより前に送られたので、まだ有効と返す
		during: func() {
			clock.advance(30 * time.Millisecond) // クエリの途中で、書く側がコミットして消す
			c.evictUser("u1")
			clock.advance(10 * time.Millisecond)
			c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, lateReaderSentAt)
		},
	}
	clock.advance(5 * time.Millisecond)
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.get("t1"); ok {
		t.Fatal("a revalidation sent before the eviction must not make the old token usable")
	}
}

// 消した後のエントリを、消す前に送った確かめ直しで延ばさない。
func TestSessionCacheRevalidateSentBeforeEvictionDoesNotExtend(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	clock.advance(time.Millisecond)
	c.evictUser("u1")
	// 消した時刻より古い validatedAt のエントリが残っていたとする（evictUser と同時に入ったものなど）。
	c.mu.Lock()
	c.entries["t1"] = &sessionCacheEntry{session: Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, validatedAt: clock.now().Add(-time.Millisecond)}
	c.mu.Unlock()
	source := &hookSessionSource{valid: map[string]bool{"t1": true}}
	// 確かめ直しのクエリを、消した時刻と同じ時刻に送る。
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	validatedAt := c.entries["t1"].validatedAt
	c.mu.Unlock()
	if !validatedAt.Before(clock.now()) {
		t.Fatal("a revalidation sent at or before the eviction must not move validatedAt forward")
	}
}

func TestSessionCacheIgnoresPutOlderThanTTL(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	sentAt := clock.now()
	clock.advance(sessionCacheTTL + time.Millisecond)
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, sentAt)
	if len(c.tokenHashes()) != 0 {
		t.Fatal("a result older than TTL is never usable and must not be stored")
	}
}

// evictedAt は sessionEvictionMemory を過ぎたら忘れる。忘れた後でも、そのころに送った古い結果は TTL を過ぎていて入らない。
func TestSessionCacheEvictionMemoryIsPruned(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	sentBeforeEviction := clock.now()
	clock.advance(time.Millisecond)
	c.evictUser("u1")
	clock.advance(sessionEvictionMemory + time.Millisecond)
	c.evictUser("u2")
	c.mu.Lock()
	_, remembered := c.evictedAt["u1"]
	size := len(c.evictedAt)
	c.mu.Unlock()
	if remembered || size != 1 {
		t.Fatalf("evictedAt still has u1 (size %d), want it pruned", size)
	}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, sentBeforeEviction)
	if _, ok := c.get("t1"); ok {
		t.Fatal("an old result must not be stored after its eviction time is forgotten")
	}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	if _, ok := c.get("t1"); !ok {
		t.Fatal("a fresh result should be stored")
	}
}

// キャッシュしないプロセス（asia や、初期化の前）では、evictUser は時刻を覚えない。
func TestSessionCacheEvictUserWithoutCache(t *testing.T) {
	uninitialized := &sessionCache{}
	uninitialized.evictUser("u1") // now が nil でも落ちないこと
	asia, _ := newTestSessionCache("asia")
	asia.evictUser("u1")
	if len(asia.evictedAt) != 0 {
		t.Fatal("a process without a cache must not remember evictions")
	}
}
