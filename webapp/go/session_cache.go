package isureal

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// セッション確認のキャッシュ（#38）。
//
// europe と america のアプリから DB への1往復は約100ms かかり、全 API が SessionMiddleware で1往復していた。
// そこで token_hash からセッションを引いた結果をプロセス内に持つ。
//
// 正しさは次の2つの約束で保つ（時計の同期は不要。どちらも各プロセスの単調時計だけを使う）。
//
//  1. 読む側：エントリは「DB で有効と確かめたクエリを送った時刻」（validatedAt）を持ち、
//     そこから sessionCacheTTL 以内のときだけ使う。古ければ DB を引き直す。
//     キャッシュを持つプロセスは sessionCacheInterval ごとに、持っている全トークンをまとめて1クエリで確かめ直す。
//  2. 書く側：セッションを無効にする操作（ログイン、ログアウト、退会、リージョン変更）は、
//     キャッシュを持ちうるリージョンが関わるとき、コミットしてから sessionCacheTTL 待って応答する。
//
// 書く側が応答した後に届いたリクエストの時刻を r、コミットの完了を c とすると、r > c + TTL。
// 読む側が使えるエントリは validatedAt >= r - TTL > c なので、確かめたクエリはコミットの後に送られている。
// そのクエリのスナップショットはコミットを含むので、無効になったセッションは有効と判定されない。
// コミットより前に送られたクエリで入ったエントリは、r の時点で TTL を過ぎているので使われない。
//
// asia は asia1 と asia2 の2プロセスに分かれ、/api/auth/ は asia1 だけが処理する。
// asia のアプリと DB の往復は約1ms で効果が小さいので、既定ではキャッシュしない（ISUREAL_SESSION_CACHE_REGIONS）。
// 全サーバで同じ設定にすること（書く側は、この設定で待つかどうかを決める）。

var (
	sessionCacheRegions  = parseSessionCacheRegions(envOrDefault("ISUREAL_SESSION_CACHE_REGIONS", "europe,america"))
	sessionCacheTTL      = time.Duration(envInt64("ISUREAL_SESSION_CACHE_TTL_MS", 400)) * time.Millisecond
	sessionCacheInterval = time.Duration(envInt64("ISUREAL_SESSION_CACHE_INTERVAL_MS", 100)) * time.Millisecond
	sessionCacheMax      = int(envInt64("ISUREAL_SESSION_CACHE_MAX", 100000))
)

// sessions はこのプロセスのセッションキャッシュ。リージョンは最初に使うときに決まる。
var sessions = &sessionCache{}

func parseSessionCacheRegions(value string) map[string]bool {
	regions := map[string]bool{}
	for _, region := range strings.Split(value, ",") {
		if region = strings.TrimSpace(region); region != "" {
			regions[region] = true
		}
	}
	return regions
}

// sessionInvalidationNeedsWait は、そのリージョンの利用者のセッションを無効にしたとき、
// ほかのプロセスのキャッシュに反映されるまで待つ必要があるかを返す。
func sessionInvalidationNeedsWait(region string) bool {
	return sessionCacheRegions[region]
}

// waitSessionInvalidation は、セッションを無効にする操作をコミットした後、応答する前に呼ぶ。
// 呼んだプロセスのキャッシュからはすぐに消し、ほかのプロセスのキャッシュに確実に反映されるまで待つ。
func waitSessionInvalidation(ctx context.Context, region string, userID string) {
	sessions.evictUser(userID)
	if !sessionInvalidationNeedsWait(region) {
		return
	}
	timer := time.NewTimer(sessionCacheTTL)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

type sessionCacheEntry struct {
	session     Session
	validatedAt time.Time
}

// sessionCacheSource は、トークンが今も有効かを DB で確かめる。テストで差し替えられるようにする。
type sessionCacheSource interface {
	validTokens(ctx context.Context, region string, tokenHashes []string) (map[string]bool, error)
}

type dbSessionCacheSource struct{ db *sqlx.DB }

func (s dbSessionCacheSource) validTokens(ctx context.Context, region string, tokenHashes []string) (map[string]bool, error) {
	var valid []string
	if err := s.db.SelectContext(ctx, &valid, `
  SELECT s.token_hash FROM login_sessions s JOIN users u ON u.id = s.user_id
  WHERE s.token_hash = ANY($1) AND s.active = true AND u.status = 'active' AND u.region = $2
 `, tokenHashes, region); err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(valid))
	for _, tokenHash := range valid {
		result[tokenHash] = true
	}
	return result, nil
}

type sessionCache struct {
	mu      sync.Mutex
	region  string
	enabled bool
	entries map[string]*sessionCacheEntry
	start   sync.Once
	now     func() time.Time
}

// init は最初に使うときに1回だけ呼ばれ、リージョンを決めて、キャッシュするなら確かめ直しのループを始める。
func (c *sessionCache) init(region string, source sessionCacheSource) {
	c.start.Do(func() {
		c.mu.Lock()
		c.region = region
		c.enabled = sessionCacheRegions[region]
		c.entries = map[string]*sessionCacheEntry{}
		if c.now == nil {
			c.now = time.Now
		}
		c.mu.Unlock()
		if c.enabled {
			go c.run(source)
		}
	})
}

// get は、TTL 以内に有効と確かめたセッションを返す。
func (c *sessionCache) get(tokenHash string) (*Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return nil, false
	}
	entry, ok := c.entries[tokenHash]
	if !ok || c.now().Sub(entry.validatedAt) > sessionCacheTTL {
		return nil, false
	}
	session := entry.session
	return &session, true
}

// put は、sentAt に送ったクエリで有効と分かったセッションを入れる。
// このプロセスのリージョンの利用者だけを入れる（ほかのリージョンの利用者は RegionMiddleware で弾かれるので、引き直してよい）。
func (c *sessionCache) put(session Session, sentAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || session.Region != c.region {
		return
	}
	if entry, ok := c.entries[session.TokenHash]; ok {
		if sentAt.After(entry.validatedAt) {
			entry.validatedAt = sentAt
		}
		return
	}
	if len(c.entries) >= sessionCacheMax {
		return
	}
	session.Token = ""
	c.entries[session.TokenHash] = &sessionCacheEntry{session: session, validatedAt: sentAt}
}

func (c *sessionCache) evictUser(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tokenHash, entry := range c.entries {
		if entry.session.UserID == userID {
			delete(c.entries, tokenHash)
		}
	}
}

func (c *sessionCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries != nil {
		c.entries = map[string]*sessionCacheEntry{}
	}
}

func (c *sessionCache) tokenHashes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	hashes := make([]string, 0, len(c.entries))
	for tokenHash := range c.entries {
		hashes = append(hashes, tokenHash)
	}
	return hashes
}

// revalidate は、持っている全トークンを1クエリで確かめ直す。
// 有効なものは validatedAt を送った時刻に進め、無効なものは消す。
// 失敗したときは何も変えない（エントリは TTL を過ぎると使われなくなり、DB を引き直す）。
func (c *sessionCache) revalidate(ctx context.Context, source sessionCacheSource) error {
	hashes := c.tokenHashes()
	if len(hashes) == 0 {
		return nil
	}
	sentAt := c.now()
	valid, err := source.validTokens(ctx, c.region, hashes)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tokenHash := range hashes {
		entry, ok := c.entries[tokenHash]
		if !ok {
			continue
		}
		if valid[tokenHash] {
			if sentAt.After(entry.validatedAt) {
				entry.validatedAt = sentAt
			}
		} else {
			delete(c.entries, tokenHash)
		}
	}
	return nil
}

func (c *sessionCache) run(source sessionCacheSource) {
	ticker := time.NewTicker(sessionCacheInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), sessionCacheTTL)
		if err := c.revalidate(ctx, source); err != nil {
			log.Printf("session cache revalidation failed: %v", err)
		}
		cancel()
	}
}

// resetSessionCache は初期化で呼ぶ。login_sessions は空になるので、このプロセスのキャッシュを消し、
// ほかのプロセスのキャッシュが確かめ直しで消えるまで待つ。
func resetSessionCache(ctx context.Context) {
	sessions.reset()
	if len(sessionCacheRegions) == 0 {
		return
	}
	timer := time.NewTimer(sessionCacheTTL)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
