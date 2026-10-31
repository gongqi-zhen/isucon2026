package isureal

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// #62 の待ちの省略を、実際の PostgreSQL と HTTP ハンドラで確かめる。
// europe のアプリとして起動したテストサーバ（session_cache_db_test.go）で、ISUREAL_SESSION_CACHE_SOLE=1 の状態にする。
// ISUREAL_TEST_DSN を指定したときだけ実行する。

// enableSessionCacheSole は、このテストの間だけ、テストサーバのプロセスを europe の唯一のキャッシュとして扱う。
func enableSessionCacheSole(t *testing.T, env *sessionTestEnv) {
	t.Helper()
	// キャッシュはセッションの確認で初めて初期化されるので、先に1回確認させる（トークンは無効でよい）。
	env.do("GET", "/api/me", "not-a-token", nil)
	sessions.mu.Lock()
	region, enabled, prev := sessions.region, sessions.enabled, sessions.sole
	sessions.sole = true
	sessions.mu.Unlock()
	t.Cleanup(func() {
		sessions.mu.Lock()
		sessions.sole = prev
		sessions.mu.Unlock()
	})
	if region != "europe" || !enabled {
		t.Skipf("session cache was initialized for %q (enabled=%v) by another test server", region, enabled)
	}
}

// registerSoleTestUser は、指定したリージョンの利用者を登録して、ユーザー名と SecretKey を返す。
func registerSoleTestUser(env *sessionTestEnv, region string) (username, secretKey string) {
	env.t.Helper()
	username = fmt.Sprintf("so%d_%d", time.Now().UnixNano()%1_000_000_000, env.seq.Add(1))
	status, body := env.do("POST", "/api/auth/register", "", map[string]string{"username": username, "displayName": username, "region": region})
	if status != http.StatusCreated {
		env.t.Fatalf("register: %d %v", status, body)
	}
	return username, body["secretKey"].(string)
}

func loginSoleTestUser(env *sessionTestEnv, username, secretKey string) (token string, elapsed time.Duration) {
	env.t.Helper()
	start := time.Now()
	status, body := env.do("POST", "/api/auth/login", "", map[string]string{"username": username, "secretKey": secretKey})
	elapsed = time.Since(start)
	if status != http.StatusOK {
		env.t.Fatalf("login: %d %v", status, body)
	}
	return body["token"].(string), elapsed
}

// 待たずに応答したかどうか。ローカルの DB なので、待たなければ TTL の半分より十分に短い。
func assertNoWait(t *testing.T, what string, elapsed time.Duration) {
	t.Helper()
	if elapsed >= sessionCacheTTL/2 {
		t.Fatalf("%s took %v, want no wait (TTL %v)", what, elapsed, sessionCacheTTL)
	}
}

func TestSessionCacheSoleDBLogoutSkipsWaitAndInvalidates(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	enableSessionCacheSole(t, env)
	start := time.Now()
	if status, _ := env.do("POST", "/api/auth/logout", token, nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	assertNoWait(t, "logout", time.Since(start))
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("me right after logout: %d, want 401", status)
	}
}

func TestSessionCacheSoleDBReloginInvalidatesOldTokenImmediately(t *testing.T) {
	env := newSessionTestEnv(t)
	username, secretKey := registerSoleTestUser(env, "europe")
	oldToken, _ := loginSoleTestUser(env, username, secretKey)
	if status, _ := env.do("GET", "/api/me", oldToken, nil); status != http.StatusOK {
		t.Fatalf("me: %d", status)
	}
	enableSessionCacheSole(t, env)
	newToken, elapsed := loginSoleTestUser(env, username, secretKey)
	assertNoWait(t, "login", elapsed)
	if status, _ := env.do("GET", "/api/me", oldToken, nil); status != http.StatusUnauthorized {
		t.Fatalf("old token right after re-login: %d, want 401", status)
	}
	if status, _ := env.do("GET", "/api/me", newToken, nil); status != http.StatusOK {
		t.Fatalf("new token: %d, want 200", status)
	}
}

func TestSessionCacheSoleDBAccountDeletionSkipsWait(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	enableSessionCacheSole(t, env)
	start := time.Now()
	if status, _ := env.do("POST", "/api/me/account-deletion", token, nil); status != http.StatusNoContent {
		t.Fatalf("account deletion: %d", status)
	}
	assertNoWait(t, "account deletion", time.Since(start))
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("me right after account deletion: %d, want 401", status)
	}
}

func TestSessionCacheSoleDBRegionChangeSkipsWait(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	enableSessionCacheSole(t, env)
	start := time.Now()
	if status, body := env.do("PATCH", "/api/me", token, map[string]string{"region": "asia"}); status != http.StatusOK {
		t.Fatalf("patch me: %d %v", status, body)
	}
	assertNoWait(t, "region change from europe on the europe process", time.Since(start))
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusForbidden {
		t.Fatalf("me right after moving to asia: %d, want 403", status)
	}
}

// ほかのリージョンの利用者（america1 がキャッシュしうる）を europe のプロセスで無効にしたときは、今まで通り待つ。
func TestSessionCacheSoleDBOtherRegionUserStillWaits(t *testing.T) {
	env := newSessionTestEnv(t)
	username, secretKey := registerSoleTestUser(env, "america")
	loginSoleTestUser(env, username, secretKey)
	enableSessionCacheSole(t, env)
	if _, elapsed := loginSoleTestUser(env, username, secretKey); elapsed < sessionCacheTTL {
		t.Fatalf("re-login of an america user on the europe process took %v, want at least %v", elapsed, sessionCacheTTL)
	}
}

// ログアウトの間も読み取りが並行して走り、コミットより前の結果を入れようとする。
// 待たずに応答した直後から、1回も通らないこと。
func TestSessionCacheSoleDBConcurrentReadersDoNotResurrect(t *testing.T) {
	env := newSessionTestEnv(t)
	enableSessionCacheSole(t, env)
	for iteration := 0; iteration < 20; iteration++ {
		_, token := env.newUser()
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
					req.Header.Set("X-Client-IP", "10.62.0.1")
					res, err := sessionTestClient.Do(req)
					if err != nil {
						continue
					}
					drainAndClose(res)
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
