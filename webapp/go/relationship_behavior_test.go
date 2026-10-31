package isureal

import (
	"net/http"
	"testing"
)

// #33 の往復削減の前後で、友達申請・承認・フォロー・ブロックの挙動（ステータス、応答、DB の状態）が変わらないことを確かめる。
// 変更前の実装でも同じテストが通ることを確認してある。ISUREAL_TEST_DSN を指定したときだけ実行する。

func (env *lockTestEnv) count(query string, args ...any) int {
	env.t.Helper()
	var n int
	if err := env.db.Get(&n, query, args...); err != nil {
		env.t.Fatal(err)
	}
	return n
}

func (env *lockTestEnv) friends(a, b lockTestUser) bool {
	return env.count(`SELECT count(*) FROM friendships WHERE user_low_id = $1 AND user_high_id = $2`, min(a.id, b.id), max(a.id, b.id)) == 1
}

func (env *lockTestEnv) requestStatus(id string) string {
	env.t.Helper()
	var status string
	if err := env.db.Get(&status, `SELECT status FROM friend_requests WHERE id = $1`, id); err != nil {
		env.t.Fatal(err)
	}
	return status
}

func expectStatus(t *testing.T, name string, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status = %d, want %d (body=%v)", name, got, want, body)
	}
}

func expectError(t *testing.T, name string, got, want int, body map[string]any, code string) {
	t.Helper()
	expectStatus(t, name, got, want, body)
	if body["error"] != code {
		t.Fatalf("%s: error = %v, want %s", name, body["error"], code)
	}
}

func TestRelationshipFriendRequestBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	a, b, c := env.user("asia"), env.user("asia"), env.user("asia")

	// 申請の作成
	status, body := env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
	expectStatus(t, "a->b", status, http.StatusCreated, body)
	if body["status"] != "pending" || body["relationId"] == "" || body["updatedAt"] == nil {
		t.Fatalf("a->b body = %v", body)
	}
	ab := body["relationId"].(string)
	// 同じ向きの二重申請は 409
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
	expectError(t, "a->b again", status, http.StatusConflict, body, "relationship_conflict")
	// 自分自身は 400、存在しない相手は 404
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": a.id}, nil)
	expectError(t, "a->a", status, http.StatusBadRequest, body, "invalid_relationship")
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": "00000000-0000-0000-0000-000000000000"}, nil)
	expectError(t, "a->missing", status, http.StatusNotFound, body, "relationship_not_found")
	// 逆向きの申請は、既存の申請を承認して友達になる
	status, body = env.do("POST", "/api/friend-requests", b.token, map[string]string{"recipientId": a.id}, nil)
	expectStatus(t, "b->a", status, http.StatusCreated, body)
	if body["status"] != "accepted" || body["relationId"] != ab {
		t.Fatalf("b->a body = %v (want accepted %s)", body, ab)
	}
	if !env.friends(a, b) || env.requestStatus(ab) != "accepted" {
		t.Fatal("a and b should be friends")
	}
	var source string
	env.db.Get(&source, `SELECT source_request_id FROM friendships WHERE user_low_id = $1 AND user_high_id = $2`, min(a.id, b.id), max(a.id, b.id))
	if source != ab {
		t.Fatalf("source_request_id = %s, want %s", source, ab)
	}
	// 友達どうしの申請は 409
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
	expectError(t, "a->b friends", status, http.StatusConflict, body, "relationship_conflict")

	// 承認・拒否・取り下げ
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": c.id}, nil)
	expectStatus(t, "a->c", status, http.StatusCreated, body)
	ac := body["relationId"].(string)
	status, body = env.do("POST", "/api/friend-requests/"+ac+"/accept", a.token, nil, nil)
	expectError(t, "a accepts own", status, http.StatusForbidden, body, "relationship_action_forbidden")
	status, body = env.do("POST", "/api/friend-requests/"+ac+"/cancel", c.token, nil, nil)
	expectError(t, "c cancels", status, http.StatusForbidden, body, "relationship_action_forbidden")
	status, body = env.do("POST", "/api/friend-requests/"+ac+"/unknown", c.token, nil, nil)
	expectError(t, "unknown action", status, http.StatusForbidden, body, "relationship_action_forbidden")
	status, body = env.do("POST", "/api/friend-requests/00000000-0000-0000-0000-000000000000/accept", c.token, nil, nil)
	expectError(t, "missing request", status, http.StatusNotFound, body, "relationship_not_found")
	status, body = env.do("POST", "/api/friend-requests/"+ac+"/reject", c.token, nil, nil)
	expectStatus(t, "c rejects", status, http.StatusOK, body)
	if body["status"] != "rejected" || body["relationId"] != ac || body["updatedAt"] == nil {
		t.Fatalf("reject body = %v", body)
	}
	if env.friends(a, c) {
		t.Fatal("a and c should not be friends after reject")
	}
	status, body = env.do("POST", "/api/friend-requests/"+ac+"/accept", c.token, nil, nil)
	expectError(t, "accept rejected", status, http.StatusForbidden, body, "relationship_action_forbidden")

	status, body = env.do("POST", "/api/friend-requests", c.token, map[string]string{"recipientId": a.id}, nil)
	expectStatus(t, "c->a", status, http.StatusCreated, body)
	ca := body["relationId"].(string)
	if body["status"] != "pending" {
		t.Fatalf("c->a after reject should be a new pending request: %v", body)
	}
	status, body = env.do("POST", "/api/friend-requests/"+ca+"/cancel", c.token, nil, nil)
	expectStatus(t, "c cancels own", status, http.StatusOK, body)
	if body["status"] != "cancelled" {
		t.Fatalf("cancel body = %v", body)
	}

	status, body = env.do("POST", "/api/friend-requests", c.token, map[string]string{"recipientId": a.id}, nil)
	expectStatus(t, "c->a again", status, http.StatusCreated, body)
	ca = body["relationId"].(string)
	status, body = env.do("POST", "/api/friend-requests/"+ca+"/accept", a.token, nil, nil)
	expectStatus(t, "a accepts", status, http.StatusOK, body)
	if body["status"] != "accepted" || body["relationId"] != ca {
		t.Fatalf("accept body = %v", body)
	}
	if !env.friends(a, c) {
		t.Fatal("a and c should be friends after accept")
	}
	status, body = env.do("POST", "/api/friend-requests/"+ca+"/accept", a.token, nil, nil)
	expectError(t, "accept twice", status, http.StatusForbidden, body, "relationship_action_forbidden")

	// 友達の解除
	status, body = env.do("DELETE", "/api/friendships/"+c.id, a.token, nil, nil)
	expectStatus(t, "unfriend", status, http.StatusNoContent, body)
	status, body = env.do("DELETE", "/api/friendships/"+c.id, a.token, nil, nil)
	expectError(t, "unfriend again", status, http.StatusConflict, body, "relationship_conflict")
}

func TestRelationshipBlockAndFollowBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	a, b, star, other := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.db.MustExec(`UPDATE users SET account_type = 'certified_public' WHERE id = $1`, star.id)

	// フォロー
	status, body := env.do("PUT", "/api/follows/"+b.id, a.token, nil, nil)
	expectError(t, "follow regular", status, http.StatusConflict, body, "relationship_conflict")
	status, body = env.do("PUT", "/api/follows/"+star.id, a.token, nil, nil)
	expectStatus(t, "follow star", status, http.StatusNoContent, body)
	status, body = env.do("PUT", "/api/follows/"+star.id, a.token, nil, nil)
	expectError(t, "follow star again", status, http.StatusConflict, body, "relationship_conflict")
	status, body = env.do("PUT", "/api/follows/"+a.id, a.token, nil, nil)
	expectError(t, "follow self", status, http.StatusBadRequest, body, "invalid_relationship")
	status, body = env.do("PUT", "/api/follows/00000000-0000-0000-0000-000000000000", a.token, nil, nil)
	expectError(t, "follow missing", status, http.StatusNotFound, body, "relationship_not_found")
	status, body = env.do("DELETE", "/api/follows/"+star.id, a.token, nil, nil)
	expectStatus(t, "unfollow", status, http.StatusNoContent, body)
	status, body = env.do("DELETE", "/api/follows/"+star.id, a.token, nil, nil)
	expectError(t, "unfollow again", status, http.StatusConflict, body, "relationship_conflict")

	// ブロックは、友達関係、双方向のフォロー、pending の申請を片付ける
	_, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": star.id}, nil)
	env.do("POST", "/api/friend-requests/"+body["relationId"].(string)+"/accept", star.token, nil, nil)
	env.do("PUT", "/api/follows/"+star.id, a.token, nil, nil)
	_, body = env.do("POST", "/api/friend-requests", star.token, map[string]string{"recipientId": other.id}, nil)
	otherRequest := body["relationId"].(string)
	_, body = env.do("POST", "/api/friend-requests", b.token, map[string]string{"recipientId": star.id}, nil)
	pendingBStar := body["relationId"].(string)
	if !env.friends(a, star) || env.count(`SELECT count(*) FROM follows WHERE follower_id = $1 AND followee_id = $2`, a.id, star.id) != 1 {
		t.Fatal("setup failed")
	}
	status, body = env.do("PUT", "/api/blocks/"+a.id, star.token, nil, nil)
	expectStatus(t, "star blocks a", status, http.StatusNoContent, body)
	if env.friends(a, star) || env.count(`SELECT count(*) FROM follows WHERE (follower_id = $1 AND followee_id = $2) OR (follower_id = $2 AND followee_id = $1)`, a.id, star.id) != 0 {
		t.Fatal("block should remove friendship and follows")
	}
	if env.requestStatus(otherRequest) != "pending" {
		t.Fatal("block must not touch requests of other pairs")
	}
	status, body = env.do("PUT", "/api/blocks/"+a.id, star.token, nil, nil)
	expectError(t, "block again", status, http.StatusConflict, body, "relationship_conflict")
	status, body = env.do("PUT", "/api/blocks/"+star.id, a.token, nil, nil)
	expectStatus(t, "a blocks star back", status, http.StatusNoContent, body)
	// ブロックされている相手への申請とフォローは 409
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": star.id}, nil)
	expectError(t, "request while blocked", status, http.StatusConflict, body, "relationship_conflict")
	status, body = env.do("PUT", "/api/follows/"+star.id, a.token, nil, nil)
	expectError(t, "follow while blocked", status, http.StatusConflict, body, "relationship_conflict")

	// pending の申請は、ブロックで取り下げになる
	status, body = env.do("PUT", "/api/blocks/"+b.id, star.token, nil, nil)
	expectStatus(t, "star blocks b", status, http.StatusNoContent, body)
	if env.requestStatus(pendingBStar) != "cancelled" {
		t.Fatalf("pending request should be cancelled by block: %s", env.requestStatus(pendingBStar))
	}
	status, body = env.do("POST", "/api/friend-requests/"+pendingBStar+"/accept", star.token, nil, nil)
	expectError(t, "accept cancelled by block", status, http.StatusForbidden, body, "relationship_action_forbidden")

	// ブロックの解除
	status, body = env.do("DELETE", "/api/blocks/"+b.id, star.token, nil, nil)
	expectStatus(t, "unblock", status, http.StatusNoContent, body)
	status, body = env.do("DELETE", "/api/blocks/"+b.id, star.token, nil, nil)
	expectError(t, "unblock again", status, http.StatusForbidden, body, "relationship_action_forbidden")
	status, body = env.do("POST", "/api/friend-requests", b.token, map[string]string{"recipientId": star.id}, nil)
	expectStatus(t, "request after unblock", status, http.StatusCreated, body)
}

// 互いに同時に申請しても、友達関係は1件だけで、片方が pending、もう片方が accepted になる。
// 同時の承認とブロックでも、500 やデッドロックにならず、ブロックの後に友達関係は残らない。
func TestRelationshipConcurrent(t *testing.T) {
	env := newLockTestEnv(t)
	for i := range 10 {
		a, b := env.user("asia"), env.user("asia")
		results := make([]map[string]any, 2)
		statuses := make([]int, 2)
		parallel(2, func(j int) {
			from, to := a, b
			if j == 1 {
				from, to = b, a
			}
			statuses[j], results[j] = env.do("POST", "/api/friend-requests", from.token, map[string]string{"recipientId": to.id}, nil)
		})
		if statuses[0] != http.StatusCreated || statuses[1] != http.StatusCreated {
			t.Fatalf("round %d: statuses = %v %v", i, statuses, results)
		}
		got := map[any]int{results[0]["status"]: 1}
		got[results[1]["status"]]++
		if got["pending"] != 1 || got["accepted"] != 1 || results[0]["relationId"] != results[1]["relationId"] || !env.friends(a, b) {
			t.Fatalf("round %d: results = %v", i, results)
		}
	}
	for i := range 10 {
		a, b := env.user("asia"), env.user("asia")
		_, body := env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
		id := body["relationId"].(string)
		statuses := make([]int, 2)
		parallel(2, func(j int) {
			if j == 0 {
				statuses[j], _ = env.do("POST", "/api/friend-requests/"+id+"/accept", b.token, nil, nil)
			} else {
				statuses[j], _ = env.do("PUT", "/api/blocks/"+b.id, a.token, nil, nil)
			}
		})
		if statuses[1] != http.StatusNoContent || (statuses[0] != http.StatusOK && statuses[0] != http.StatusForbidden) {
			t.Fatalf("round %d: accept=%d block=%d", i, statuses[0], statuses[1])
		}
		if env.friends(a, b) {
			t.Fatalf("round %d: friendship remains after block", i)
		}
	}
}

// 退会した相手との操作は、ロックを取れないので 404 になる（#33 案A で DB の関数に移した分岐）。
func TestRelationshipDeletedUserBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	a, b, gone := env.user("asia"), env.user("asia"), env.user("asia")

	// a から gone への申請と、b から a への申請を作ってから gone を退会させる
	status, body := env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": gone.id}, nil)
	expectStatus(t, "a->gone", status, http.StatusCreated, body)
	toGone := body["relationId"].(string)
	status, body = env.do("POST", "/api/me/account-deletion", gone.token, nil, nil)
	expectStatus(t, "gone deletes account", status, http.StatusNoContent, body)

	status, body = env.do("POST", "/api/friend-requests", b.token, map[string]string{"recipientId": gone.id}, nil)
	expectError(t, "request to deleted", status, http.StatusNotFound, body, "relationship_not_found")
	status, body = env.do("POST", "/api/friend-requests/"+toGone+"/cancel", a.token, nil, nil)
	expectError(t, "cancel request to deleted", status, http.StatusNotFound, body, "relationship_not_found")
	if env.requestStatus(toGone) != "pending" {
		t.Fatal("request to deleted user should stay pending")
	}
	// 権限の確認はロックの前に行うので、権限がなければ退会済みでも 403
	status, body = env.do("POST", "/api/friend-requests/"+toGone+"/accept", a.token, nil, nil)
	expectError(t, "accept own request to deleted", status, http.StatusForbidden, body, "relationship_action_forbidden")
	for _, op := range []struct{ name, method, path string }{
		{"follow deleted", "PUT", "/api/follows/"},
		{"unfollow deleted", "DELETE", "/api/follows/"},
		{"block deleted", "PUT", "/api/blocks/"},
		{"unblock deleted", "DELETE", "/api/blocks/"},
		{"unfriend deleted", "DELETE", "/api/friendships/"},
	} {
		status, body = env.do(op.method, op.path+gone.id, b.token, nil, nil)
		expectError(t, op.name, status, http.StatusNotFound, body, "relationship_not_found")
	}
}
