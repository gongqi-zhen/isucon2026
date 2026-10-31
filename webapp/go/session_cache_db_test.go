package isureal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// #38 のセッションキャッシュを、実際の PostgreSQL と HTTP ハンドラで確かめる。
// europe のアプリとして起動し、ほかのプロセス（asia1 など）での無効化は SQL で再現する。
// ISUREAL_TEST_DSN に、001-schema.sql と migrate.sql を流した DB を指定したときだけ実行する（データは追加される）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55438/isureal?sslmode=disable' go test -run TestSessionCacheDB -v ./...

var (
	sessionTestServerOnce sync.Once
	sessionTestServer     *httptest.Server
)

type sessionTestEnv struct {
	t      *testing.T
	db     *sqlx.DB
	server *httptest.Server
	seq    atomic.Int64
}

func newSessionTestEnv(t *testing.T) *sessionTestEnv {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	config := &Config{
		Region: "europe", DBHost: u.Hostname(), DBPort: u.Port(), DBUser: u.User.Username(), DBPassword: password,
		DBName: strings.TrimPrefix(u.Path, "/"), DBSSLMode: "disable", LogLevel: "error", PprofAddr: "off",
		ListenAddr: ":0", DBMaxConns: 32,
	}
	sessionTestServerOnce.Do(func() { sessionTestServer = httptest.NewServer(NewServer(config)) })
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &sessionTestEnv{t: t, db: db, server: sessionTestServer}
}

func (env *sessionTestEnv) do(method, path, token string, body any) (int, map[string]any) {
	env.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, env.server.URL+path, reader)
	if err != nil {
		env.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-IP", "10.38.0.1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

// newUser は europe の利用者を登録してログインし、トークンをキャッシュに入れた状態で返す。
func (env *sessionTestEnv) newUser() (userID, token string) {
	env.t.Helper()
	name := fmt.Sprintf("sc%d_%d", time.Now().UnixNano()%1_000_000_000, env.seq.Add(1))
	status, body := env.do("POST", "/api/auth/register", "", map[string]string{"username": name, "displayName": name, "region": "europe"})
	if status != http.StatusCreated {
		env.t.Fatalf("register: %d %v", status, body)
	}
	secret := body["secretKey"].(string)
	status, body = env.do("POST", "/api/auth/login", "", map[string]string{"username": name, "secretKey": secret})
	if status != http.StatusOK {
		env.t.Fatalf("login: %d %v", status, body)
	}
	token = body["token"].(string)
	userID = body["user"].(map[string]any)["id"].(string)
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusOK {
		env.t.Fatalf("me: %d", status)
	}
	return userID, token
}

func (env *sessionTestEnv) exec(query string, args ...any) {
	env.t.Helper()
	if _, err := env.db.Exec(query, args...); err != nil {
		env.t.Fatal(err)
	}
}

// ほかのプロセスでの無効化（SQL で再現）の直後は、キャッシュから応答する（キャッシュが効いている）。
// 書く側が待つ TTL を過ぎた後は、必ず無効になる。
func TestSessionCacheDBInvalidationsFromOtherProcesses(t *testing.T) {
	env := newSessionTestEnv(t)
	cases := []struct {
		name       string
		invalidate func(userID string)
		want       int
	}{
		{"logout", func(userID string) {
			env.exec(`UPDATE login_sessions SET active=false, invalidated_at=clock_timestamp(), reason='logout' WHERE user_id=$1 AND active`, userID)
		}, http.StatusUnauthorized},
		{"login replaces session", func(userID string) {
			env.exec(`WITH invalidated AS (UPDATE login_sessions SET active=false, invalidated_at=clock_timestamp(), reason='replaced' WHERE user_id=$1 AND active)
			  INSERT INTO login_sessions (id, user_id, token_hash) VALUES (gen_random_uuid()::text, $1, md5(random()::text))`, userID)
		}, http.StatusUnauthorized},
		{"account deletion", func(userID string) {
			env.exec(`UPDATE users SET status='deleted', deleted_at=clock_timestamp(), updated_at=clock_timestamp() WHERE id=$1`, userID)
		}, http.StatusUnauthorized},
		{"region change to asia", func(userID string) {
			env.exec(`UPDATE users SET region='asia', updated_at=clock_timestamp() WHERE id=$1`, userID)
		}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID, token := env.newUser()
			tc.invalidate(userID)
			if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusOK {
				t.Logf("immediately after the other process's commit: %d (cache was already refreshed)", status)
			}
			time.Sleep(sessionCacheTTL)
			for i := 0; i < 20; i++ {
				if status, _ := env.do("GET", "/api/me", token, nil); status != tc.want {
					t.Fatalf("request %d after the writer's wait: status %d, want %d", i, status, tc.want)
				}
			}
		})
	}
}

// 書く側の待ちの間も読み取りが並行して走り、古い結果を入れようとする。待ち終わった後は1回も通らないこと。
func TestSessionCacheDBConcurrentReadersDoNotResurrect(t *testing.T) {
	env := newSessionTestEnv(t)
	for iteration := 0; iteration < 10; iteration++ {
		userID, token := env.newUser()
		stop := make(chan struct{})
		var after atomic.Bool
		var bad atomic.Int64
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
					req.Header.Set("X-Client-IP", "10.38.0.2")
					res, err := http.DefaultClient.Do(req)
					if err != nil {
						continue
					}
					res.Body.Close()
					if checkAfter && res.StatusCode == http.StatusOK {
						bad.Add(1)
					}
				}
			}()
		}
		time.Sleep(30 * time.Millisecond)
		env.exec(`UPDATE login_sessions SET active=false, invalidated_at=clock_timestamp(), reason='logout' WHERE user_id=$1 AND active`, userID)
		time.Sleep(sessionCacheTTL) // 書く側の待ち
		after.Store(true)
		time.Sleep(300 * time.Millisecond)
		close(stop)
		wg.Wait()
		if n := bad.Load(); n > 0 {
			t.Fatalf("iteration %d: %d requests succeeded after the writer responded", iteration, n)
		}
	}
}

// 同じプロセスでのログアウトは、待ってから応答し、その後は古いトークンが使えない。
func TestSessionCacheDBLogoutHandlerWaitsAndInvalidates(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	start := time.Now()
	if status, _ := env.do("POST", "/api/auth/logout", token, nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	if elapsed := time.Since(start); elapsed < sessionCacheTTL {
		t.Fatalf("logout returned after %v, want at least %v for a europe user", elapsed, sessionCacheTTL)
	}
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", status)
	}
}

// リージョン変更（europe から asia）は待ってから応答し、その後 europe のアプリでは 403 になる。
func TestSessionCacheDBRegionChangeHandler(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	start := time.Now()
	if status, body := env.do("PATCH", "/api/me", token, map[string]string{"region": "asia"}); status != http.StatusOK {
		t.Fatalf("patch me: %d %v", status, body)
	}
	if elapsed := time.Since(start); elapsed < sessionCacheTTL {
		t.Fatalf("region change returned after %v, want at least %v", elapsed, sessionCacheTTL)
	}
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusForbidden {
		t.Fatalf("me after moving to asia: %d, want 403", status)
	}
	// 表示名だけの変更は待たない。
	_, token2 := env.newUser()
	start = time.Now()
	if status, _ := env.do("PATCH", "/api/me", token2, map[string]string{"displayName": "renamed"}); status != http.StatusOK {
		t.Fatalf("rename: %d", status)
	}
	if elapsed := time.Since(start); elapsed >= sessionCacheTTL {
		t.Fatalf("display name change waited %v", elapsed)
	}
}

// 初期化と同じく login_sessions を空にすると、TTL 後には古いトークンが使えない。
func TestSessionCacheDBTruncate(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	env.exec(`DELETE FROM login_sessions`)
	time.Sleep(sessionCacheTTL)
	if status, _ := env.do("GET", "/api/me", token, nil); status != http.StatusUnauthorized {
		t.Fatalf("me after sessions were cleared: %d", status)
	}
}

// キャッシュが効いていることを、DB の往復数で確かめる（europe では1回あたり約100ms 減る）。
func TestSessionCacheDBHitAvoidsQuery(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	// キャッシュはプロセスに1つで、最初に使ったサーバのリージョンで決まる。
	// ほかの DB テスト（asia で起動する）が先に走ったときは、キャッシュしない設定になっている。
	if sessions.region != "europe" {
		t.Skipf("session cache was initialized for %q by another test server", sessions.region)
	}
	if _, ok := sessions.get(tokenHash); !ok {
		t.Fatal("token should be cached after a successful request")
	}
}
