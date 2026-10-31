package isureal

import (
	"net/http"
	"testing"
)

// #84 のログアウトの挙動テスト。users 行のロックとトランザクションをやめて autocommit の1文にする前の main でも、後でも同じ結果になることを確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql を流した DB を指定したときだけ実行する。
// 利用者は asia にする（テストのサーバは asia で、asia はセッションをキャッシュしないので、反映待ちなしに DB の状態がそのまま見える）。

func (env *lockTestEnv) logoutSessionState(token string) (active bool, reason *string) {
	env.t.Helper()
	var row struct {
		Active bool    `db:"active"`
		Reason *string `db:"reason"`
	}
	if err := env.db.Get(&row, `SELECT active, reason FROM login_sessions WHERE token_hash = encode(sha256($1::bytea), 'hex')`, token); err != nil {
		env.t.Fatal(err)
	}
	return row.Active, row.Reason
}

// ログアウトしたトークンは 401 になり、同じ利用者のほかの有効なセッションは無効にしない。
func TestLogoutBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	_, _, user := env.acctNewUser("asia")
	if status, body := env.acctMe(user.token); status != http.StatusOK {
		t.Fatalf("me before logout: %d %v", status, body)
	}

	// ログインは前のセッションを置き換えるので、別のトークンの有効なセッションを DB に直接足す。
	otherToken, otherHash := newLoginSessionToken()
	env.db.MustExec(`INSERT INTO login_sessions (id, user_id, token_hash) VALUES (gen_random_uuid()::text, $1, $2)`, user.id, otherHash)

	status, body := env.do("POST", "/api/auth/logout", user.token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("logout: %d %v", status, body)
	}
	status, body = env.acctMe(user.token)
	expectInteraction(t, "me after logout", status, body, http.StatusUnauthorized, "invalid_session")
	if active, reason := env.logoutSessionState(user.token); active || reason == nil || *reason != "logout" {
		t.Errorf("logged out session: active=%v reason=%v, want inactive with reason logout", active, reason)
	}

	// 同じトークンでもう一度ログアウトすると、セッションの確認で 401 になる。
	status, body = env.do("POST", "/api/auth/logout", user.token, nil, nil)
	expectInteraction(t, "second logout", status, body, http.StatusUnauthorized, "invalid_session")

	// ほかのトークンのセッションは有効なまま。
	if status, body := env.acctMe(otherToken); status != http.StatusOK {
		t.Errorf("me with the other token: %d %v, want 200", status, body)
	}
	if n := env.acctActiveSessions(user.id); n != 1 {
		t.Errorf("active sessions = %d, want 1", n)
	}
}

// 同じ利用者のログインと、同じトークンでのログアウト2回を同時に送っても 500 にならない。
// ログアウトしたトークンは使えなくなり、同時のログインで得たトークンだけが有効なまま残る。
func TestLogoutConcurrentWithLogin(t *testing.T) {
	env := newLockTestEnv(t)
	name, secret, user := env.acctNewUser("asia")
	token := user.token
	for iteration := range 20 {
		var loginStatus int
		var login map[string]any
		logoutStatuses := make([]int, 2)
		logoutBodies := make([]map[string]any, 2)
		parallel(3, func(i int) {
			if i == 2 {
				loginStatus, login = env.acctLogin(name, secret)
				return
			}
			logoutStatuses[i], logoutBodies[i] = env.do("POST", "/api/auth/logout", token, nil, nil)
		})
		if loginStatus != http.StatusOK {
			t.Fatalf("iteration %d: login: %d %v", iteration, loginStatus, login)
		}
		for i, status := range logoutStatuses {
			// ログインかもう一方のログアウトが先に確定していれば、セッションの確認で 401 になる。
			if status == http.StatusUnauthorized {
				expectInteraction(t, "concurrent logout", status, logoutBodies[i], http.StatusUnauthorized, "invalid_session")
			} else if status != http.StatusNoContent {
				t.Fatalf("iteration %d: logout %d: %d %v, want 204 or 401", iteration, i, status, logoutBodies[i])
			}
		}
		if status, body := env.acctMe(token); status != http.StatusUnauthorized {
			t.Fatalf("iteration %d: me with the old token: %d %v, want 401", iteration, status, body)
		}
		if active, reason := env.logoutSessionState(token); active || reason == nil || (*reason != "logout" && *reason != "replaced") {
			t.Fatalf("iteration %d: old session: active=%v reason=%v, want inactive with reason logout or replaced", iteration, active, reason)
		}
		token = login["token"].(string)
		if status, body := env.acctMe(token); status != http.StatusOK {
			t.Fatalf("iteration %d: me with the new token: %d %v, want 200", iteration, status, body)
		}
		if n := env.acctActiveSessions(user.id); n != 1 {
			t.Fatalf("iteration %d: active sessions = %d, want 1", iteration, n)
		}
	}
}
