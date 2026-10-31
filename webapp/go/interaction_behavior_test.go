package isureal

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// #50 の、投稿・RealMoji・コメントの挙動テスト。
// 関数1回の呼び出しに書き換える前の main でも、書き換えた後でも同じ結果になることを確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql、posting_window_functions.sql を流した DB を指定したときだけ実行する。

var postForm = map[string]string{"visibility": "public", "location_kind": "none"}

func (env *lockTestEnv) post(u lockTestUser, form map[string]string) (int, map[string]any) {
	return env.do("POST", "/api/posts", u.token, nil, form)
}

func (env *lockTestEnv) mustPost(u lockTestUser, form map[string]string) string {
	env.t.Helper()
	status, body := env.post(u, form)
	if status != http.StatusCreated {
		env.t.Fatalf("post: %d %v", status, body)
	}
	return body["id"].(string)
}

func (env *lockTestEnv) befriend(a, b lockTestUser) {
	env.t.Helper()
	status, body := env.do("POST", "/api/friend-requests", a.token, map[string]string{"recipientId": b.id}, nil)
	if status != http.StatusCreated && status != http.StatusOK {
		env.t.Fatalf("friend request: %d %v", status, body)
	}
	if status, body := env.do("POST", "/api/friend-requests/"+body["relationId"].(string)+"/accept", b.token, nil, nil); status != http.StatusOK {
		env.t.Fatalf("accept: %d %v", status, body)
	}
}

func (env *lockTestEnv) mustOpenWindow(admin string) string {
	env.t.Helper()
	status, body := env.openWindow(admin)
	if status != http.StatusCreated {
		env.t.Fatalf("open window: %d %v", status, body)
	}
	return body["id"].(string)
}

func (env *lockTestEnv) realmoji(u lockTestUser, postID, reactionType string) (int, map[string]any) {
	return env.do("POST", "/api/posts/"+postID+"/realmojis", u.token, nil, map[string]string{"reactionType": reactionType})
}

func (env *lockTestEnv) comment(u lockTestUser, postID, body string) (int, map[string]any) {
	return env.do("POST", "/api/posts/"+postID+"/comments", u.token, map[string]string{"body": body}, nil)
}

func expectInteraction(t *testing.T, name string, status int, body map[string]any, wantStatus int, wantError string) {
	t.Helper()
	if status != wantStatus {
		t.Errorf("%s: status = %d, want %d (%v)", name, status, wantStatus, body)
		return
	}
	if wantError != "" && body["error"] != wantError {
		t.Errorf("%s: error = %v, want %q", name, body["error"], wantError)
	}
}

func TestInteractionCreatePostBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	windowID := env.mustOpenWindow(admin)
	u := env.user("asia")

	locality := "Tokyo"
	status, body := env.post(u, map[string]string{"visibility": "friends", "location_kind": "approximate", "locality": locality, "caption": "hello"})
	expectInteraction(t, "first post", status, body, http.StatusCreated, "")
	if status == http.StatusCreated {
		if body["postingWindowId"] != windowID || body["caption"] != "hello" || body["visibility"] != "friends" ||
			body["onTime"] != true || body["lateSeconds"] != float64(0) {
			t.Errorf("first post body: %v", body)
		}
		if loc := body["location"].(map[string]any); loc["kind"] != "approximate" || loc["locality"] != locality || len(loc) != 2 {
			t.Errorf("location: %v", loc)
		}
		author := body["author"].(map[string]any)
		if author["id"] != u.id || author["accountType"] != "regular" || author["profileImageUrl"] != nil {
			t.Errorf("author: %v", author)
		}
		if !strings.HasPrefix(body["imageUrl"].(string), "/api/images/") {
			t.Errorf("imageUrl: %v", body["imageUrl"])
		}
		if s, _ := env.do("GET", body["imageUrl"].(string), "", nil, nil); s != http.StatusOK {
			t.Errorf("image right after post: %d", s)
		}
		if s, me := env.do("GET", "/api/posts/me/current", u.token, nil, nil); s != http.StatusOK || me["id"] != body["id"] {
			t.Errorf("current post: %d %v", s, me)
		}
	}
	// 1日1件。
	status, body = env.post(u, postForm)
	expectInteraction(t, "second post", status, body, http.StatusConflict, "post_slot_unavailable")

	// 削除は2回まで。2回削除したら、その日はもう投稿できない。
	_, first := env.do("GET", "/api/posts/me/current", u.token, nil, nil)
	if s, _ := env.do("DELETE", "/api/posts/"+first["id"].(string), u.token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete 1: %d", s)
	}
	second := env.mustPost(u, map[string]string{"visibility": "public", "location_kind": "exact", "latitude": "35.5", "longitude": "139.25"})
	var loc struct {
		Lat float64 `db:"latitude"`
		Lon float64 `db:"longitude"`
	}
	env.db.Get(&loc, `SELECT latitude, longitude FROM posts WHERE id = $1`, second)
	if loc.Lat != 35.5 || loc.Lon != 139.25 {
		t.Errorf("exact location = %v", loc)
	}
	if s, _ := env.do("DELETE", "/api/posts/"+second, u.token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete 2: %d", s)
	}
	status, body = env.post(u, postForm)
	expectInteraction(t, "post after 2 deletions", status, body, http.StatusConflict, "post_slot_unavailable")

	// 新しい投稿期間（別の日）では、また投稿できる。
	env.mustOpenWindow(admin)
	env.mustPost(u, postForm)

	// 入力の検証は DB より前に行う。
	status, body = env.post(u, map[string]string{"visibility": "secret", "location_kind": "none"})
	expectInteraction(t, "invalid visibility", status, body, http.StatusBadRequest, "invalid_post")

	// 投稿期間が開いていなければ 409。
	other := env.user("asia")
	env.db.MustExec(`UPDATE posting_windows SET ended_at = clock_timestamp() WHERE region = 'asia' AND ended_at IS NULL`)
	status, body = env.post(other, postForm)
	expectInteraction(t, "no open window", status, body, http.StatusConflict, "post_slot_unavailable")
	var images int
	env.db.Get(&images, `SELECT count(*) FROM image_objects WHERE owner_user_id = $1`, other.id)
	if images != 0 {
		t.Errorf("rejected post left %d images", images)
	}
	env.mustOpenWindow(admin)
}

func TestInteractionLatePost(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	lockTestDateMu.Lock()
	lockTestDate = lockTestDate.AddDate(0, 0, 1)
	var latest time.Time
	env.db.Get(&latest, `SELECT coalesce(max(local_date), '2200-01-01') FROM posting_windows WHERE region = 'asia'`)
	if !latest.Before(lockTestDate) {
		lockTestDate = latest.AddDate(0, 0, 1)
	}
	date := lockTestDate.Format(time.DateOnly)
	lockTestDateMu.Unlock()
	if status, body := env.do("POST", "/api/admin/posting-window", admin, map[string]any{"region": "asia", "localDate": date, "onTimeSeconds": 0}, nil); status != http.StatusCreated {
		t.Fatalf("open window: %d %v", status, body)
	}
	u := env.user("asia")
	time.Sleep(1200 * time.Millisecond)
	status, body := env.post(u, postForm)
	if status != http.StatusCreated || body["onTime"] != false || body["lateSeconds"].(float64) < 1 {
		t.Fatalf("late post: %d %v", status, body)
	}
}

// 同じ利用者の同時投稿は、1件だけ受け付けられる。
func TestInteractionConcurrentPostsSameUser(t *testing.T) {
	env := newLockTestEnv(t)
	env.mustOpenWindow(env.adminToken())
	for round := range 3 {
		u := env.user("asia")
		statuses := make([]int, 12)
		parallel(len(statuses), func(i int) { statuses[i], _ = env.post(u, postForm) })
		counts := map[int]int{}
		for _, s := range statuses {
			counts[s]++
		}
		if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != len(statuses)-1 {
			t.Fatalf("round %d: statuses = %v", round, counts)
		}
		var posts, images int
		env.db.Get(&posts, `SELECT count(*) FROM posts WHERE author_id = $1`, u.id)
		env.db.Get(&images, `SELECT count(*) FROM image_objects WHERE owner_user_id = $1`, u.id)
		if posts != 1 || images != 1 {
			t.Fatalf("round %d: posts=%d images=%d", round, posts, images)
		}
	}
}

func TestInteractionRealMojiBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, friend, stranger, lazy, blocker := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.befriend(author, friend)
	friendsPost := env.mustPost(author, map[string]string{"visibility": "friends", "location_kind": "none"})
	for _, u := range []lockTestUser{friend, stranger, blocker} {
		env.mustPost(u, postForm)
	}

	status, body := env.realmoji(friend, friendsPost, "like")
	expectInteraction(t, "friend", status, body, http.StatusCreated, "")
	if status == http.StatusCreated {
		reactor := body["reactor"].(map[string]any)
		if body["reactionType"] != "like" || reactor["id"] != friend.id || reactor["bio"] != "" || body["id"] == nil || body["createdAt"] == nil {
			t.Errorf("realmoji body: %v", body)
		}
		if s, _ := env.do("GET", body["imageUrl"].(string), "", nil, nil); s != http.StatusOK {
			t.Errorf("realmoji image: %d", s)
		}
	}
	// 置き換え：有効な RealMoji は1件だけ。
	status, body = env.realmoji(friend, friendsPost, "heart")
	expectInteraction(t, "replace", status, body, http.StatusCreated, "")
	var active int
	var kind string
	env.db.Get(&active, `SELECT count(*) FROM realmojis WHERE post_id = $1 AND reactor_id = $2 AND replaced_at IS NULL AND deleted_at IS NULL`, friendsPost, friend.id)
	env.db.Get(&kind, `SELECT reaction_type FROM realmojis WHERE post_id = $1 AND reactor_id = $2 AND replaced_at IS NULL AND deleted_at IS NULL`, friendsPost, friend.id)
	if active != 1 || kind != "heart" {
		t.Errorf("active=%d kind=%q", active, kind)
	}

	status, body = env.realmoji(stranger, friendsPost, "like")
	expectInteraction(t, "stranger on friends post", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = env.realmoji(author, friendsPost, "like")
	expectInteraction(t, "own post", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = env.realmoji(friend, "00000000-0000-0000-0000-000000000000", "like")
	expectInteraction(t, "missing post", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = env.realmoji(friend, friendsPost, "angry")
	expectInteraction(t, "invalid type", status, body, http.StatusBadRequest, "invalid_realmoji")

	// 今日まだ投稿していない利用者は見られない。
	_, strangerPublic := env.do("GET", "/api/posts/me/current", stranger.token, nil, nil)
	status, body = env.realmoji(lazy, strangerPublic["id"].(string), "like")
	expectInteraction(t, "viewer has not posted", status, body, http.StatusForbidden, "interaction_forbidden")
	// ブロックしていると見られない。
	if s, _ := env.do("PUT", "/api/blocks/"+stranger.id, blocker.token, nil, nil); s >= 300 {
		t.Fatalf("block: %d", s)
	}
	status, body = env.realmoji(blocker, strangerPublic["id"].(string), "like")
	expectInteraction(t, "blocked", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = env.realmoji(friend, strangerPublic["id"].(string), "smile")
	expectInteraction(t, "public post", status, body, http.StatusCreated, "")

	// 投稿を削除すると 403。
	if s, _ := env.do("DELETE", "/api/posts/"+strangerPublic["id"].(string), stranger.token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete: %d", s)
	}
	status, body = env.realmoji(friend, strangerPublic["id"].(string), "like")
	expectInteraction(t, "deleted post", status, body, http.StatusForbidden, "interaction_forbidden")

	// 投稿者が退会していると 404。
	quitter := env.user("asia")
	quitterPost := env.mustPost(quitter, postForm)
	if s, _ := env.do("POST", "/api/me/account-deletion", quitter.token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("account deletion: %d", s)
	}
	status, body = env.realmoji(friend, quitterPost, "like")
	expectInteraction(t, "author deleted", status, body, http.StatusNotFound, "relationship_not_found")
	status, body = env.comment(friend, quitterPost, "hi")
	expectInteraction(t, "comment on deleted author", status, body, http.StatusForbidden, "interaction_forbidden")

	// 投稿期間が終わった投稿は 403。
	env.mustOpenWindow(admin)
	env.mustPost(friend, postForm)
	status, body = env.realmoji(friend, friendsPost, "like")
	expectInteraction(t, "ended window", status, body, http.StatusForbidden, "interaction_forbidden")

	var rejectedImages int
	env.db.Get(&rejectedImages, `
  SELECT count(*) FROM image_objects i
  WHERE i.owner_user_id = ANY($1)
    AND NOT EXISTS (SELECT 1 FROM posts p WHERE p.image_id = i.id)
    AND NOT EXISTS (SELECT 1 FROM realmojis r WHERE r.image_id = i.id)
 `, userIDs([]lockTestUser{friend, stranger, author, lazy, blocker}))
	if rejectedImages != 0 {
		t.Errorf("%d images left by rejected realmojis", rejectedImages)
	}
}

func TestInteractionCommentBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, friend, fof, stranger, lazy := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.befriend(author, friend)
	env.befriend(friend, fof)
	fofPost := env.mustPost(author, map[string]string{"visibility": "friends_of_friends", "location_kind": "none"})
	for _, u := range []lockTestUser{friend, fof, stranger} {
		env.mustPost(u, postForm)
	}

	status, body := env.comment(friend, fofPost, "nice")
	expectInteraction(t, "friend", status, body, http.StatusCreated, "")
	if status == http.StatusCreated {
		a := body["author"].(map[string]any)
		if body["body"] != "nice" || a["id"] != friend.id || body["id"] == nil || body["createdAt"] == nil || len(body) != 4 {
			t.Errorf("comment body: %v", body)
		}
	}
	// 友達の友達は見られるが、コメントは友達だけ。
	if s, _ := env.do("GET", "/api/posts/"+fofPost, fof.token, nil, nil); s != http.StatusOK {
		t.Fatalf("fof can view: %d", s)
	}
	status, body = env.comment(fof, fofPost, "hi")
	expectInteraction(t, "friend of friend", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = env.comment(stranger, fofPost, "hi")
	expectInteraction(t, "stranger", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = env.comment(author, fofPost, "mine")
	expectInteraction(t, "own post", status, body, http.StatusCreated, "")
	status, body = env.comment(friend, fofPost, "")
	expectInteraction(t, "empty body", status, body, http.StatusBadRequest, "invalid_interaction")
	status, body = env.comment(friend, "00000000-0000-0000-0000-000000000000", "hi")
	expectInteraction(t, "missing post", status, body, http.StatusForbidden, "interaction_forbidden")

	_, strangerPost := env.do("GET", "/api/posts/me/current", stranger.token, nil, nil)
	status, body = env.comment(fof, strangerPost["id"].(string), "public")
	expectInteraction(t, "public post", status, body, http.StatusCreated, "")
	status, body = env.comment(lazy, strangerPost["id"].(string), "hi")
	expectInteraction(t, "viewer has not posted", status, body, http.StatusForbidden, "interaction_forbidden")

	var n int
	env.db.Get(&n, `SELECT count(*) FROM comments WHERE post_id = $1`, fofPost)
	if n != 2 {
		t.Errorf("comments on fof post = %d", n)
	}
	if s, list := env.do("GET", "/api/posts/"+fofPost+"/comments", author.token, nil, nil); s != http.StatusOK || len(list["items"].([]any)) != 2 {
		t.Errorf("list comments: %d %v", s, list)
	}

	// 投稿期間が終わった投稿にはコメントできない（自分の投稿でも）。
	env.mustOpenWindow(admin)
	env.mustPost(author, postForm)
	status, body = env.comment(author, fofPost, "late")
	expectInteraction(t, "ended window", status, body, http.StatusForbidden, "interaction_forbidden")
}
