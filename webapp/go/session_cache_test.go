package isureal

import (
	"context"
	"errors"
	"fmt"
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

// gatedSessionSource は、最初の1回だけ release が閉じられるまで返さない（遅れて返る確かめ直しを再現する）。
type gatedSessionSource struct {
	valid   map[string]bool
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (s *gatedSessionSource) validate(_ context.Context, _ string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		close(s.started)
		<-s.release
	}
	return fakeValidate(s.valid, nil, checks), nil
}

// 確かめ直しを同時に送るとき（#121）、後から送った結果が先に返っても、遅れて返った古い結果で validatedAt は戻らない。
func TestSessionCacheOutOfOrderRevalidationsDoNotGoBack(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	source := &gatedSessionSource{valid: map[string]bool{"t1": true}, started: make(chan struct{}), release: make(chan struct{})}

	clock.advance(sessionCacheInterval)
	oldSentAt := clock.now()
	done := make(chan error)
	go func() { done <- c.revalidate(context.Background(), source) }()
	<-source.started // 古い1本は oldSentAt に送られ、まだ返っていない

	clock.advance(sessionCacheInterval)
	newSentAt := clock.now()
	if err := c.revalidate(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	validatedAt := c.entries["t1"].validatedAt
	c.mu.Unlock()
	if !validatedAt.Equal(newSentAt) {
		t.Fatalf("validatedAt = %v, want the newer query %v (not the late one %v)", validatedAt, newSentAt, oldSentAt)
	}
}

// 古い1本が遅れて返る間に、消した利用者のエントリが入り直しても、消す前に送った古い1本では延ばさない（約束3）。
func TestSessionCacheLateRevalidationSentBeforeEvictionDoesNotExtend(t *testing.T) {
	c, clock := newTestSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	source := &gatedSessionSource{valid: map[string]bool{"t1": true}, started: make(chan struct{}), release: make(chan struct{})}
	clock.advance(sessionCacheInterval)
	done := make(chan error)
	go func() { done <- c.revalidate(context.Background(), source) }()
	<-source.started

	clock.advance(time.Millisecond)
	c.evictUser("u1")
	clock.advance(time.Millisecond)
	putAt := clock.now()
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, putAt)
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	validatedAt := c.entries["t1"].validatedAt
	c.mu.Unlock()
	if !validatedAt.Equal(putAt) {
		t.Fatalf("validatedAt = %v, want %v: a query sent before the eviction must not touch the entry", validatedAt, putAt)
	}
}

// blockingSessionSource は、ctx が終わるまで返さない（打ち切られる確かめ直しを再現する）。
// slowCalls 回目までだけ止まり、それより後は、すぐに返す（0 なら全部止まる）。
type blockingSessionSource struct {
	valid     map[string]bool
	slowCalls int
	mu        sync.Mutex
	calls     int
	inFlight  int
	maxFlight int
}

func (s *blockingSessionSource) validate(ctx context.Context, _ string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error) {
	s.mu.Lock()
	s.calls++
	slow := s.slowCalls == 0 || s.calls <= s.slowCalls
	s.inFlight++
	s.maxFlight = max(s.maxFlight, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if slow {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return fakeValidate(s.valid, nil, checks), nil
}

func (s *blockingSessionSource) stats() (calls, maxFlight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.maxFlight
}

func newRealClockSessionCache(region string) *sessionCache {
	c := &sessionCache{now: time.Now}
	c.start.Do(func() {})
	c.region = region
	c.enabled = sessionCacheRegions[region]
	c.entries = map[string]*sessionCacheEntry{}
	return c
}

// 1本が打ち切られるまで止まっていても、次の確かめ直しを待たずに送り、エントリは使えるまま（#121）。
func TestSessionCacheRunDoesNotWaitForSlowRevalidation(t *testing.T) {
	c := newRealClockSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, time.Now())
	source := &blockingSessionSource{valid: map[string]bool{"t1": true}, slowCalls: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx, source, 20*time.Millisecond)

	time.Sleep(sessionCacheTTL - 100*time.Millisecond) // 最初の1本はまだ打ち切られていない
	calls, _ := source.stats()
	if calls < 3 {
		t.Fatalf("calls = %d while the first revalidation is stuck, want later ones sent without waiting", calls)
	}
	time.Sleep(200 * time.Millisecond) // 最初に入れてから TTL を過ぎた
	if _, ok := c.get("t1"); !ok {
		t.Fatal("entry must stay usable: later revalidations should have refreshed it")
	}
}

// 同時に送るのは sessionCacheMaxInFlight 本まで（専用プールの接続数と同じ）。
func TestSessionCacheRunLimitsInFlight(t *testing.T) {
	c := newRealClockSessionCache("europe")
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, time.Now())
	source := &blockingSessionSource{valid: map[string]bool{"t1": true}}
	skippedBefore := sessionCacheStats.skipped.Load()
	ctx, cancel := context.WithCancel(context.Background())
	go c.run(ctx, source, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	cancel()
	_, maxFlight := source.stats()
	if maxFlight != sessionCacheMaxInFlight {
		t.Fatalf("max in flight = %d, want %d", maxFlight, sessionCacheMaxInFlight)
	}
	if sessionCacheStats.skipped.Load() == skippedBefore {
		t.Fatal("ticks while the limit is reached must be skipped")
	}
}

// recordingSessionSource は、送られたトークンを覚える。
type recordingSessionSource struct {
	valid map[string]bool
	last  []string
}

func (s *recordingSessionSource) validate(_ context.Context, _ string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error) {
	s.last = s.last[:0]
	for _, check := range checks {
		s.last = append(s.last, check.TokenHash)
	}
	return fakeValidate(s.valid, nil, checks), nil
}

// 最後に使ってから sessionCacheIdle を過ぎたエントリは、確かめ直しに送らずに消す（#110）。使い続けているものは残る。
func TestSessionCacheDropsIdleEntries(t *testing.T) {
	if sessionCacheIdle <= 0 {
		t.Skip("idle dropping is disabled")
	}
	c, clock := newTestSessionCache("europe")
	source := &recordingSessionSource{valid: map[string]bool{"t1": true, "t2": true}}
	c.put(Session{UserID: "u1", TokenHash: "t1", Region: "europe"}, clock.now())
	c.put(Session{UserID: "u2", TokenHash: "t2", Region: "europe"}, clock.now())
	for elapsed := time.Duration(0); elapsed <= sessionCacheIdle+sessionCacheTTL; elapsed += sessionCacheTTL / 2 {
		clock.advance(sessionCacheTTL / 2)
		if err := c.revalidate(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.get("t1"); !ok {
			t.Fatalf("t1 is used continuously and must stay cached (after %v)", elapsed)
		}
	}
	if len(source.last) != 1 || source.last[0] != "t1" {
		t.Fatalf("last revalidation sent %v, want only t1", source.last)
	}
	if _, ok := c.get("t2"); ok {
		t.Fatal("an idle entry must be dropped")
	}
}

// キャッシュするリージョンでは確かめ直し専用のプールを開き、しないリージョンでは接続を増やさない（#121）。
func TestOpenSessionCacheDB(t *testing.T) {
	config := &Config{Region: "europe", DBHost: "127.0.0.1", DBPort: "1", DBUser: "x", DBPassword: "x", DBName: "x", DBSSLMode: "disable"}
	shared, err := openDB(config)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	dedicated := openSessionCacheDB(config, shared)
	if dedicated == shared {
		t.Fatal("europe must use a dedicated pool")
	}
	defer dedicated.Close()
	if got := dedicated.Stats().MaxOpenConnections; got != sessionCacheMaxInFlight {
		t.Fatalf("dedicated pool max open = %d, want %d", got, sessionCacheMaxInFlight)
	}
	config.Region = "asia"
	if openSessionCacheDB(config, shared) != shared {
		t.Fatal("asia does not cache sessions and must not open another pool")
	}
}
