package isureal

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// #87：フォロー・フォローの解除・友達申請・申請の承認・拒否・取り下げで、相手の users 行を FOR SHARE でロックする。
// 同じ相手への別々の行為者からの操作と、相手の投稿へのコメント・RealMoji が並行して進み、
// 権限を減らす変更（友達の解除、ブロック）とは今まで通り直列になることを確かめる。
// ISUREAL_TEST_DSN を指定したときだけ実行する。

// targetLockTimeout は、ロックを待たされたと判定するまでの時間。待たない操作は、これより十分に早く終わる。
const targetLockTimeout = "300ms"

// targetLockHold は、別のトランザクションで query を実行し、そのロックを持ったままにする。返した関数でロールバックする。
func (env *lockTestEnv) targetLockHold(query string, args ...any) (release func()) {
	env.t.Helper()
	tx := env.db.MustBegin()
	if _, err := tx.Exec(query, args...); err != nil {
		tx.Rollback()
		env.t.Fatalf("%s: %v", query, err)
	}
	return func() { tx.Rollback() }
}

// targetLockWaits は、別のトランザクションで query を実行し、targetLockTimeout のうちにロックを取れなければ true を返す。
// 最後にロールバックするので、DB の状態は変わらない。
func (env *lockTestEnv) targetLockWaits(query string, args ...any) bool {
	env.t.Helper()
	tx := env.db.MustBegin()
	defer tx.Rollback()
	tx.MustExec(`SET LOCAL lock_timeout = '` + targetLockTimeout + `'`)
	_, err := tx.Exec(query, args...)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.As(err, &pgErr) && pgErr.Code == "55P03": // lock_not_available
		return true
	default:
		env.t.Fatalf("%s: %v", query, err)
		return false
	}
}

func TestRelationshipTargetLockModes(t *testing.T) {
	env := newLockTestEnv(t)
	a, b, c, d, x, star := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.db.MustExec(`UPDATE users SET account_type = 'certified_public' WHERE id = $1`, star.id)
	// a は star をフォローし、star に申請している。star は b に申請している。c と star は友達。
	request := func(from, to lockTestUser) string {
		t.Helper()
		status, body := env.do("POST", "/api/friend-requests", from.token, map[string]string{"recipientId": to.id}, nil)
		expectStatus(t, "friend request", status, http.StatusCreated, body)
		return body["relationId"].(string)
	}
	env.mustDo("PUT", "/api/follows/"+star.id, a.token, http.StatusNoContent)
	aToStar := request(a, star)
	starToB := request(star, b)
	env.mustDo("POST", "/api/friend-requests/"+request(c, star)+"/accept", star.token, http.StatusOK)
	low, high := min(c.id, star.id), max(c.id, star.id)

	check := func(name string, wantWait bool, query string, args ...any) {
		t.Helper()
		if got := env.targetLockWaits(query, args...); got != wantWait {
			t.Errorf("%s: waited = %v, want %v", name, got, wantWait)
		}
	}

	// star の投稿へのコメント・RealMoji と同じロック（行為者 x を FOR NO KEY UPDATE、投稿者 star を FOR SHARE）を持っている間。
	release := env.targetLockHold(`SELECT isureal_lock_interaction_users($1, $2)`, x.id, star.id)
	// 権限を減らさない操作は、star が相手でも待たない。
	check("follow star", false, `SELECT isureal_follow($1, $2, $3)`, b.id, star.id, uuid.NewString())
	check("unfollow star", false, `SELECT isureal_unfollow($1, $2)`, a.id, star.id)
	check("request to star", false, `SELECT * FROM isureal_create_friend_request($1, $2, $3, $4, $5, $6)`,
		d.id, star.id, uuid.NewString(), uuid.NewString(), min(d.id, star.id), max(d.id, star.id))
	check("cancel request to star", false, `SELECT * FROM isureal_transition_friend_request($1, $2, 'cancelled', $3)`, a.id, aToStar, uuid.NewString())
	check("accept request from star", false, `SELECT * FROM isureal_transition_friend_request($1, $2, 'accepted', $3)`, b.id, starToB, uuid.NewString())
	// 権限を減らす変更は、今まで通り star の行で待つ。
	check("unfriend star", true, `SELECT isureal_delete_friendship($1, $2, $3, $4)`, c.id, star.id, low, high)
	check("block star", true, `SELECT isureal_block($1, $2, $3, $4, $5)`, a.id, star.id, uuid.NewString(), min(a.id, star.id), max(a.id, star.id))
	check("unblock star", true, `SELECT isureal_unblock($1, $2)`, a.id, star.id)
	// star 自身が行為者の操作は、今まで通り star の行で待つ。
	check("star accepts", true, `SELECT * FROM isureal_transition_friend_request($1, $2, 'accepted', $3)`, star.id, aToStar, uuid.NewString())
	release()

	// a が star をフォローしている間（a を FOR NO KEY UPDATE、star を FOR SHARE）。
	release = env.targetLockHold(`SELECT isureal_lock_actor_and_target($1, $2)`, a.id, star.id)
	// ほかの利用者から star への操作と、star の投稿へのコメント・RealMoji は待たない。
	check("other follows star", false, `SELECT isureal_follow($1, $2, $3)`, b.id, star.id, uuid.NewString())
	check("comment on star's post", false, `SELECT isureal_lock_interaction_users($1, $2)`, x.id, star.id)
	// 同じ行為者 a の操作と、a の投稿へのコメント・RealMoji は待つ（行為者の行は FOR NO KEY UPDATE のまま）。
	check("a unfollows", true, `SELECT isureal_unfollow($1, $2)`, a.id, star.id)
	check("comment on a's post", true, `SELECT isureal_lock_interaction_users($1, $2)`, x.id, a.id)
	// star のブロック、退会や認定（UPDATE users）は待つ。
	check("star blocks d", true, `SELECT isureal_block($1, $2, $3, $4, $5)`, star.id, d.id, uuid.NewString(), min(star.id, d.id), max(star.id, d.id))
	check("update star", true, `UPDATE users SET updated_at = clock_timestamp() WHERE id = $1`, star.id)
	release()
}

// 多数の利用者が同じ認定公開アカウントを同時にフォロー・フォローの解除しても、全部受け付ける。
func TestRelationshipTargetLockManyFollowers(t *testing.T) {
	env := newLockTestEnv(t)
	star := env.user("asia")
	env.db.MustExec(`UPDATE users SET account_type = 'certified_public' WHERE id = $1`, star.id)
	followers := make([]lockTestUser, 30)
	for i := range followers {
		followers[i] = env.user("asia")
	}
	for _, op := range []struct {
		method string
		want   int
	}{{"PUT", len(followers)}, {"DELETE", 0}} {
		statuses := make([]int, len(followers))
		bodies := make([]map[string]any, len(followers))
		parallel(len(followers), func(i int) {
			statuses[i], bodies[i] = env.do(op.method, "/api/follows/"+star.id, followers[i].token, nil, nil)
		})
		for i, status := range statuses {
			expectStatus(t, op.method+" follow", status, http.StatusNoContent, bodies[i])
		}
		if n := env.count(`SELECT count(*) FROM follows WHERE followee_id = $1`, star.id); n != op.want {
			t.Fatalf("%s: follows = %d, want %d", op.method, n, op.want)
		}
	}
}

// フォローとブロックを同時に送っても、デッドロックや 500 にならない。
// ブロックが確定した後には、ブロックした2人の間のフォローは残らない（ブロックの前に確定したフォローはブロックで消え、後のフォローは 409）。
// ブロックに関わらない利用者から同じ相手へのフォローは、受け付ける。
func TestRelationshipTargetLockFollowVersusBlock(t *testing.T) {
	env := newLockTestEnv(t)
	rounds := 15
	type round struct{ a, b, star lockTestUser }
	rs := make([]round, rounds)
	for i := range rs {
		rs[i] = round{env.user("asia"), env.user("asia"), env.user("asia")}
		env.db.MustExec(`UPDATE users SET account_type = 'certified_public' WHERE id = $1`, rs[i].star.id)
	}
	statuses := make([][3]int, rounds)
	bodies := make([][3]map[string]any, rounds)
	parallel(rounds*3, func(i int) {
		r, op := rs[i%rounds], i/rounds
		switch op {
		case 0: // a が star をフォロー
			statuses[i%rounds][op], bodies[i%rounds][op] = env.do("PUT", "/api/follows/"+r.star.id, r.a.token, nil, nil)
		case 1: // star が a をブロック
			statuses[i%rounds][op], bodies[i%rounds][op] = env.do("PUT", "/api/blocks/"+r.a.id, r.star.token, nil, nil)
		case 2: // b が star をフォロー
			statuses[i%rounds][op], bodies[i%rounds][op] = env.do("PUT", "/api/follows/"+r.star.id, r.b.token, nil, nil)
		}
	})
	for i, r := range rs {
		s, b := statuses[i], bodies[i]
		if s[0] != http.StatusNoContent && (s[0] != http.StatusConflict || b[0]["error"] != "relationship_conflict") {
			t.Errorf("round %d: a follows star: %d %v", i, s[0], b[0])
		}
		expectStatus(t, "star blocks a", s[1], http.StatusNoContent, b[1])
		expectStatus(t, "b follows star", s[2], http.StatusNoContent, b[2])
		if n := env.count(`SELECT count(*) FROM follows WHERE (follower_id = $1 AND followee_id = $2) OR (follower_id = $2 AND followee_id = $1)`, r.a.id, r.star.id); n != 0 {
			t.Errorf("round %d: %d follows remain between a and star after the block", i, n)
		}
		if n := env.count(`SELECT count(*) FROM follows WHERE follower_id = $1 AND followee_id = $2`, r.b.id, r.star.id); n != 1 {
			t.Errorf("round %d: b follows star = %d, want 1", i, n)
		}
	}
}

// 同じ受信者への多数の申請と、受信者からの逆向きの申請を同時に送っても、デッドロックや 500 にならない。
// 送り合った2人は友達になり（片方が pending、もう片方が accepted）、片方だけが送った申請は pending のまま残る。
// その後、受信者の承認と、申請者の取り下げを同時に送っても、それぞれ受け付ける。
func TestRelationshipTargetLockConcurrentRequestsToOneRecipient(t *testing.T) {
	env := newLockTestEnv(t)
	recipient := env.user("asia")
	requesters := make([]lockTestUser, 12)
	for i := range requesters {
		requesters[i] = env.user("asia")
	}
	mutual := 6
	toRecipient := make([]int, len(requesters))
	toRecipientBody := make([]map[string]any, len(requesters))
	fromRecipient := make([]int, mutual)
	fromRecipientBody := make([]map[string]any, mutual)
	parallel(len(requesters)+mutual, func(i int) {
		if i < len(requesters) {
			toRecipient[i], toRecipientBody[i] = env.do("POST", "/api/friend-requests", requesters[i].token, map[string]string{"recipientId": recipient.id}, nil)
			return
		}
		j := i - len(requesters)
		fromRecipient[j], fromRecipientBody[j] = env.do("POST", "/api/friend-requests", recipient.token, map[string]string{"recipientId": requesters[j].id}, nil)
	})
	for i, requester := range requesters {
		expectStatus(t, "request to recipient", toRecipient[i], http.StatusCreated, toRecipientBody[i])
		if i < mutual {
			expectStatus(t, "request from recipient", fromRecipient[i], http.StatusCreated, fromRecipientBody[i])
			got := map[any]int{toRecipientBody[i]["status"]: 1}
			got[fromRecipientBody[i]["status"]]++
			if got["pending"] != 1 || got["accepted"] != 1 || toRecipientBody[i]["relationId"] != fromRecipientBody[i]["relationId"] || !env.friends(recipient, requester) {
				t.Errorf("mutual %d: %v %v", i, toRecipientBody[i], fromRecipientBody[i])
			}
			continue
		}
		if toRecipientBody[i]["status"] != "pending" || env.friends(recipient, requester) {
			t.Errorf("request %d: %v", i, toRecipientBody[i])
		}
	}

	// 残りの申請の半分を受信者が承認し、もう半分を申請者が取り下げる。
	pending := requesters[mutual:]
	half := len(pending) / 2
	statuses := make([]int, len(pending))
	bodies := make([]map[string]any, len(pending))
	parallel(len(pending), func(i int) {
		id := toRecipientBody[mutual+i]["relationId"].(string)
		if i < half {
			statuses[i], bodies[i] = env.do("POST", "/api/friend-requests/"+id+"/accept", recipient.token, nil, nil)
		} else {
			statuses[i], bodies[i] = env.do("POST", "/api/friend-requests/"+id+"/cancel", pending[i].token, nil, nil)
		}
	})
	for i, requester := range pending {
		expectStatus(t, "transition", statuses[i], http.StatusOK, bodies[i])
		want := "cancelled"
		if i < half {
			want = "accepted"
		}
		if bodies[i]["status"] != want || env.friends(recipient, requester) != (i < half) {
			t.Errorf("transition %d: %v, friends = %v", i, bodies[i], env.friends(recipient, requester))
		}
	}
}
