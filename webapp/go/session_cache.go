package isureal

import (
	"context"
	"log"
	"os"
	"strconv"
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
//
// 待ちの省略（#62）：キャッシュに入れるのは、そのプロセスのリージョンの利用者だけ（put）。
// そのため、あるリージョンをキャッシュするプロセスが1つだけなら、そのリージョンの利用者のセッションを
// 有効なまま持ちうるのは、そのプロセスだけになる。そのプロセス自身が無効化を処理したときは、
// 自分のキャッシュから消せば足り、ほかのプロセスを待つ必要はない。
// 「1つだけ」であることはコードからは分からないので、ISUREAL_SESSION_CACHE_SOLE=1 で明示する（既定は待つ）。
//
// 待たない場合は、約束2の代わりに次の約束で、コミットより前に送ったクエリの結果が残らないようにする。
//
//  3. 消した側：evictUser は利用者ごとに消した時刻（evictedAt）を覚える。evictUser はコミットの後に呼ぶので、
//     evictedAt より前に送ったクエリはコミットより前に送られた可能性がある。その結果は put しないし、
//     revalidate でも validatedAt を進めない。evictedAt より後に送ったクエリはコミットの後なので、
//     無効になったセッションを有効と判定しない。
//
// 利用者の行のキャッシュ（#58）：エントリは、セッションと同じクエリで読んだ利用者の行（表示に使う列）も持ち、
// GET /api/me はそれを使う。確かめ直しのクエリは、行の md5（fingerprint）が変わった利用者の行だけを返す。
// 行と validatedAt は必ず同じクエリの結果で一緒に進めるので、約束1は利用者の行にもそのまま当てはまる。
// 約束2と3も同じで、利用者の行を変える操作（プロフィールの変更、プロフィール画像の設定と削除、退会、管理者による認定、初期化）は、
// コミットしてから waitSessionInvalidation（または resetSessionCache）を呼んで応答する。

var (
	sessionCacheRegions  = parseSessionCacheRegions(envOrDefault("ISUREAL_SESSION_CACHE_REGIONS", "europe,america"))
	sessionCacheTTL      = time.Duration(envInt64("ISUREAL_SESSION_CACHE_TTL_MS", 400)) * time.Millisecond
	sessionCacheInterval = time.Duration(envInt64("ISUREAL_SESSION_CACHE_INTERVAL_MS", 100)) * time.Millisecond
	sessionCacheMax      = int(envInt64("ISUREAL_SESSION_CACHE_MAX", 100000))
	// このプロセスのリージョンをキャッシュするのは、このプロセスだけであることを示す（#62）。
	// 同じリージョンのアプリを2つ以上動かすときは、どのプロセスにも設定しないこと。
	sessionCacheSole = parseSessionCacheSole(os.Getenv("ISUREAL_SESSION_CACHE_SOLE"))
	// evictedAt を覚えておく時間。確かめ直しのクエリは sessionCacheTTL で打ち切るので、それより十分に長くする。
	// これより古い evictedAt を忘れても、そのころに送ったクエリの結果は TTL を過ぎていて put されない。
	sessionEvictionMemory = 10 * time.Second
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

// parseSessionCacheSole は、値がない、または読めないときは false（待つ）を返す。
func parseSessionCacheSole(value string) bool {
	sole, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && sole
}

// sessionInvalidationNeedsWait は、そのリージョンの利用者のセッションを無効にしたとき、
// ほかのプロセスのキャッシュに反映されるまで待つ必要があるかを返す。
func sessionInvalidationNeedsWait(region string) bool {
	return sessionCacheRegions[region]
}

// waitSessionInvalidation は、セッションを無効にする操作をコミットした後、応答する前に呼ぶ。
// region は無効にしたセッションの利用者のリージョン（キャッシュに入ったときのリージョン）。
// 呼んだプロセスのキャッシュからはすぐに消し、ほかのプロセスのキャッシュに確実に反映されるまで待つ。
func waitSessionInvalidation(ctx context.Context, region string, userID string) {
	sessions.waitInvalidation(ctx, region, userID)
}

func (c *sessionCache) waitInvalidation(ctx context.Context, region string, userID string) {
	c.evictUser(userID)
	if !sessionInvalidationNeedsWait(region) || c.solelyCaches(region) {
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
	// session.User は、validatedAt に送ったクエリで読んだ利用者の行（#58）。nil なら GET /api/me は DB を引く。
	session     Session
	validatedAt time.Time
}

// sessionUserFingerprint は、users u の行の md5。確かめ直しで、変わった利用者の行だけを返してもらうのに使う。
var sessionUserFingerprint = `md5(row(` + qualifiedUserColumns("u") + `)::text)`

// sessionCacheCheck は、確かめ直すトークンと、持っている利用者の行の fingerprint（持っていなければ空）。
type sessionCacheCheck struct {
	TokenHash   string
	Fingerprint string
}

// sessionCacheValidation は、有効だったトークンの確かめ直しの結果。
// User は、利用者の行の fingerprint が送ったものと違うときだけ入る（同じなら持っている行が今も正しい）。
type sessionCacheValidation struct {
	Fingerprint string
	User        *User
}

// sessionCacheSource は、トークンが今も有効かを DB で確かめる。テストで差し替えられるようにする。
// 結果に入っていないトークンは無効。
type sessionCacheSource interface {
	validate(ctx context.Context, region string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error)
}

type dbSessionCacheSource struct{ db *sqlx.DB }

func (s dbSessionCacheSource) validate(ctx context.Context, region string, checks []sessionCacheCheck) (map[string]sessionCacheValidation, error) {
	tokenHashes := make([]string, len(checks))
	fingerprints := make([]string, len(checks))
	for i, check := range checks {
		tokenHashes[i], fingerprints[i] = check.TokenHash, check.Fingerprint
	}
	// 利用者の行は、fingerprint が変わったときだけ返す（europe と america から asia2 の DB へ送る量を増やさない）。
	var rows []struct {
		TokenHash   string `db:"token_hash"`
		Fingerprint string `db:"fingerprint"`
		nullableUser
	}
	if err := s.db.SelectContext(ctx, &rows, `
  SELECT c.token_hash, f.fingerprint, changed.*
  FROM unnest($1::text[], $2::text[]) AS c(token_hash, fingerprint)
  JOIN login_sessions s ON s.token_hash = c.token_hash AND s.active = true
  JOIN users u ON u.id = s.user_id AND u.status = 'active' AND u.region = $3
  CROSS JOIN LATERAL (SELECT `+sessionUserFingerprint+` AS fingerprint) f
  LEFT JOIN LATERAL (
    SELECT `+qualifiedUserColumns("u")+` WHERE f.fingerprint IS DISTINCT FROM c.fingerprint
  ) changed ON true
 `, tokenHashes, fingerprints, region); err != nil {
		return nil, err
	}
	result := make(map[string]sessionCacheValidation, len(rows))
	for _, row := range rows {
		validation := sessionCacheValidation{Fingerprint: row.Fingerprint}
		if row.ID != nil {
			user, err := row.user()
			if err != nil {
				return nil, err
			}
			validation.User = &user
		}
		result[row.TokenHash] = validation
	}
	return result, nil
}

type sessionCache struct {
	mu      sync.Mutex
	region  string
	enabled bool
	// sole は、このプロセスのリージョンをキャッシュするのが、このプロセスだけであること（ISUREAL_SESSION_CACHE_SOLE）。
	sole    bool
	entries map[string]*sessionCacheEntry
	// evictedAt は、利用者ごとに evictUser で最後に消した時刻（約束3）。sessionEvictionMemory を過ぎたら忘れる。
	evictedAt map[string]time.Time
	start     sync.Once
	now       func() time.Time
}

// init は最初に使うときに1回だけ呼ばれ、リージョンを決めて、キャッシュするなら確かめ直しのループを始める。
func (c *sessionCache) init(region string, source sessionCacheSource) {
	c.start.Do(func() {
		c.mu.Lock()
		c.region = region
		c.enabled = sessionCacheRegions[region]
		c.sole = sessionCacheSole
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
	if session.User != nil {
		user := *session.User
		session.User = &user
	}
	return &session, true
}

// put は、sentAt に送ったクエリで有効と分かったセッションを入れる。
// session.User と session.userFingerprint は、同じクエリで読んだ利用者の行とその fingerprint（#58）。
// このプロセスのリージョンの利用者だけを入れる（ほかのリージョンの利用者は RegionMiddleware で弾かれるので、引き直してよい）。
// 次の結果は入れない。
//   - その利用者を evictUser で消した時刻より前に送ったクエリの結果（約束3）
//   - TTL より前に送ったクエリの結果（入れても使われない。evictedAt を忘れた後に、古い結果が入らないようにもする）
func (c *sessionCache) put(session Session, sentAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || session.Region != c.region {
		return
	}
	if c.now().Sub(sentAt) > sessionCacheTTL || c.sentBeforeEviction(session.UserID, sentAt) {
		return
	}
	if entry, ok := c.entries[session.TokenHash]; ok {
		if sentAt.After(entry.validatedAt) {
			// 利用者の行も同じクエリの結果に置き換える（行がなければ、古い行を残さずに捨てる）。
			entry.session.User, entry.session.userFingerprint = session.User, session.userFingerprint
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

// evictUser は、その利用者のエントリを消し、消した時刻を覚える。セッションを無効にした書き込みのコミットの後に呼ぶこと。
func (c *sessionCache) evictUser(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tokenHash, entry := range c.entries {
		if entry.session.UserID == userID {
			delete(c.entries, tokenHash)
		}
	}
	if !c.enabled {
		return
	}
	now := c.now()
	if c.evictedAt == nil {
		c.evictedAt = map[string]time.Time{}
	}
	for id, at := range c.evictedAt {
		if now.Sub(at) > sessionEvictionMemory {
			delete(c.evictedAt, id)
		}
	}
	c.evictedAt[userID] = now
}

// sentBeforeEviction は、sentAt に送ったクエリが、その利用者を最後に消した時刻より前（同時を含む）に送られたかを返す。
// c.mu を持って呼ぶこと。
func (c *sessionCache) sentBeforeEviction(userID string, sentAt time.Time) bool {
	evictedAt, ok := c.evictedAt[userID]
	return ok && !sentAt.After(evictedAt)
}

// caching は、このプロセスがセッションをキャッシュするかを返す。init の後に呼ぶこと。
func (c *sessionCache) caching() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// solelyCaches は、region の利用者のセッションを有効なまま持ちうるのが、このプロセスだけかを返す。
func (c *sessionCache) solelyCaches(region string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sole && c.enabled && c.region == region
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

func (c *sessionCache) checks() []sessionCacheCheck {
	c.mu.Lock()
	defer c.mu.Unlock()
	checks := make([]sessionCacheCheck, 0, len(c.entries))
	for tokenHash, entry := range c.entries {
		fingerprint := entry.session.userFingerprint
		if entry.session.User == nil {
			fingerprint = ""
		}
		checks = append(checks, sessionCacheCheck{TokenHash: tokenHash, Fingerprint: fingerprint})
	}
	return checks
}

// revalidate は、持っている全トークンを1クエリで確かめ直す。
// 有効なものは validatedAt を送った時刻に進め、無効なものは消す。
// その利用者を消した時刻より前に送ったクエリでは、有効でも validatedAt を進めない（約束3）。
// 利用者の行は、validatedAt と一緒に、同じクエリの結果に合わせる（#58）。
// 行が返らなかった（変わっていない）のに、送った後にエントリの行が入れ替わっていたときは、行を捨てる（GET /api/me は DB を引く）。
// 失敗したときは何も変えない（エントリは TTL を過ぎると使われなくなり、DB を引き直す）。
func (c *sessionCache) revalidate(ctx context.Context, source sessionCacheSource) error {
	checks := c.checks()
	if len(checks) == 0 {
		return nil
	}
	sentAt := c.now()
	valid, err := source.validate(ctx, c.region, checks)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, check := range checks {
		entry, ok := c.entries[check.TokenHash]
		if !ok {
			continue
		}
		validation, isValid := valid[check.TokenHash]
		if !isValid {
			delete(c.entries, check.TokenHash)
			continue
		}
		if !sentAt.After(entry.validatedAt) || c.sentBeforeEviction(entry.session.UserID, sentAt) {
			continue
		}
		if validation.User != nil {
			entry.session.User, entry.session.userFingerprint = validation.User, validation.Fingerprint
		} else if entry.session.userFingerprint != validation.Fingerprint {
			// 送った後に、エントリの行が別のクエリの結果に入れ替わっていた。この結果では確かめられないので捨てる。
			entry.session.User = nil
		}
		entry.validatedAt = sentAt
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
