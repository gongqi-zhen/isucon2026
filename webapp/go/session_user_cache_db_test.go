package isureal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// #58：europe のアプリの GET /api/me は、セッションと一緒にキャッシュした利用者の行を使い、DB を引かない。
// 利用者の行を変える操作が応答した後に届いた GET /api/me には、変更が必ず反映されることを、
// 実際の PostgreSQL と HTTP ハンドラで確かめる（newSessionTestEnv と同じく ISUREAL_TEST_DSN を指定したときだけ実行する）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55438/isureal?sslmode=disable' go test -run TestSessionUserCacheDB -v ./...

// sessionReaderClient は、並行して読み続けるテストで接続を使い回す（既定の Transport はアイドルの接続を2本しか残さず、
// macOS では一時ポートが尽きる）。
var sessionReaderClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 32}}

func (env *sessionTestEnv) raw(method, path, token, contentType string, body []byte) int {
	env.t.Helper()
	req, err := http.NewRequest(method, env.server.URL+path, bytes.NewReader(body))
	if err != nil {
		env.t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Client-IP", "10.58.0.1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func sessionTestJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (env *sessionTestEnv) adminToken() string {
	env.t.Helper()
	hash := sha256.Sum256([]byte("password"))
	env.exec(`INSERT INTO administrators (id, username, password_hash) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		"session-user-cache-admin", "sucadmin", hex.EncodeToString(hash[:]))
	status, body := env.do("POST", "/api/admin/auth/login", "", map[string]string{"username": "sucadmin", "password": "password"})
	if status != http.StatusOK {
		env.t.Fatalf("admin login: %d %v", status, body)
	}
	return body["token"].(string)
}

// readMeConcurrently は、write の間と後に GET /api/me を並行して送り続け、
// write が応答した後に送ったリクエストのうち、want を満たさなかった応答の数を返す。
func (env *sessionTestEnv) readMeConcurrently(token string, write func(), want func(me map[string]any) bool) (bad, checked int64) {
	env.t.Helper()
	var writerDone atomic.Int64 // write が応答した時刻（UnixNano）。0 は応答前。
	var badCount, checkedCount atomic.Int64
	stop := make(chan struct{})
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
				sentAt := time.Now().UnixNano()
				req, _ := http.NewRequest("GET", env.server.URL+"/api/me", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Client-IP", "10.58.0.2")
				res, err := sessionReaderClient.Do(req)
				if err != nil {
					continue
				}
				var me map[string]any
				_ = json.NewDecoder(res.Body).Decode(&me)
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if done := writerDone.Load(); done != 0 && sentAt > done {
					checkedCount.Add(1)
					if res.StatusCode != http.StatusOK || !want(me) {
						badCount.Add(1)
					}
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // 読み取りが古い行をキャッシュに入れている状態にする
	write()
	writerDone.Store(time.Now().UnixNano())
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	return badCount.Load(), checkedCount.Load()
}

// このプロセスの HTTP ハンドラでの変更。どれも、応答した後の GET /api/me に変更が反映される。
func TestSessionUserCacheDBWritersAreReflected(t *testing.T) {
	env := newSessionTestEnv(t)
	jpegData := sessionTestJPEG(t)
	admin := env.adminToken()
	cases := []struct {
		name  string
		setup func(t *testing.T, token string)
		write func(t *testing.T, userID, token string)
		want  func(t *testing.T, userID string) func(me map[string]any) bool
	}{
		{
			name: "display name",
			write: func(t *testing.T, _, token string) {
				if status, body := env.do("PATCH", "/api/me", token, map[string]string{"displayName": "renamed"}); status != http.StatusOK {
					t.Fatalf("patch: %d %v", status, body)
				}
			},
			want: func(*testing.T, string) func(map[string]any) bool {
				return func(me map[string]any) bool { return me["displayName"] == "renamed" }
			},
		},
		{
			name: "bio",
			write: func(t *testing.T, _, token string) {
				if status, body := env.do("PATCH", "/api/me", token, map[string]string{"bio": "new bio"}); status != http.StatusOK {
					t.Fatalf("patch: %d %v", status, body)
				}
			},
			want: func(*testing.T, string) func(map[string]any) bool {
				return func(me map[string]any) bool { return me["bio"] == "new bio" }
			},
		},
		{
			name: "put profile image",
			write: func(t *testing.T, _, token string) {
				if status := env.raw("PUT", "/api/me/profile-image", token, "image/jpeg", jpegData); status != http.StatusNoContent {
					t.Fatalf("put profile image: %d", status)
				}
			},
			want: func(t *testing.T, userID string) func(map[string]any) bool {
				var imageID string
				if err := env.db.Get(&imageID, `SELECT profile_image_id FROM users WHERE id = $1`, userID); err != nil {
					t.Fatal(err)
				}
				return func(me map[string]any) bool {
					url, ok := me["profileImageUrl"].(string)
					return ok && strings.Contains(url, imageID)
				}
			},
		},
		{
			name: "delete profile image",
			setup: func(t *testing.T, token string) {
				if status := env.raw("PUT", "/api/me/profile-image", token, "image/jpeg", jpegData); status != http.StatusNoContent {
					t.Fatalf("put profile image: %d", status)
				}
			},
			write: func(t *testing.T, _, token string) {
				if status := env.raw("DELETE", "/api/me/profile-image", token, "application/json", nil); status != http.StatusNoContent {
					t.Fatalf("delete profile image: %d", status)
				}
			},
			want: func(*testing.T, string) func(map[string]any) bool {
				return func(me map[string]any) bool { v, ok := me["profileImageUrl"]; return ok && v == nil }
			},
		},
		{
			name: "certification by admin",
			write: func(t *testing.T, userID, _ string) {
				if status := env.raw("PUT", "/api/admin/users/"+userID+"/certification", admin, "application/json", nil); status != http.StatusNoContent {
					t.Fatalf("certify: %d", status)
				}
			},
			want: func(*testing.T, string) func(map[string]any) bool {
				return func(me map[string]any) bool { return me["accountType"] == "certified_public" }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for iteration := 0; iteration < 3; iteration++ {
				userID, token := env.newUser()
				if tc.setup != nil {
					tc.setup(t, token)
				}
				var want func(map[string]any) bool
				bad, checked := env.readMeConcurrently(token, func() {
					start := time.Now()
					tc.write(t, userID, token)
					if elapsed := time.Since(start); sessions.caching() && elapsed < sessionCacheTTL {
						t.Errorf("the writer responded after %v, want at least %v for a europe user", elapsed, sessionCacheTTL)
					}
					want = tc.want(t, userID)
				}, func(me map[string]any) bool { return want(me) })
				if checked == 0 {
					t.Fatal("no request was sent after the writer responded")
				}
				if bad > 0 {
					t.Fatalf("iteration %d: %d of %d requests after the writer responded did not see the change", iteration, bad, checked)
				}
			}
		})
	}
}

// ほかのプロセス（asia1 のプロフィール画像の API や、別のサーバに届いた admin のリクエスト）での変更を SQL で再現する。
// 書く側と同じく、コミットしてから TTL 待った後は、必ず変更が見える。
func TestSessionUserCacheDBChangesFromOtherProcesses(t *testing.T) {
	env := newSessionTestEnv(t)
	otherProcessImageID := uuid.NewString()
	cases := []struct {
		name  string
		write func(userID string)
		want  func(me map[string]any) bool
	}{
		{"display name", func(userID string) {
			env.exec(`UPDATE users SET display_name = 'elsewhere', updated_at = clock_timestamp() WHERE id = $1`, userID)
		}, func(me map[string]any) bool { return me["displayName"] == "elsewhere" }},
		{"account type", func(userID string) {
			env.exec(`UPDATE users SET account_type = 'certified_public', updated_at = clock_timestamp() WHERE id = $1`, userID)
		}, func(me map[string]any) bool { return me["accountType"] == "certified_public" }},
		{"profile image", func(userID string) {
			env.exec(`INSERT INTO image_objects (id, owner_user_id, data, media_type) VALUES ($1, $2, '\xffd8'::bytea, 'image/jpeg')`, otherProcessImageID, userID)
			env.exec(`UPDATE users SET profile_image_id = $1, updated_at = clock_timestamp() WHERE id = $2`, otherProcessImageID, userID)
		}, func(me map[string]any) bool {
			url, ok := me["profileImageUrl"].(string)
			return ok && strings.Contains(url, otherProcessImageID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID, token := env.newUser()
			bad, checked := env.readMeConcurrently(token, func() {
				tc.write(userID)
				time.Sleep(sessionCacheTTL) // 書く側の待ち
			}, tc.want)
			if checked == 0 {
				t.Fatal("no request was sent after the writer's wait")
			}
			if bad > 0 {
				t.Fatalf("%d of %d requests after the writer's wait did not see the change", bad, checked)
			}
		})
	}
}

// GET /api/me は、キャッシュした利用者の行から応答する（europe では1回あたり約100ms 減る）。
func TestSessionUserCacheDBHitHasUserRow(t *testing.T) {
	env := newSessionTestEnv(t)
	_, token := env.newUser()
	if sessions.region != "europe" {
		t.Skipf("session cache was initialized for %q by another test server", sessions.region)
	}
	sum := sha256.Sum256([]byte(token))
	cached, ok := sessions.get(hex.EncodeToString(sum[:]))
	if !ok || cached.User == nil {
		t.Fatalf("the user row should be cached with the session: %+v, %v", cached, ok)
	}
	// 確かめ直しの後も、利用者の行を持ち続ける（変わっていない行は返されない）。
	time.Sleep(3 * sessionCacheInterval)
	if cached, ok := sessions.get(hex.EncodeToString(sum[:])); !ok || cached.User == nil {
		t.Fatalf("the user row should survive revalidation: %+v, %v", cached, ok)
	}
	status, me := env.do("GET", "/api/me", token, nil)
	if status != http.StatusOK || me["displayName"] != cached.User.DisplayName || me["region"] != "europe" {
		t.Fatalf("me from the cache: %d %v", status, me)
	}
}
