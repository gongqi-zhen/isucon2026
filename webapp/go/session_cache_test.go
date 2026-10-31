package isureal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSessionSource struct {
	mu    sync.Mutex
	valid map[string]bool
	err   error
	calls int
}

func (s *fakeSessionSource) validTokens(_ context.Context, _ string, tokenHashes []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	result := map[string]bool{}
	for _, tokenHash := range tokenHashes {
		if s.valid[tokenHash] {
			result[tokenHash] = true
		}
	}
	return result, nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestSessionCache(region string) (*sessionCache, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := &sessionCache{now: clock.now}
	// init のループは使わず、テストから revalidate を呼ぶ。
	c.start.Do(func() {})
	c.region = region
	c.enabled = sessionCacheRegions[region]
	c.entries = map[string]*sessionCacheEntry{}
	return c, clock
}

func TestSessionCacheRegionsDefault(t *testing.T) {
	if !sessionCacheRegions["europe"] || !sessionCacheRegions["america"] || sessionCacheRegions["asia"] {
		t.Fatalf("default cache regions = %v, want europe and america only", sessionCacheRegions)
	}
	if !sessionInvalidationNeedsWait("europe") || sessionInvalidationNeedsWait("asia") {
		t.Fatal("writers must wait only for caching regions")
	}
}

func TestSessionCacheUsesEntryOnlyWithinTTL(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	if s, ok := c.get("t1"); !ok || s.UserID != "u1" {
		t.Fatalf("get = %v, %v; want cached session", s, ok)
	}
	clock.advance(sessionCacheTTL)
	if _, ok := c.get("t1"); !ok {
		t.Fatal("entry exactly at TTL should still be usable")
	}
	clock.advance(time.Millisecond)
	if _, ok := c.get("t1"); ok {
		t.Fatal("entry older than TTL must not be used")
	}
}

func TestSessionCacheIgnoresOtherRegionsAndDisabledProcesses(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "asia"}, clock.now())
	if _, ok := c.get("t1"); ok {
		t.Fatal("a session of another region must not be cached")
	}
	asia, clock2 := newTestSessionCache("asia")
	asia.put(Session{UserID: "u1", TokenHash: "t1", Region: "asia"}, clock2.now())
	if _, ok := asia.get("t1"); ok {
		t.Fatal("asia does not cache by default")
	}
}

func TestSessionCacheRevalidateRefreshesAndEvicts(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	source := &fakeSessionSource{valid: map[string]bool{"t1": true}}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	c.put(Session{UserID: "u2", TokenHash: "t2", Region: "europe"}, clock.now())

	clock.advance(sessionCacheTTL - time.Millisecond)
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	clock.advance(sessionCacheTTL - time.Millisecond)
	if _, ok := c.get("t1"); !ok {
		t.Fatal("revalidated entry should be usable for another TTL")
	}
	if _, ok := c.get("t2"); ok {
		t.Fatal("entry reported invalid must be evicted")
	}
	if len(c.tokenHashes()) != 1 {
		t.Fatalf("entries = %v, want only t1", c.tokenHashes())
	}
}

func TestSessionCacheRevalidateFailureKeepsTTL(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	source := &fakeSessionSource{err: errors.New("db down")}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	clock.advance(sessionCacheTTL / 2)
	if err := c.revalidate(context.Background(), source); err == nil {
		t.Fatal("want error")
	}
	clock.advance(sessionCacheTTL/2 + time.Millisecond)
	if _, ok := c.get("t1"); ok {
		t.Fatal("a failed revalidation must not extend the entry")
	}
}

// コミットより前に送ったクエリで入ったエントリ（同時に処理中だったリクエストが遅れて入れたもの）は、
// 書く側が TTL 待って応答した後には使われない。
func TestSessionCacheLateInsertFromQueryBeforeCommitExpiresBeforeWriterResponds(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	sentBeforeCommit := clock.now()
	clock.advance(50 * time.Millisecond) // ここで書く側がコミットした
	commit := clock.now()
	c.evictUser("u1")
	clock.advance(80 * time.Millisecond) // 遅れていた読み取りが、古い結果を入れる
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, sentBeforeCommit)
	clock.t = commit.Add(sessionCacheTTL) // 書く側が応答した時刻
	clock.advance(time.Nanosecond)        // その後に届いたリクエスト
	if _, ok := c.get("t1"); ok {
		t.Fatal("stale entry validated before the commit must not be usable after the writer responds")
	}
}

func TestSessionCachePutDoesNotMoveValidatedAtBackwards(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	old := clock.now().Add(-time.Second)
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, old)
	clock.advance(sessionCacheTTL)
	if _, ok := c.get("t1"); !ok {
		t.Fatal("an older validation must not shorten a newer one")
	}
}

func TestWaitSessionInvalidation(t *testing.T) {
	start := time.Now()
	waitSessionInvalidation(context.Background(), "asia", "nobody")
	if elapsed := time.Since(start); elapsed > sessionCacheTTL/2 {
		t.Fatalf("asia waited %v, want no wait", elapsed)
	}
	start = time.Now()
	waitSessionInvalidation(context.Background(), "europe", "nobody")
	if elapsed := time.Since(start); elapsed < sessionCacheTTL {
		t.Fatalf("europe waited %v, want at least %v", elapsed, sessionCacheTTL)
	}
}
