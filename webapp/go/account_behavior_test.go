package isureal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// #67 の、登録・ログイン・プロフィールの変更・プロフィール画像・退会の挙動テスト。
// 1文や DB の関数1回の呼び出しに書き換える前の main でも、書き換えた後でも同じ結果になることを確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql を流した DB を指定したときだけ実行する。

func acctName() string { return "ac" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20] }

func (env *lockTestEnv) acctRegister(name, displayName, region string) (int, map[string]any) {
	return env.do("POST", "/api/auth/register", "", map[string]string{"username": name, "displayName": displayName, "region": region}, nil)
}

func (env *lockTestEnv) acctLogin(name, secret string) (int, map[string]any) {
	return env.do("POST", "/api/auth/login", "", map[string]string{"username": name, "secretKey": secret}, nil)
}

// acctNewUser は登録してログインし、ユーザー名、SecretKey、ログインした利用者を返す。
func (env *lockTestEnv) acctNewUser(region string) (string, string, lockTestUser) {
	env.t.Helper()
	name := acctName()
	status, registered := env.acctRegister(name, name, region)
	if status != http.StatusCreated {
		env.t.Fatalf("register: %d %v", status, registered)
	}
	secret := registered["secretKey"].(string)
	status, login := env.acctLogin(name, secret)
	if status != http.StatusOK {
		env.t.Fatalf("login: %d %v", status, login)
	}
	return name, secret, lockTestUser{id: login["user"].(map[string]any)["id"].(string), token: login["token"].(string)}
}

// acctRaw は本文をそのまま送り、ステータスと応答の本文を返す。
func (env *lockTestEnv) acctRaw(method, path, token, contentType string, body []byte) (int, []byte) {
	env.t.Helper()
	req, err := http.NewRequest(method, env.server.URL+path, bytes.NewReader(body))
	if err != nil {
		env.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Client-IP", fmt.Sprintf("10.67.%d.%d", time.Now().UnixNano()%250, time.Now().UnixNano()/7%250))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		env.t.Fatal(err)
	}
	return res.StatusCode, data
}

func (env *lockTestEnv) acctMe(token string) (int, map[string]any) {
	return env.do("GET", "/api/me", token, nil, nil)
}

func (env *lockTestEnv) acctActiveSessions(userID string) int {
	env.t.Helper()
	var n int
	if err := env.db.Get(&n, `SELECT count(*) FROM login_sessions WHERE user_id = $1 AND active = true`, userID); err != nil {
		env.t.Fatal(err)
	}
	return n
}

func (env *lockTestEnv) acctImageCount(userID string) int {
	env.t.Helper()
	var n int
	if err := env.db.Get(&n, `SELECT count(*) FROM image_objects WHERE owner_user_id = $1`, userID); err != nil {
		env.t.Fatal(err)
	}
	return n
}

// expectUserJSON は、応答の利用者の項目が want と完全に一致すること（項目の過不足もないこと）を確かめる。
func expectUserJSON(t *testing.T, name string, got any, want map[string]any) {
	t.Helper()
	user, ok := got.(map[string]any)
	if !ok {
		t.Errorf("%s: user = %v", name, got)
		return
	}
	if len(user) != len(want) {
		t.Errorf("%s: user keys = %v, want %v", name, user, want)
	}
	for key, value := range want {
		if fmt.Sprint(user[key]) != fmt.Sprint(value) {
			t.Errorf("%s: user[%s] = %v, want %v", name, key, user[key], value)
		}
	}
}

func sessionUserJSON(id, name, displayName, bio, region string) map[string]any {
	return map[string]any{
		"id": id, "username": name, "displayName": displayName, "bio": bio,
		"region": region, "accountType": "regular", "status": "active",
	}
}

func TestAccountRegisterBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	name := acctName()
	status, body := env.acctRegister(name, "Ｔｅｓｔ 太郎", "europe")
	expectInteraction(t, "register", status, body, http.StatusCreated, "")
	if status != http.StatusCreated {
		return
	}
	if len(body) != 2 {
		t.Errorf("register body keys: %v", body)
	}
	secret, _ := body["secretKey"].(string)
	if _, err := uuid.Parse(secret); err != nil {
		t.Errorf("secretKey = %q", secret)
	}
	user := body["user"].(map[string]any)
	id, _ := user["id"].(string)
	expectUserJSON(t, "register", user, sessionUserJSON(id, name, "Ｔｅｓｔ 太郎", "", "europe"))

	var row struct {
		ID            string  `db:"id"`
		SecretKeyHash string  `db:"secret_key_hash"`
		SearchText    *string `db:"search_text"`
		AccountType   string  `db:"account_type"`
		Status        string  `db:"status"`
		Profile       *string `db:"profile_image_id"`
	}
	if err := env.db.Get(&row, `SELECT id, secret_key_hash, search_text, account_type, status, profile_image_id FROM users WHERE username = $1`, name); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(secret))
	if row.ID != id || row.SecretKeyHash != hex.EncodeToString(hash[:]) || row.AccountType != "regular" || row.Status != "active" || row.Profile != nil {
		t.Errorf("users row = %+v", row)
	}
	if row.SearchText == nil || *row.SearchText != userSearchText(name, "Ｔｅｓｔ 太郎") {
		t.Errorf("search_text = %v, want %q", row.SearchText, userSearchText(name, "Ｔｅｓｔ 太郎"))
	}

	// 同じユーザー名は 409。
	status, body = env.acctRegister(name, "other", "asia")
	expectInteraction(t, "duplicate", status, body, http.StatusConflict, "username_taken")

	// 入力の検証。
	for _, input := range []map[string]string{
		{"username": "Upper" + name[:10], "displayName": "x", "region": "asia"},
		{"username": acctName(), "displayName": "", "region": "asia"},
		{"username": acctName(), "displayName": strings.Repeat("あ", 101), "region": "asia"},
		{"username": acctName(), "displayName": "x", "region": "mars"},
		{"username": "", "displayName": "x", "region": "asia"},
	} {
		status, body = env.do("POST", "/api/auth/register", "", input, nil)
		expectInteraction(t, fmt.Sprintf("invalid %v", input), status, body, http.StatusBadRequest, "invalid_registration")
	}
}

func TestAccountLoginBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	name := acctName()
	_, registered := env.acctRegister(name, "Login User", "america")
	secret := registered["secretKey"].(string)
	id := registered["user"].(map[string]any)["id"].(string)

	status, body := env.acctLogin(name, secret)
	expectInteraction(t, "login", status, body, http.StatusOK, "")
	if len(body) != 2 {
		t.Errorf("login body keys: %v", body)
	}
	expectUserJSON(t, "login", body["user"], sessionUserJSON(id, name, "Login User", "", "america"))
	first, _ := body["token"].(string)
	if _, err := uuid.Parse(first); err != nil {
		t.Errorf("token = %q", first)
	}
	if env.acctActiveSessions(id) != 1 {
		t.Errorf("active sessions after login = %d", env.acctActiveSessions(id))
	}

	// 再ログインで、前のセッションは reason = 'replaced' で無効になる。
	status, body = env.acctLogin(name, secret)
	expectInteraction(t, "relogin", status, body, http.StatusOK, "")
	second, _ := body["token"].(string)
	if second == first {
		t.Errorf("relogin returned the same token")
	}
	if s, _ := env.acctMe(first); s != http.StatusUnauthorized {
		t.Errorf("old token after relogin: %d", s)
	}
	var reasons []string
	env.db.Select(&reasons, `SELECT coalesce(reason, '') FROM login_sessions WHERE user_id = $1 ORDER BY created_at`, id)
	if len(reasons) != 2 || reasons[0] != "replaced" || reasons[1] != "" {
		t.Errorf("session reasons = %v", reasons)
	}
	// america の利用者なので、asia のアプリでは region_mismatch になる（セッションは有効）。
	if s, b := env.acctMe(second); s != http.StatusForbidden || b["error"] != "region_mismatch" {
		t.Errorf("me with new token: %d %v", s, b)
	}

	// 失敗はすべて 401 invalid_credentials で、セッションを作らない。
	for label, input := range map[string][2]string{
		"wrong secret":     {name, uuid.NewString()},
		"empty secret":     {name, ""},
		"unknown username": {acctName(), secret},
		"invalid username": {"Bad Name", secret},
		"empty username":   {"", secret},
	} {
		status, body = env.acctLogin(input[0], input[1])
		expectInteraction(t, label, status, body, http.StatusUnauthorized, "invalid_credentials")
	}
	if env.acctActiveSessions(id) != 1 {
		t.Errorf("active sessions after failed logins = %d", env.acctActiveSessions(id))
	}
}

// 初期データの利用者（SQL で入れた行。search_text は NULL のこともある）でもログインできる。
func TestAccountLoginSeededUser(t *testing.T) {
	env := newLockTestEnv(t)
	name := acctName()
	secret := uuid.NewString()
	hash := sha256.Sum256([]byte(secret))
	id := uuid.NewString()
	env.db.MustExec(`INSERT INTO users (id, username, secret_key_hash, display_name, bio, region, account_type, created_at, updated_at)
		VALUES ($1, $2, $3, '小林 悠真 1', 'bio text', 'asia', 'certified_public', '2025-01-01', '2025-01-01')`, id, name, hex.EncodeToString(hash[:]))
	status, body := env.acctLogin(name, secret)
	expectInteraction(t, "seeded login", status, body, http.StatusOK, "")
	expectUserJSON(t, "seeded login", body["user"], map[string]any{
		"id": id, "username": name, "displayName": "小林 悠真 1", "bio": "bio text",
		"region": "asia", "accountType": "certified_public", "status": "active",
	})
	if s, me := env.acctMe(body["token"].(string)); s != http.StatusOK || me["id"] != id {
		t.Errorf("me after seeded login: %d %v", s, me)
	}
}

// 同じ利用者の同時のログインは、どれも成功し、有効なセッションは1つだけ残る。
func TestAccountConcurrentLogins(t *testing.T) {
	env := newLockTestEnv(t)
	name, secret, u := env.acctNewUser("asia")
	tokens := make([]string, 12)
	statuses := make([]int, len(tokens))
	parallel(len(tokens), func(i int) {
		var body map[string]any
		statuses[i], body = env.acctLogin(name, secret)
		tokens[i], _ = body["token"].(string)
	})
	valid := 0
	for i, token := range tokens {
		if statuses[i] != http.StatusOK {
			t.Fatalf("login %d: %d", i, statuses[i])
		}
		if s, _ := env.acctMe(token); s == http.StatusOK {
			valid++
		}
	}
	if valid != 1 || env.acctActiveSessions(u.id) != 1 {
		t.Errorf("valid tokens = %d, active sessions = %d", valid, env.acctActiveSessions(u.id))
	}
}

func TestAccountUpdateMeBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	name, _, u := env.acctNewUser("asia")
	var before time.Time
	env.db.Get(&before, `SELECT updated_at FROM users WHERE id = $1`, u.id)

	status, body := env.do("PATCH", "/api/me", u.token, map[string]any{}, nil)
	expectInteraction(t, "empty patch", status, body, http.StatusBadRequest, "invalid_profile")
	for label, input := range map[string]map[string]any{
		"empty displayName": {"displayName": ""},
		"long displayName":  {"displayName": strings.Repeat("a", 101)},
		"long bio":          {"bio": strings.Repeat("b", 501)},
		"invalid region":    {"region": "mars"},
		"valid and invalid": {"displayName": "ok", "region": "mars"},
	} {
		status, body = env.do("PATCH", "/api/me", u.token, input, nil)
		expectInteraction(t, label, status, body, http.StatusBadRequest, "invalid_profile")
	}

	// 表示名だけ。検索用の正規化済みテキストも変わる。
	status, body = env.do("PATCH", "/api/me", u.token, map[string]any{"displayName": "New NAME"}, nil)
	expectInteraction(t, "displayName", status, body, http.StatusOK, "")
	want := sessionUserJSON(u.id, name, "New NAME", "", "asia")
	want["profileImageUrl"] = nil
	expectUserJSON(t, "displayName", body, want)
	var row struct {
		DisplayName string    `db:"display_name"`
		Bio         string    `db:"bio"`
		Region      string    `db:"region"`
		SearchText  *string   `db:"search_text"`
		UpdatedAt   time.Time `db:"updated_at"`
	}
	env.db.Get(&row, `SELECT display_name, bio, region, search_text, updated_at FROM users WHERE id = $1`, u.id)
	if row.SearchText == nil || *row.SearchText != name+"\nnew name" || !row.UpdatedAt.After(before) {
		t.Errorf("after displayName: %+v", row)
	}
	searchText := *row.SearchText

	// 自己紹介だけ。表示名と検索用のテキストは変わらない。
	status, body = env.do("PATCH", "/api/me", u.token, map[string]any{"bio": "hello\nworld"}, nil)
	expectInteraction(t, "bio", status, body, http.StatusOK, "")
	want["bio"] = "hello\nworld"
	expectUserJSON(t, "bio", body, want)
	env.db.Get(&row, `SELECT display_name, bio, region, search_text, updated_at FROM users WHERE id = $1`, u.id)
	if row.DisplayName != "New NAME" || row.SearchText == nil || *row.SearchText != searchText {
		t.Errorf("after bio: %+v", row)
	}

	// 自己紹介を空にもできる。
	status, body = env.do("PATCH", "/api/me", u.token, map[string]any{"bio": ""}, nil)
	expectInteraction(t, "empty bio", status, body, http.StatusOK, "")
	want["bio"] = ""
	expectUserJSON(t, "empty bio", body, want)

	// 3つ同時。リージョンを変えると、asia のアプリでは region_mismatch になる。PATCH /api/me は RegionMiddleware を通らない。
	status, body = env.do("PATCH", "/api/me", u.token, map[string]any{"displayName": "Euro", "bio": "moved", "region": "europe"}, nil)
	expectInteraction(t, "all fields", status, body, http.StatusOK, "")
	want = sessionUserJSON(u.id, name, "Euro", "moved", "europe")
	want["profileImageUrl"] = nil
	expectUserJSON(t, "all fields", body, want)
	if s, b := env.acctMe(u.token); s != http.StatusForbidden || b["error"] != "region_mismatch" {
		t.Errorf("me after region change: %d %v", s, b)
	}
	status, body = env.do("PATCH", "/api/me", u.token, map[string]any{"region": "asia"}, nil)
	expectInteraction(t, "region back", status, body, http.StatusOK, "")
	if s, me := env.acctMe(u.token); s != http.StatusOK || me["region"] != "asia" || me["displayName"] != "Euro" {
		t.Errorf("me after region back: %d %v", s, me)
	}

	// ユーザー名が正規化済みの形でなければ、search_text は NULL にする（検索時に Go で判定する）。
	odd := acctName()
	oddID := uuid.NewString()
	env.db.MustExec(`INSERT INTO users (id, username, secret_key_hash, display_name, region, search_text) VALUES ($1, $2, $3, 'x', 'asia', 'x')`,
		oddID, "Odd"+odd, strings.Repeat("0", 64))
	env.db.MustExec(`INSERT INTO login_sessions (id, user_id, token_hash) VALUES ($1, $2, $3)`, uuid.NewString(), oddID, acctTokenHash("odd-"+odd))
	status, body = env.do("PATCH", "/api/me", "odd-"+odd, map[string]any{"displayName": "Renamed"}, nil)
	expectInteraction(t, "odd username", status, body, http.StatusOK, "")
	var oddText *string
	env.db.Get(&oddText, `SELECT search_text FROM users WHERE id = $1`, oddID)
	if oddText != nil {
		t.Errorf("search_text for odd username = %q", *oddText)
	}

	// 表示名を含む変更で、プロフィール画像の URL も返す。
	jpg := encodeTestJPEG(t, 32, 32, false)
	if s, b := env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/jpeg", jpg); s != http.StatusNoContent {
		t.Fatalf("put profile image: %d %s", s, b)
	}
	status, body = env.do("PATCH", "/api/me", u.token, map[string]any{"displayName": "With Image"}, nil)
	expectInteraction(t, "with image", status, body, http.StatusOK, "")
	if url, _ := body["profileImageUrl"].(string); !strings.HasPrefix(url, "/api/images/") {
		t.Errorf("profileImageUrl = %v", body["profileImageUrl"])
	}
}

func acctTokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func TestAccountProfileImageBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	_, _, u := env.acctNewUser("asia")
	jpg := encodeTestJPEG(t, 48, 40, false)

	status, data := env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/png", jpg)
	if status != http.StatusUnsupportedMediaType || !strings.Contains(string(data), `"unsupported_media_type"`) {
		t.Errorf("wrong content type: %d %s", status, data)
	}
	status, data = env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/jpeg", []byte("not a jpeg"))
	if status != http.StatusBadRequest || !strings.Contains(string(data), `"invalid_profile_image"`) {
		t.Errorf("invalid jpeg: %d %s", status, data)
	}
	status, data = env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/jpeg", encodeTestJPEG(t, 513, 20, true))
	if status != http.StatusBadRequest || !strings.Contains(string(data), `"invalid_profile_image"`) {
		t.Errorf("too large: %d %s", status, data)
	}
	if n := env.acctImageCount(u.id); n != 0 {
		t.Errorf("rejected uploads left %d images", n)
	}
	status, data = env.acctRaw("DELETE", "/api/me/profile-image", u.token, "", nil)
	if status != http.StatusNotFound || !strings.Contains(string(data), `"account_not_found"`) {
		t.Errorf("delete without image: %d %s", status, data)
	}

	// 設定すると、GET /api/me に URL が出て、その URL で同じ画像を取得できる。
	status, data = env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/jpeg", jpg)
	if status != http.StatusNoContent || len(data) != 0 {
		t.Fatalf("put: %d %s", status, data)
	}
	_, me := env.acctMe(u.token)
	firstURL, _ := me["profileImageUrl"].(string)
	if s, got := env.acctRaw("GET", firstURL, "", "", nil); s != http.StatusOK || !bytes.Equal(got, jpg) {
		t.Errorf("get profile image: %d (%d bytes)", s, len(got))
	}
	var firstID string
	env.db.Get(&firstID, `SELECT profile_image_id FROM users WHERE id = $1`, u.id)
	var mediaType string
	env.db.Get(&mediaType, `SELECT media_type FROM image_objects WHERE id = $1`, firstID)
	if mediaType != "image/jpeg" || env.acctImageCount(u.id) != 1 {
		t.Errorf("after put: media_type = %q, images = %d", mediaType, env.acctImageCount(u.id))
	}

	// 差し替えると、前の画像の行は消え、新しい画像だけが残る。
	second := encodeTestJPEG(t, 512, 512, true)
	if s, b := env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/jpeg", second); s != http.StatusNoContent {
		t.Fatalf("replace: %d %s", s, b)
	}
	var secondID string
	env.db.Get(&secondID, `SELECT profile_image_id FROM users WHERE id = $1`, u.id)
	var remaining []string
	env.db.Select(&remaining, `SELECT id FROM image_objects WHERE owner_user_id = $1`, u.id)
	if secondID == firstID || len(remaining) != 1 || remaining[0] != secondID {
		t.Errorf("after replace: profile = %s, images = %v (first %s)", secondID, remaining, firstID)
	}
	_, me = env.acctMe(u.token)
	if s, got := env.acctRaw("GET", me["profileImageUrl"].(string), "", "", nil); s != http.StatusOK || !bytes.Equal(got, second) {
		t.Errorf("get replaced image: %d (%d bytes)", s, len(got))
	}

	// 削除すると、URL は null になり、画像の行も消える。2回目は 404。
	status, data = env.acctRaw("DELETE", "/api/me/profile-image", u.token, "", nil)
	if status != http.StatusNoContent || len(data) != 0 {
		t.Errorf("delete: %d %s", status, data)
	}
	if _, me = env.acctMe(u.token); me["profileImageUrl"] != nil {
		t.Errorf("profileImageUrl after delete = %v", me["profileImageUrl"])
	}
	if n := env.acctImageCount(u.id); n != 0 {
		t.Errorf("images after delete = %d", n)
	}
	status, data = env.acctRaw("DELETE", "/api/me/profile-image", u.token, "", nil)
	if status != http.StatusNotFound || !strings.Contains(string(data), `"account_not_found"`) {
		t.Errorf("second delete: %d %s", status, data)
	}
}

// 同じ利用者のプロフィール画像の同時の差し替えでも、最後に設定された画像の行だけが残る。
func TestAccountConcurrentProfileImages(t *testing.T) {
	env := newLockTestEnv(t)
	_, _, u := env.acctNewUser("asia")
	statuses := make([]int, 10)
	parallel(len(statuses), func(i int) {
		statuses[i], _ = env.acctRaw("PUT", "/api/me/profile-image", u.token, "image/jpeg", encodeTestJPEG(t, 16+i, 16, false))
	})
	for i, status := range statuses {
		if status != http.StatusNoContent {
			t.Errorf("put %d: %d", i, status)
		}
	}
	var profileID string
	env.db.Get(&profileID, `SELECT profile_image_id FROM users WHERE id = $1`, u.id)
	var remaining []string
	env.db.Select(&remaining, `SELECT id FROM image_objects WHERE owner_user_id = $1`, u.id)
	if len(remaining) != 1 || remaining[0] != profileID {
		t.Errorf("profile = %s, images = %v", profileID, remaining)
	}
}

func TestAccountDeletionBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	name, secret, u := env.acctNewUser("asia")
	status, data := env.acctRaw("POST", "/api/me/account-deletion", u.token, "", nil)
	if status != http.StatusNoContent || len(data) != 0 {
		t.Fatalf("delete account: %d %s", status, data)
	}
	var row struct {
		Status    string     `db:"status"`
		DeletedAt *time.Time `db:"deleted_at"`
	}
	env.db.Get(&row, `SELECT status, deleted_at FROM users WHERE id = $1`, u.id)
	if row.Status != "deleted" || row.DeletedAt == nil {
		t.Errorf("users row after deletion: %+v", row)
	}
	var reasons []string
	env.db.Select(&reasons, `SELECT coalesce(reason, '') FROM login_sessions WHERE user_id = $1 AND active = false`, u.id)
	if env.acctActiveSessions(u.id) != 0 || len(reasons) != 1 || reasons[0] != "account_deletion" {
		t.Errorf("sessions after deletion: active = %d, reasons = %v", env.acctActiveSessions(u.id), reasons)
	}
	if s, _ := env.acctMe(u.token); s != http.StatusUnauthorized {
		t.Errorf("me after deletion: %d", s)
	}
	status, body := env.acctLogin(name, secret)
	expectInteraction(t, "login after deletion", status, body, http.StatusUnauthorized, "invalid_credentials")
	status, data = env.acctRaw("POST", "/api/me/account-deletion", u.token, "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("second deletion: %d %s", status, data)
	}
	// 退会した利用者のユーザー名では、登録できない。
	status, body = env.acctRegister(name, "again", "asia")
	expectInteraction(t, "register deleted name", status, body, http.StatusConflict, "username_taken")
}

// 退会とログインを同時に行っても、退会が成功したら有効なセッションは残らない。
func TestAccountConcurrentDeletionAndLogin(t *testing.T) {
	env := newLockTestEnv(t)
	for range 8 {
		name, secret, u := env.acctNewUser("asia")
		var deleteStatus, loginStatus int
		parallel(2, func(i int) {
			if i == 0 {
				deleteStatus, _ = env.acctRaw("POST", "/api/me/account-deletion", u.token, "", nil)
			} else {
				loginStatus, _ = env.acctLogin(name, secret)
			}
		})
		if deleteStatus != http.StatusNoContent && deleteStatus != http.StatusUnauthorized {
			t.Errorf("deletion: %d", deleteStatus)
		}
		if loginStatus != http.StatusOK && loginStatus != http.StatusUnauthorized {
			t.Errorf("login: %d", loginStatus)
		}
		var status string
		env.db.Get(&status, `SELECT status FROM users WHERE id = $1`, u.id)
		if status == "deleted" && env.acctActiveSessions(u.id) != 0 {
			t.Errorf("deleted user has %d active sessions (delete %d, login %d)", env.acctActiveSessions(u.id), deleteStatus, loginStatus)
		}
	}
}

// isureal-guard が journal から数える操作ログ（ユーザー名、abuse、ステータス）が変わらないこと。
func TestAccountActionLog(t *testing.T) {
	env := newLockTestEnv(t)
	h := &HandlerContext{DB: env.db, Config: &Config{Region: "asia"}}
	var logs bytes.Buffer
	e := echo.New()
	e.IPExtractor = extractClientIP
	e.Use(ActionLogMiddleware("info", &logs))
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/api/auth/register", registerHandler(h))
	e.POST("/api/auth/login", loginHandler(h))
	e.PUT("/api/me/profile-image", putProfileImageHandler(h), SessionMiddleware(h, true))
	server := httptest.NewServer(e)
	defer server.Close()

	send := func(method, path, token, contentType string, body []byte) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Client-IP", "10.67.0.1")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var decoded map[string]any
		json.NewDecoder(res.Body).Decode(&decoded)
		return decoded
	}
	jsonBody := func(v any) []byte { data, _ := json.Marshal(v); return data }

	name := acctName()
	registered := send("POST", "/api/auth/register", "", "application/json", jsonBody(map[string]string{"username": name, "displayName": "n", "region": "asia"}))
	send("POST", "/api/auth/register", "", "application/json", jsonBody(map[string]string{"username": name, "displayName": "n", "region": "asia"}))
	send("POST", "/api/auth/register", "", "application/json", jsonBody(map[string]string{"username": "BAD", "displayName": "n", "region": "asia"}))
	send("POST", "/api/auth/login", "", "application/json", jsonBody(map[string]string{"username": name, "secretKey": "wrong"}))
	send("POST", "/api/auth/login", "", "application/json", jsonBody(map[string]string{"username": "BAD NAME", "secretKey": "wrong"}))
	login := send("POST", "/api/auth/login", "", "application/json", jsonBody(map[string]string{"username": name, "secretKey": registered["secretKey"].(string)}))
	token := login["token"].(string)
	userID := login["user"].(map[string]any)["id"].(string)
	send("PUT", "/api/me/profile-image", token, "image/jpeg", []byte("broken"))
	send("PUT", "/api/me/profile-image", token, "image/jpeg", encodeTestJPEG(t, 600, 10, true))
	send("PUT", "/api/me/profile-image", token, "image/jpeg", encodeTestJPEG(t, 10, 10, true))

	want := []map[string]any{
		{"level": "info", "action": "register", "status": 201, "ip": "10.67.0.1", "user_id": nil, "username": name},
		{"level": "info", "action": "register", "status": 409, "ip": "10.67.0.1", "user_id": nil, "username": name},
		{"level": "info", "action": "register", "status": 400, "ip": "10.67.0.1", "user_id": nil, "username": nil},
		{"level": "info", "action": "login", "status": 401, "ip": "10.67.0.1", "user_id": nil, "username": name},
		{"level": "info", "action": "login", "status": 401, "ip": "10.67.0.1", "user_id": nil, "username": nil},
		{"level": "info", "action": "login", "status": 200, "ip": "10.67.0.1", "user_id": nil, "username": name},
		{"level": "info", "action": "put_profile_image", "status": 400, "ip": "10.67.0.1", "user_id": userID, "abuse": "invalid_image"},
		{"level": "info", "action": "put_profile_image", "status": 400, "ip": "10.67.0.1", "user_id": userID},
		{"level": "info", "action": "put_profile_image", "status": 204, "ip": "10.67.0.1", "user_id": userID},
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("log lines = %d, want %d:\n%s", len(lines), len(want), logs.String())
	}
	for i, line := range lines {
		wantJSON, _ := json.Marshal(want[i])
		var got, expected map[string]any
		json.Unmarshal([]byte(line), &got)
		json.Unmarshal(wantJSON, &expected)
		if fmt.Sprint(got) != fmt.Sprint(expected) {
			t.Errorf("log %d = %s, want %s", i, line, wantJSON)
		}
	}
}
