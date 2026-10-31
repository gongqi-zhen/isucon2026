package isureal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

type fakeSessionSource struct {
	mu    sync.Mutex
	valid map[string]bool
	// users は、トークンごとの利用者の今の行（#58）。ないトークンでは、利用者の行を返さない。
	users map[string]User
	err   error
	calls int
}

func (s *fakeSessionSource) validate(_ context.Context, _ string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return fakeValidate(s.valid, s.users, checks), nil
}

// fakeValidate は、dbSessionCacheSource と同じく、fingerprint が変わった利用者の行だけを返す。
func fakeValidate(valid map[string]bool, users map[string]User, checks []sessionCacheCheck) map[string]sessionCacheValidation {
	result := map[string]sessionCacheValidation{}
	for _, check := range checks {
		if !valid[check.TokenHash] {
			continue
		}
		validation := sessionCacheValidation{}
		if user, ok := users[check.TokenHash]; ok {
			validation.Fingerprint = fakeFingerprint(user)
			if validation.Fingerprint != check.Fingerprint {
				validation.User = &user
			}
		}
		result[check.TokenHash] = validation
	}
	return result
}

func fakeFingerprint(user User) string {
	return fmt.Sprintf("%q", []string{user.ID, user.DisplayName, user.Bio, fmt.Sprint(user.ProfileImageID), user.Region, user.AccountType, user.Status})
}

// cachedSession は、利用者の行を持つセッション（getSessionFromHeader が put するもの）を作る。
func cachedSession(tokenHash string, user User) Session {
	return Session{UserID: user.ID, TokenHash: tokenHash, Region: user.Region, User: &user, userFingerprint: fakeFingerprint(user)}
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
	if value := os.Getenv("ISUREAL_SESSION_CACHE_REGIONS"); value != "" {
		t.Skipf("ISUREAL_SESSION_CACHE_REGIONS=%q overrides the default", value)
	}
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

func TestSessionCacheKeepsUserRowWithSession(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	user := User{ID: "u1", DisplayName: "before", Region: "europe", Status: "active"}
	c.put(cachedSession("t1", user), clock.now())
	s, ok := c.get("t1")
	if !ok || s.User == nil || s.User.DisplayName != "before" {
		t.Fatalf("get = %+v, %v; want the cached user row", s, ok)
	}
	s.User.DisplayName = "mutated"
	if s2, _ := c.get("t1"); s2.User.DisplayName != "before" {
		t.Fatal("get must return a copy of the user row")
	}
}

// 確かめ直しは、fingerprint が変わった利用者の行だけを受け取り、validatedAt と一緒に置き換える。
func TestSessionCacheRevalidateRefreshesUserRow(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	user := User{ID: "u1", DisplayName: "before", Region: "europe", Status: "active"}
	source := &fakeSessionSource{valid: map[string]bool{"t1": true}, users: map[string]User{"t1": user}}
	c.put(cachedSession("t1", user), clock.now())

	clock.advance(sessionCacheInterval)
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if s, ok := c.get("t1"); !ok || s.User == nil || s.User.DisplayName != "before" {
		t.Fatalf("unchanged row: get = %+v, %v", s, ok)
	}

	changed := user
	changed.DisplayName = "after"
	source.users["t1"] = changed
	clock.advance(sessionCacheInterval)
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if s, ok := c.get("t1"); !ok || s.User == nil || s.User.DisplayName != "after" {
		t.Fatalf("changed row: get = %+v, %v; want the new display name", s, ok)
	}
}

// 書く側がコミットして TTL 待った後は、コミットより前に読んだ利用者の行は使われない（約束1と2）。
func TestSessionCacheStaleUserRowExpiresBeforeWriterResponds(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	user := User{ID: "u1", DisplayName: "before", Region: "europe", Status: "active"}
	sentBeforeCommit := clock.now()
	clock.advance(50 * time.Millisecond) // ほかのプロセスで、表示名の変更をコミットした
	commit := clock.now()
	clock.advance(30 * time.Millisecond)
	c.put(cachedSession("t1", user), sentBeforeCommit) // 遅れていた読み取りが、古い行を入れる
	clock.t = commit.Add(sessionCacheTTL)              // 書く側が応答した時刻
	clock.advance(time.Nanosecond)
	if s, ok := c.get("t1"); ok {
		t.Fatalf("a user row read before the commit must not be used after the writer responds: %+v", s.User)
	}
}

// 確かめ直しのクエリを送った後に、エントリの行が別のクエリの（もっと古い）結果に入れ替わったときは、
// 「変わっていない」という結果でその行を確かめたことにしない。
func TestSessionCacheRevalidateDropsRowReplacedDuringQuery(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	current := User{ID: "u1", DisplayName: "current", Region: "europe", Status: "active"}
	stale := current
	stale.DisplayName = "stale"
	c.put(cachedSession("t1", current), clock.now())
	staleSentAt := clock.now().Add(time.Millisecond)
	clock.advance(sessionCacheInterval)
	source := &hookSessionSource{
		valid: map[string]bool{"t1": true},
		during: func() {
			// クエリの途中で、確かめ直しより前に送った別のクエリの結果が、新しい validatedAt で入る。
			clock.advance(5 * time.Millisecond)
			c.put(cachedSession("t1", stale), staleSentAt.Add(sessionCacheInterval-2*time.Millisecond))
		},
	}
	source.users = map[string]User{"t1": current}
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if s, ok := c.get("t1"); ok && s.User != nil && s.User.DisplayName == "stale" {
		t.Fatal("a row that the revalidation did not check must not be extended")
	}
}

func TestSessionCachePutReplacesUserRowOnlyWithNewerQuery(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	before := User{ID: "u1", DisplayName: "before", Region: "europe", Status: "active"}
	after := before
	after.DisplayName = "after"
	older := clock.now()
	clock.advance(10 * time.Millisecond)
	c.put(cachedSession("t1", after), clock.now())
	c.put(cachedSession("t1", before), older)
	if s, _ := c.get("t1"); s.User.DisplayName != "after" {
		t.Fatalf("an older query replaced the row: %q", s.User.DisplayName)
	}
	clock.advance(10 * time.Millisecond)
	newer := after
	newer.Bio = "newer"
	c.put(cachedSession("t1", newer), clock.now())
	if s, _ := c.get("t1"); s.User.Bio != "newer" {
		t.Fatalf("a newer query did not replace the row: %+v", s.User)
	}
}

// #110：sessionCacheIdle を超えて使われていないエントリは、確かめ直しの対象から外れて消える。
// 使われ続けるエントリは残り、確かめ直される。
func TestSessionCacheIdleEntriesAreDroppedFromRevalidation(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	source := &fakeSessionSource{valid: map[string]bool{"used": true, "idle": true}}
	c.put(Session{UserID: "u1", TokenHash: "used", Region: "europe"}, clock.now())
	c.put(Session{UserID: "u2", TokenHash: "idle", Region: "europe"}, clock.now())

	// 期限の直前までは、どちらも確かめ直しの対象。used だけがここで使われる。
	clock.advance(sessionCacheIdle - time.Millisecond)
	c.get("used")
	if checks := c.checks(); len(checks) != 2 {
		t.Fatalf("both entries are within the idle limit, checks = %v", checks)
	}
	// idle は期限を超えたので、確かめ直さずに消える。
	clock.advance(2 * time.Millisecond)
	checks := c.checks()
	if len(checks) != 1 || checks[0].TokenHash != "used" {
		t.Fatalf("checks = %v, want only the used entry", checks)
	}
	if hashes := c.tokenHashes(); len(hashes) != 1 || hashes[0] != "used" {
		t.Fatalf("entries = %v, want the idle entry dropped", hashes)
	}
	// 使われ続けるエントリは、期限を何度またいでも残る。
	for range 3 {
		clock.advance(sessionCacheIdle - time.Millisecond)
		c.get("used")
		if checks := c.checks(); len(checks) != 1 {
			t.Fatalf("the used entry must stay, checks = %v", checks)
		}
	}

	// 残ったエントリは、これまでどおり確かめ直されて延びる。
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	clock.advance(sessionCacheTTL - time.Millisecond)
	if _, ok := c.get("used"); !ok {
		t.Fatal("the used entry must be revalidated and usable")
	}
	// 消えたトークンは、次に使われたとき DB の結果が put で入り直す。
	if _, ok := c.get("idle"); ok {
		t.Fatal("the dropped entry must not be usable without a new validation")
	}
	c.put(Session{UserID: "u2", TokenHash: "idle", Region: "europe"}, clock.now())
	if _, ok := c.get("idle"); !ok {
		t.Fatal("a fresh validation must re-enter the dropped token")
	}
}

// get は、TTL を過ぎたエントリでも「使われた」とみなす（その後の DB の結果が put で入り直すため）。
// put は、既存のエントリでも使った時刻を進める。
func TestSessionCacheUseIsRecordedOnExpiredGetAndPut(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	clock.advance(sessionCacheIdle - time.Millisecond)
	if _, ok := c.get("t1"); ok {
		t.Fatal("entry past TTL must not be returned")
	}
	clock.advance(sessionCacheIdle - time.Millisecond)
	if checks := c.checks(); len(checks) != 1 {
		t.Fatalf("an expired get must count as use, checks = %v", checks)
	}
	clock.advance(sessionCacheIdle - time.Millisecond)
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	clock.advance(sessionCacheIdle - time.Millisecond)
	if checks := c.checks(); len(checks) != 1 {
		t.Fatalf("a put must count as use, checks = %v", checks)
	}
	clock.advance(2 * time.Millisecond)
	if checks := c.checks(); len(checks) != 0 {
		t.Fatalf("checks = %v, want the entry dropped once idle", checks)
	}
}

func TestSessionCacheIdleDefault(t *testing.T) {
	if sessionCacheIdle != 5*time.Second {
		t.Fatalf("sessionCacheIdle = %v, want 5s by default", sessionCacheIdle)
	}
	if sessionCacheIdle <= sessionCacheTTL {
		t.Fatal("the idle limit must be longer than the TTL, or entries would be dropped before they expire")
	}
}
