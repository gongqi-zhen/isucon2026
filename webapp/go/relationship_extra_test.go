package isureal

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// #33 案A（#43、関数1回の呼び出し）の前後で、エラーの順序と同時実行の結果が変わらないことを確かめる。
// relationship_behavior_test.go で見ていない分を足したもの。#34 の版（f043af7）と #43 の版の両方で通ることを確認してある。
// ISUREAL_TEST_DSN を指定したときだけ実行する。

func TestRelationshipErrorOrder(t *testing.T) {
	env := newLockTestEnv(t)
	a, b, c := env.user("asia"), env.user("asia"), env.user("asia")

	// 自分自身や空の相手は、DB に問い合わせる前に 400
	status, body := env.do("PUT", "/api/blocks/"+a.id, a.token, nil, nil)
	expectError(t, "block self", status, http.StatusBadRequest, body, "invalid_relationship")
	status, body = env.do("DELETE", "/api/friendships/"+a.id, a.token, nil, nil)
	expectError(t, "unfriend self", status, http.StatusBadRequest, body, "invalid_relationship")
	status, body = env.do("DELETE", "/api/follows/"+a.id, a.token, nil, nil)
	expectError(t, "unfollow self", status, http.StatusBadRequest, body, "invalid_relationship")
	status, body = env.do("DELETE", "/api/blocks/"+a.id, a.token, nil, nil)
	expectError(t, "unblock self", status, http.StatusBadRequest, body, "invalid_relationship")

	// 存在しない相手は 404
	missing := "00000000-0000-0000-0000-000000000000"
	for _, op := range []struct{ method, path string }{
		{"PUT", "/api/blocks/"}, {"DELETE", "/api/blocks/"}, {"DELETE", "/api/follows/"}, {"DELETE", "/api/friendships/"},
	} {
		status, body = env.do(op.method, op.path+missing, a.token, nil, nil)
		expectError(t, op.method+" "+op.path+"missing", status, http.StatusNotFound, body, "relationship_not_found")
	}

	// 何もないときの解除
	status, body = env.do("DELETE", "/api/follows/"+b.id, a.token, nil, nil)
	expectError(t, "unfollow not following", status, http.StatusConflict, body, "relationship_conflict")
	status, body = env.do("DELETE", "/api/friendships/"+b.id, a.token, nil, nil)
	expectError(t, "unfriend not friends", status, http.StatusConflict, body, "relationship_conflict")

	// 申請の相手が退会したあとの操作
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
	expectStatus(t, "a->b", status, http.StatusCreated, body)
	ab := body["relationId"].(string)
	status, body = env.do("POST", "/api/friend-requests", c.token, map[string]string{"recipientId": a.id}, nil)
	expectStatus(t, "c->a", status, http.StatusCreated, body)
	ca := body["relationId"].(string)
	status, body = env.do("POST", "/api/me/account-deletion", b.token, nil, nil)
	expectStatus(t, "delete b", status, http.StatusNoContent, body)
	// 権限の確認が先なので、本人以外は 403
	status, body = env.do("POST", "/api/friend-requests/"+ab+"/cancel", c.token, nil, nil)
	expectError(t, "cancel by other (b deleted)", status, http.StatusForbidden, body, "relationship_action_forbidden")
	// 本人なら、相手が active でないので 404
	status, body = env.do("POST", "/api/friend-requests/"+ab+"/cancel", a.token, nil, nil)
	expectError(t, "cancel (b deleted)", status, http.StatusNotFound, body, "relationship_not_found")
	if env.requestStatus(ab) != "pending" {
		t.Fatalf("a->b status = %s, want pending", env.requestStatus(ab))
	}
	status, body = env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
	expectError(t, "request to deleted", status, http.StatusNotFound, body, "relationship_not_found")
	status, body = env.do("PUT", "/api/blocks/"+b.id, a.token, nil, nil)
	expectError(t, "block deleted", status, http.StatusNotFound, body, "relationship_not_found")

	// 拒否された申請を再び操作すると 403、本人以外の拒否も 403
	status, body = env.do("POST", "/api/friend-requests/"+ca+"/reject", c.token, nil, nil)
	expectError(t, "reject by requester", status, http.StatusForbidden, body, "relationship_action_forbidden")
	status, body = env.do("POST", "/api/friend-requests/"+ca+"/reject", a.token, nil, nil)
	expectStatus(t, "reject", status, http.StatusOK, body)
	if body["relationId"] != ca || body["status"] != "rejected" || body["updatedAt"] == nil {
		t.Fatalf("reject body = %v", body)
	}
	status, body = env.do("POST", "/api/friend-requests/"+ca+"/accept", a.token, nil, nil)
	expectError(t, "accept rejected", status, http.StatusForbidden, body, "relationship_action_forbidden")
	if env.friends(a, c) {
		t.Fatal("a and c must not be friends")
	}
}

func TestRelationshipConcurrentSameDirection(t *testing.T) {
	env := newLockTestEnv(t)
	a, b := env.user("asia"), env.user("asia")
	var created, conflicts atomic.Int32
	parallel(8, func(int) {
		status, body := env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
		switch {
		case status == http.StatusCreated:
			created.Add(1)
		case status == http.StatusConflict && body["error"] == "relationship_conflict":
			conflicts.Add(1)
		default:
			t.Errorf("a->b: %d %v", status, body)
		}
	})
	if created.Load() != 1 || conflicts.Load() != 7 {
		t.Fatalf("created=%d conflicts=%d, want 1 and 7", created.Load(), conflicts.Load())
	}
	if n := env.count(`SELECT count(*) FROM friend_requests WHERE requester_id = $1 AND recipient_id = $2 AND status = 'pending'`, a.id, b.id); n != 1 {
		t.Fatalf("pending requests = %d, want 1", n)
	}
}

func TestRelationshipConcurrentAcceptAndCancel(t *testing.T) {
	env := newLockTestEnv(t)
	for range 5 {
		a, b := env.user("asia"), env.user("asia")
		status, body := env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
		expectStatus(t, "a->b", status, http.StatusCreated, body)
		id := body["relationId"].(string)
		var ok, forbidden atomic.Int32
		parallel(2, func(i int) {
			path, token := "/api/friend-requests/"+id+"/accept", b.token
			if i == 1 {
				path, token = "/api/friend-requests/"+id+"/cancel", a.token
			}
			status, body := env.do("POST", path, token, nil, nil)
			switch {
			case status == http.StatusOK:
				ok.Add(1)
			case status == http.StatusForbidden && body["error"] == "relationship_action_forbidden":
				forbidden.Add(1)
			default:
				t.Errorf("%s: %d %v", path, status, body)
			}
		})
		if ok.Load() != 1 || forbidden.Load() != 1 {
			t.Fatalf("ok=%d forbidden=%d, want 1 and 1", ok.Load(), forbidden.Load())
		}
		// 承認が勝ったときだけ友達になる
		if want := env.requestStatus(id) == "accepted"; env.friends(a, b) != want {
			t.Fatalf("friends = %v, status = %s", env.friends(a, b), env.requestStatus(id))
		}
	}
}

func TestRelationshipConcurrentFollow(t *testing.T) {
	env := newLockTestEnv(t)
	a, b := env.user("asia"), env.user("asia")
	env.db.MustExec(`UPDATE users SET account_type = 'certified_public' WHERE id = $1`, b.id)
	var followed, conflicts atomic.Int32
	parallel(8, func(int) {
		status, body := env.do("PUT", "/api/follows/"+b.id, a.token, nil, nil)
		switch {
		case status == http.StatusNoContent:
			followed.Add(1)
		case status == http.StatusConflict && body["error"] == "relationship_conflict":
			conflicts.Add(1)
		default:
			t.Errorf("follow: %d %v", status, body)
		}
	})
	if followed.Load() != 1 || conflicts.Load() != 7 {
		t.Fatalf("followed=%d conflicts=%d, want 1 and 7", followed.Load(), conflicts.Load())
	}
}
