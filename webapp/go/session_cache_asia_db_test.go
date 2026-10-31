package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// #109：asia のアプリとして起動し、asia でもセッションキャッシュを有効にした状態
// （ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america、ISUREAL_SESSION_CACHE_SOLE=1）で、
// ログイン・ログアウト・退会・PATCH /api/me・初期化の後に、古いセッションや古い利用者の行が使われないことを、
// 実際の PostgreSQL と HTTP ハンドラで確かめる。
//
// キャッシュはプロセスに1つで、設定は起動時の環境変数で決まる。そのため、このテストだけを別のプロセスで実行する。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55438/isureal?sslmode=disable' \
//	ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america ISUREAL_SESSION_CACHE_SOLE=1 \
//	go test -run TestSessionCacheAsiaDB -v ./...
//
// 設定がないときは skip する。

var (
	asiaSessionTestServerOnce sync.Once
	asiaSessionTestServer     *httptest.Server
)

// newAsiaSessionTestEnv は asia のアプリとしてテストサーバを起動する。
func newAsiaSessionTestEnv(t *testing.T) *sessionTestEnv {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	if !sessionCacheRegions["asia"] || !sessionCacheSole {
		t.Skip("run with ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america ISUREAL_SESSION_CACHE_SOLE=1")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	config := &Config{
		Region: "asia", DBHost: u.Hostname(), DBPort: u.Port(), DBUser: u.User.Username(), DBPassword: password,
		DBName: strings.TrimPrefix(u.Path, "/"), DBSSLMode: "disable", LogLevel: "error", PprofAddr: "off",
		ListenAddr: ":0", DBMaxConns: 32,
	}
	asiaSessionTestServerOnce.Do(func() { asiaSessionTestServer = httptest.NewServer(NewServer(config)) })
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	env := &sessionTestEnv{t: t, db: db, server: asiaSessionTestServer}
	// キャッシュはセッションの確認で初めて初期化される。別のリージョンのテストサーバが先に初期化していたら、このプロセスでは確かめられない。
	env.do("GET", "/api/me", "not-a-token", nil)
	sessions.mu.Lock()
	region, enabled, sole := sessions.region, sessions.enabled, sessions.sole
	sessions.mu.Unlock()
	if region != "asia" || !enabled || !sole {
		t.Skipf("session cache was initialized for %q (enabled=%v, sole=%v); run only TestSessionCacheAsiaDB in this process", region, enabled, sole)
	}
	return env
}

// newAsiaUser は asia の利用者を登録してログインし、トークンをキャッシュに入れた状態で返す。
func (env *sessionTestEnv) newAsiaUser() (username, secretKey, token string) {
	env.t.Helper()
	username, secretKey = registerSoleTestUser(env, "asia")
	token, _ = loginSoleTestUser(env, username, secretKey)
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusOK {
		env.t.Fatalf("me: %d", status)
	}
	return username, secretKey, token
}

func TestSessionCacheAsiaDBMeIsServedFromCache(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	_, _, token := env.newAsiaUser()
	sum := sha256.Sum256([]byte(token))
	cached, ok := sessions.get(hex.EncodeToString(sum[:]))
	if !ok || cached.Region != "asia" {
		t.Fatalf("asia token should be cached after a successful request (ok=%v, region=%q)", ok, cached.Region)
	}
	if cached.User == nil {
		t.Fatal("the user row should be cached with the session, so GET /api/me needs no round trip (#58)")
	}
}

func TestSessionCacheAsiaDBLogoutSkipsWaitAndInvalidates(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	_, _, token := env.newAsiaUser()
	start := time.Now()
	if status, _ := env.do("POST", "/api/auth/logout", token, nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	assertNoWait(t, "logout of an asia user on the sole asia process", time.Since(start))
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("me right after logout: %d, want 401", status)
	}
}

func TestSessionCacheAsiaDBReloginInvalidatesOldTokenImmediately(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	username, secretKey, oldToken := env.newAsiaUser()
	newToken, elapsed := loginSoleTestUser(env, username, secretKey)
	assertNoWait(t, "re-login", elapsed)
	if status, _ := env.do("GET", "/api/me", oldToken, nil); status != http.StatusUnauthorized {
		t.Fatalf("old token right after re-login: %d, want 401", status)
	}
	if status, _ := env.do("GET", "/api/me", newToken, nil); status != http.StatusOK {
		t.Fatalf("new token: %d, want 200", status)
	}
}

func TestSessionCacheAsiaDBAccountDeletionSkipsWait(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	_, _, token := env.newAsiaUser()
	start := time.Now()
	if status, _ := env.do("POST", "/api/me/account-deletion", token, nil); status != http.StatusNoContent {
		t.Fatalf("account deletion: %d", status)
	}
	assertNoWait(t, "account deletion", time.Since(start))
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("me right after account deletion: %d, want 401", status)
	}
}

// PATCH /api/me：表示名の変更はキャッシュした利用者の行（#58）に、リージョンの変更は RegionMiddleware に、すぐ反映される。
func TestSessionCacheAsiaDBUpdateMeIsReflectedImmediately(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	_, _, token := env.newAsiaUser()
	start := time.Now()
	if status, body := env.do("PATCH", "/api/me", token, map[string]string{"displayName": "renamed"}); status != http.StatusOK {
		t.Fatalf("rename: %d %v", status, body)
	}
	assertNoWait(t, "display name change", time.Since(start))
	if status, me := env.do("GET", "/api/me", token, nil); status != http.StatusOK || me["displayName"] != "renamed" {
		t.Fatalf("me after rename: %d %v", status, me)
	}
	start = time.Now()
	if status, body := env.do("PATCH", "/api/me", token, map[string]string{"region": "europe"}); status != http.StatusOK {
		t.Fatalf("patch region: %d %v", status, body)
	}
	assertNoWait(t, "region change from asia on the asia process", time.Since(start))
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusForbidden {
		t.Fatalf("me right after moving to europe: %d, want 403", status)
	}
}

// ほかのリージョンの利用者（europe1 がキャッシュしうる）を asia のプロセスで無効にしたときは、今まで通り待つ。
func TestSessionCacheAsiaDBOtherRegionUserStillWaits(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	username, secretKey := registerSoleTestUser(env, "europe")
	loginSoleTestUser(env, username, secretKey)
	if _, elapsed := loginSoleTestUser(env, username, secretKey); elapsed < sessionCacheTTL {
		t.Fatalf("re-login of a europe user on the asia process took %v, want at least %v", elapsed, sessionCacheTTL)
	}
}

// 初期化（/api/initialize は init.sh を実行するので、ここでは login_sessions を空にして resetSessionCache を呼ぶ）の後は、
// 古いトークンがすぐに使えない。
func TestSessionCacheAsiaDBInitializeClearsCache(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	_, _, token := env.newAsiaUser()
	env.exec(`DELETE FROM login_sessions`)
	resetSessionCache(context.Background())
	for i := 0; i < 10; i++ {
		if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
			t.Fatalf("request %d after initialize: %d, want 401", i, status)
		}
	}
}

// ログアウトの間も読み取りが並行して走り、コミットより前の結果を入れようとする。待たずに応答した直後から、1回も通らないこと。
func TestSessionCacheAsiaDBConcurrentReadersDoNotResurrect(t *testing.T) {
	env := newAsiaSessionTestEnv(t)
	for iteration := 0; iteration < 10; iteration++ {
		_, _, token := env.newAsiaUser()
		stop := make(chan struct{})
		var after atomic.Bool
		var bad, okBefore atomic.Int64
		var wg sync.WaitGroup
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					checkAfter := after.Load()
					req, _ := http.NewRequest("GET", env.server.URL+"/api/me", nil)
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("X-Client-IP", "10.109.0.1")
					res, err := sessionReaderClient.Do(req)
					if err != nil {
						continue
					}
					res.Body.Close()
					if res.StatusCode == http.StatusOK {
						if checkAfter {
							bad.Add(1)
						} else {
							okBefore.Add(1)
						}
					}
				}
			}()
		}
		time.Sleep(30 * time.Millisecond)
		start := time.Now()
		if status, _ := env.do("POST", "/api/auth/logout", token, nil); status != http.StatusNoContent {
			t.Fatalf("logout: %d", status)
		}
		assertNoWait(t, "logout", time.Since(start))
		after.Store(true)
		time.Sleep(300 * time.Millisecond)
		close(stop)
		wg.Wait()
		if okBefore.Load() == 0 {
			t.Fatalf("iteration %d: no request succeeded before the logout; the race was not exercised", iteration)
		}
		if n := bad.Load(); n > 0 {
			t.Fatalf("iteration %d: %d requests succeeded after the logout responded", iteration, n)
		}
	}
}
