package isureal

import (
	"bytes"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// #65（プロフィールの閲覧、RealMoji・コメントの一覧）と #66（投稿の編集・削除、RealMoji・コメントの削除）の挙動テスト。
// DB の1往復・関数1回の呼び出しに書き換える前の main でも、書き換えた後でも同じ結果になることを確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql を流した DB を指定したときだけ実行する。

const missingID = "00000000-0000-0000-0000-000000000000"

var relationshipUserKeys = []string{"accountType", "bio", "displayName", "id", "profileImageUrl", "username"}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// rawDo は、JSON でも multipart でもない本文（プロフィール画像）を送る。
func (env *lockTestEnv) rawDo(method, path, token, contentType string, body []byte) int {
	env.t.Helper()
	req, _ := http.NewRequest(method, env.server.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Client-IP", "10.65.66.1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func (env *lockTestEnv) mustStatus(name string, want int, status int, body map[string]any) {
	env.t.Helper()
	if status != want {
		env.t.Fatalf("%s: %d %v", name, status, body)
	}
}

func (env *lockTestEnv) setBio(u lockTestUser, bio string) {
	env.t.Helper()
	status, body := env.do("PATCH", "/api/me", u.token, map[string]string{"bio": bio}, nil)
	env.mustStatus("set bio", http.StatusOK, status, body)
}

func items(body map[string]any) []map[string]any {
	raw, _ := body["items"].([]any)
	list := make([]map[string]any, len(raw))
	for i, item := range raw {
		list[i] = item.(map[string]any)
	}
	return list
}

func itemIDs(body map[string]any) []string {
	ids := []string{}
	for _, item := range items(body) {
		ids = append(ids, item["id"].(string))
	}
	return ids
}

func checkRelationshipUser(t *testing.T, name string, user map[string]any, id, bio string) {
	t.Helper()
	if keys := sortedKeys(user); !slices.Equal(keys, relationshipUserKeys) {
		t.Errorf("%s: user keys = %v", name, keys)
	}
	if user["id"] != id || user["bio"] != bio {
		t.Errorf("%s: user = %v, want id %s bio %q", name, user, id, bio)
	}
}

func TestReadGetUserBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	viewer, target, friend, certified, blocker := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.befriend(viewer, friend)
	for _, u := range []lockTestUser{viewer, target, friend, certified} {
		env.setBio(u, "bio of "+u.id)
	}
	if status, body := env.do("PUT", "/api/admin/users/"+certified.id+"/certification", admin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("certify: %d %v", status, body)
	}
	if status := env.rawDo("PUT", "/api/me/profile-image", target.token, "image/jpeg", env.jpeg); status != http.StatusNoContent {
		t.Fatalf("profile image: %d", status)
	}
	get := func(u lockTestUser, id string) (int, map[string]any) {
		return env.do("GET", "/api/users/"+id, u.token, nil, nil)
	}

	// 友達でも認定公開アカウントでもなければ、自己紹介は空。
	status, body := get(viewer, target.id)
	expectInteraction(t, "stranger", status, body, http.StatusOK, "")
	checkRelationshipUser(t, "stranger", body, target.id, "")
	if body["accountType"] != "regular" || !strings.HasPrefix(body["profileImageUrl"].(string), "/api/images/") || body["username"] == nil || body["displayName"] == nil {
		t.Errorf("stranger body: %v", body)
	}
	// 友達なら自己紹介が見える（友達関係の向きの両方）。
	status, body = get(viewer, friend.id)
	expectInteraction(t, "friend", status, body, http.StatusOK, "")
	checkRelationshipUser(t, "friend", body, friend.id, "bio of "+friend.id)
	if body["profileImageUrl"] != nil {
		t.Errorf("friend profileImageUrl: %v", body["profileImageUrl"])
	}
	status, body = get(friend, viewer.id)
	expectInteraction(t, "friend reverse", status, body, http.StatusOK, "")
	checkRelationshipUser(t, "friend reverse", body, viewer.id, "bio of "+viewer.id)
	// 認定公開アカウントは、友達でなくても自己紹介が見える。
	status, body = get(viewer, certified.id)
	expectInteraction(t, "certified", status, body, http.StatusOK, "")
	checkRelationshipUser(t, "certified", body, certified.id, "bio of "+certified.id)
	if body["accountType"] != "certified_public" {
		t.Errorf("certified accountType: %v", body["accountType"])
	}
	// 自分自身は、認定公開アカウントでなければ自己紹介が空（今の実装のまま）。
	status, body = get(viewer, viewer.id)
	expectInteraction(t, "self", status, body, http.StatusOK, "")
	checkRelationshipUser(t, "self", body, viewer.id, "")
	status, body = get(certified, certified.id)
	expectInteraction(t, "certified self", status, body, http.StatusOK, "")
	checkRelationshipUser(t, "certified self", body, certified.id, "bio of "+certified.id)

	// ブロックはどちらの向きでも 404。
	if status, body := env.do("PUT", "/api/blocks/"+target.id, blocker.token, nil, nil); status >= 300 {
		t.Fatalf("block: %d %v", status, body)
	}
	status, body = get(blocker, target.id)
	expectInteraction(t, "blocker", status, body, http.StatusNotFound, "relationship_not_found")
	status, body = get(target, blocker.id)
	expectInteraction(t, "blocked", status, body, http.StatusNotFound, "relationship_not_found")
	if status, body := env.do("DELETE", "/api/blocks/"+target.id, blocker.token, nil, nil); status >= 300 {
		t.Fatalf("unblock: %d %v", status, body)
	}
	status, body = get(blocker, target.id)
	expectInteraction(t, "after unblock", status, body, http.StatusOK, "")

	status, body = get(viewer, missingID)
	expectInteraction(t, "missing", status, body, http.StatusNotFound, "relationship_not_found")
	quitter := env.user("asia")
	if status, _ := env.do("POST", "/api/me/account-deletion", quitter.token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("account deletion: %d", status)
	}
	status, body = get(viewer, quitter.id)
	expectInteraction(t, "deleted user", status, body, http.StatusNotFound, "relationship_not_found")
}

func TestReadCommentListBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, friend, fof, stranger, lazy, quitter := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.befriend(author, friend)
	env.befriend(friend, fof)
	env.befriend(author, quitter)
	env.setBio(friend, "friend bio")
	post := env.mustPost(author, map[string]string{"visibility": "friends_of_friends", "location_kind": "none"})
	for _, u := range []lockTestUser{friend, fof, stranger, quitter} {
		env.mustPost(u, postForm)
	}
	mustComment := func(u lockTestUser, postID, text string) string {
		t.Helper()
		status, body := env.comment(u, postID, text)
		env.mustStatus("comment", http.StatusCreated, status, body)
		return body["id"].(string)
	}
	c1 := mustComment(friend, post, "c1")
	c2 := mustComment(author, post, "c2")
	mustComment(quitter, post, "c3")
	c4 := mustComment(friend, post, "c4")
	// 削除したコメントと、退会した利用者のコメントは含めない。
	env.db.MustExec(`UPDATE comments SET deleted_at = clock_timestamp() WHERE id = $1`, c4)
	if status, _ := env.do("POST", "/api/me/account-deletion", quitter.token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("account deletion: %d", status)
	}
	list := func(u lockTestUser, postID, query string) (int, map[string]any) {
		return env.do("GET", "/api/posts/"+postID+"/comments"+query, u.token, nil, nil)
	}

	status, body := list(author, post, "")
	expectInteraction(t, "author", status, body, http.StatusOK, "")
	if ids := itemIDs(body); !slices.Equal(ids, []string{c1, c2}) || body["hasMore"] != false || len(body) != 2 {
		t.Errorf("author list: %v", body)
	}
	if got := items(body); len(got) == 2 {
		if keys := sortedKeys(got[0]); !slices.Equal(keys, []string{"author", "body", "createdAt", "id"}) || got[0]["body"] != "c1" || got[0]["createdAt"] == nil {
			t.Errorf("comment item: %v", got[0])
		}
		// コメントの作者の自己紹介は、そのまま返す（今の実装のまま）。
		checkRelationshipUser(t, "comment author", got[0]["author"].(map[string]any), friend.id, "friend bio")
		checkRelationshipUser(t, "comment author 2", got[1]["author"].(map[string]any), author.id, "")
	}
	status, body = list(friend, post, "")
	expectInteraction(t, "friend", status, body, http.StatusOK, "")
	// 友達の友達は投稿を見られるが、コメントの一覧は見られない（コメントの権限で判定する）。
	if s, _ := env.do("GET", "/api/posts/"+post, fof.token, nil, nil); s != http.StatusOK {
		t.Fatalf("fof can view: %d", s)
	}
	status, body = list(fof, post, "")
	expectInteraction(t, "friend of friend", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = list(stranger, post, "")
	expectInteraction(t, "stranger", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = list(lazy, post, "")
	expectInteraction(t, "viewer has not posted", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = list(author, missingID, "")
	expectInteraction(t, "missing post", status, body, http.StatusForbidden, "interaction_forbidden")

	// ページング：afterCreatedAt と afterId より後のもの。
	_, first := list(author, post, "")
	if got := items(first); len(got) == 2 {
		query := "?afterCreatedAt=" + url.QueryEscape(got[0]["createdAt"].(string)) + "&afterId=" + got[0]["id"].(string)
		status, body = list(author, post, query)
		expectInteraction(t, "after first", status, body, http.StatusOK, "")
		if ids := itemIDs(body); !slices.Equal(ids, []string{c2}) {
			t.Errorf("after first: %v", ids)
		}
	}
	status, body = list(author, post, "?afterId="+c1)
	expectInteraction(t, "afterId only", status, body, http.StatusBadRequest, "invalid_pagination")
	// ページングの検証は、権限の確認より前に行う。
	status, body = list(stranger, post, "?afterId="+c1)
	expectInteraction(t, "afterId only by stranger", status, body, http.StatusBadRequest, "invalid_pagination")

	// 51件以上あれば hasMore。
	_, current := env.do("GET", "/api/posts/me/current", stranger.token, nil, nil)
	public := current["id"].(string)
	for i := range 51 {
		mustComment(friend, public, "many "+string(rune('a'+i%26)))
	}
	status, body = list(friend, public, "")
	expectInteraction(t, "many", status, body, http.StatusOK, "")
	if got := items(body); len(got) != 50 || body["hasMore"] != true {
		t.Errorf("many: %d items, hasMore %v", len(got), body["hasMore"])
	} else {
		last := got[49]
		query := "?afterCreatedAt=" + url.QueryEscape(last["createdAt"].(string)) + "&afterId=" + last["id"].(string)
		status, body = list(friend, public, query)
		if status != http.StatusOK || len(items(body)) != 1 || body["hasMore"] != false {
			t.Errorf("many page 2: %d %v", status, body)
		}
	}

	// 投稿期間が終わった投稿のコメントは、自分の投稿でも見られない。
	env.mustOpenWindow(admin)
	env.mustPost(author, postForm)
	env.mustPost(friend, postForm)
	status, body = list(author, post, "")
	expectInteraction(t, "ended window author", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = list(friend, post, "")
	expectInteraction(t, "ended window friend", status, body, http.StatusForbidden, "interaction_forbidden")
}

func TestReadRealMojiListBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, friend, fof, stranger, lazy, blocker := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")
	env.befriend(author, friend)
	env.befriend(friend, fof)
	env.setBio(friend, "friend bio")
	post := env.mustPost(author, map[string]string{"visibility": "friends_of_friends", "location_kind": "none"})
	for _, u := range []lockTestUser{friend, fof, stranger, blocker} {
		env.mustPost(u, postForm)
	}
	mustReact := func(u lockTestUser, postID, kind string) string {
		t.Helper()
		status, body := env.realmoji(u, postID, kind)
		env.mustStatus("realmoji", http.StatusCreated, status, body)
		return body["id"].(string)
	}
	mustReact(friend, post, "like")
	heart := mustReact(friend, post, "heart") // like は置き換えられる
	smile := mustReact(fof, post, "smile")
	list := func(u lockTestUser, postID, query string) (int, map[string]any) {
		return env.do("GET", "/api/posts/"+postID+"/realmojis"+query, u.token, nil, nil)
	}

	status, body := list(author, post, "")
	expectInteraction(t, "author", status, body, http.StatusOK, "")
	if ids := itemIDs(body); !slices.Equal(ids, []string{smile, heart}) || body["hasMore"] != false || len(body) != 2 {
		t.Errorf("author list: %v", body)
	}
	if got := items(body); len(got) == 2 {
		if keys := sortedKeys(got[1]); !slices.Equal(keys, []string{"createdAt", "id", "imageUrl", "reactionType", "reactor"}) ||
			got[1]["reactionType"] != "heart" || !strings.HasPrefix(got[1]["imageUrl"].(string), "/api/images/") {
			t.Errorf("realmoji item: %v", got[1])
		}
		checkRelationshipUser(t, "reactor", got[1]["reactor"].(map[string]any), friend.id, "friend bio")
	}
	// 一覧は閲覧権限で判定する。友達の友達も見られる。
	for name, u := range map[string]lockTestUser{"friend": friend, "fof": fof} {
		status, body = list(u, post, "")
		expectInteraction(t, name, status, body, http.StatusOK, "")
	}
	status, body = list(stranger, post, "")
	expectInteraction(t, "stranger", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = list(lazy, post, "")
	expectInteraction(t, "viewer has not posted", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = list(author, missingID, "")
	expectInteraction(t, "missing post", status, body, http.StatusForbidden, "interaction_forbidden")

	// ページング：beforeCreatedAt と beforeId より前のもの。
	_, first := list(author, post, "")
	if got := items(first); len(got) == 2 {
		query := "?beforeCreatedAt=" + url.QueryEscape(got[0]["createdAt"].(string)) + "&beforeId=" + got[0]["id"].(string)
		status, body = list(author, post, query)
		expectInteraction(t, "before first", status, body, http.StatusOK, "")
		if ids := itemIDs(body); !slices.Equal(ids, []string{heart}) {
			t.Errorf("before first: %v", ids)
		}
	}
	status, body = list(author, post, "?beforeId="+heart)
	expectInteraction(t, "beforeId only", status, body, http.StatusBadRequest, "invalid_pagination")
	status, body = list(stranger, post, "?beforeId="+heart)
	expectInteraction(t, "beforeId only by stranger", status, body, http.StatusBadRequest, "invalid_pagination")

	// ブロックすると見られない。
	_, strangerPost := env.do("GET", "/api/posts/me/current", stranger.token, nil, nil)
	publicPost := strangerPost["id"].(string)
	status, body = list(blocker, publicPost, "")
	expectInteraction(t, "public before block", status, body, http.StatusOK, "")
	if status, body := env.do("PUT", "/api/blocks/"+stranger.id, blocker.token, nil, nil); status >= 300 {
		t.Fatalf("block: %d %v", status, body)
	}
	status, body = list(blocker, publicPost, "")
	expectInteraction(t, "blocked", status, body, http.StatusForbidden, "interaction_forbidden")

	// 削除した RealMoji と、退会した利用者の RealMoji は含めない。
	env.db.MustExec(`UPDATE realmojis SET deleted_at = clock_timestamp() WHERE id = $1`, heart)
	if status, _ := env.do("POST", "/api/me/account-deletion", fof.token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("account deletion: %d", status)
	}
	status, body = list(author, post, "")
	if status != http.StatusOK || len(items(body)) != 0 || body["hasMore"] != false {
		t.Errorf("after deletions: %d %v", status, body)
	}

	// 投稿期間が終わっても、自分の投稿は見られる。ほかの人は見られない。
	mustReact(friend, post, "laugh")
	env.mustOpenWindow(admin)
	env.mustPost(author, postForm)
	env.mustPost(friend, postForm)
	status, body = list(author, post, "")
	expectInteraction(t, "ended window author", status, body, http.StatusOK, "")
	if len(items(body)) != 1 {
		t.Errorf("ended window author: %v", body)
	}
	status, body = list(friend, post, "")
	expectInteraction(t, "ended window friend", status, body, http.StatusForbidden, "interaction_forbidden")
}

func TestEditPostBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, other := env.user("asia"), env.user("asia")
	env.setBio(author, "author bio")
	post := env.mustPost(author, map[string]string{"visibility": "public", "location_kind": "approximate", "locality": "Osaka", "caption": "before"})
	_, before := env.do("GET", "/api/posts/"+post, author.token, nil, nil)
	patch := func(u lockTestUser, id string, body any) (int, map[string]any) {
		return env.do("PATCH", "/api/posts/"+id, u.token, body, nil)
	}
	// 変えた項目以外は、投稿の取得と同じ応答になる（署名付き URL は exp が変わりうるのでパスだけ比べる）。
	sameExcept := func(name string, body map[string]any, changed ...string) {
		t.Helper()
		if keys := sortedKeys(body); !slices.Equal(keys, sortedKeys(before)) {
			t.Errorf("%s: keys = %v, want %v", name, keys, sortedKeys(before))
		}
		for _, key := range sortedKeys(before) {
			if slices.Contains(changed, key) {
				continue
			}
			switch key {
			case "imageUrl":
				if strings.Split(body[key].(string), "?")[0] != strings.Split(before[key].(string), "?")[0] {
					t.Errorf("%s: imageUrl = %v", name, body[key])
				}
			case "author", "location":
				if !maps.Equal(body[key].(map[string]any), before[key].(map[string]any)) {
					t.Errorf("%s: %s = %v, want %v", name, key, body[key], before[key])
				}
			default:
				if body[key] != before[key] {
					t.Errorf("%s: %s = %v, want %v", name, key, body[key], before[key])
				}
			}
		}
	}

	status, body := patch(author, post, map[string]any{"caption": "after"})
	expectInteraction(t, "caption", status, body, http.StatusOK, "")
	sameExcept("caption", body, "caption")
	if body["caption"] != "after" {
		t.Errorf("caption: %v", body["caption"])
	}
	status, body = patch(author, post, map[string]any{"visibility": "friends"})
	expectInteraction(t, "visibility", status, body, http.StatusOK, "")
	sameExcept("visibility", body, "caption", "visibility")
	if body["caption"] != "after" || body["visibility"] != "friends" {
		t.Errorf("visibility: %v", body)
	}
	status, body = patch(author, post, map[string]any{"caption": "", "visibility": "friends_of_friends"})
	expectInteraction(t, "both", status, body, http.StatusOK, "")
	if body["caption"] != "" || body["visibility"] != "friends_of_friends" {
		t.Errorf("both: %v", body)
	}
	_, got := env.do("GET", "/api/posts/"+post, author.token, nil, nil)
	if got["caption"] != "" || got["visibility"] != "friends_of_friends" {
		t.Errorf("get after edit: %v", got)
	}

	status, body = patch(author, post, map[string]any{})
	expectInteraction(t, "neither", status, body, http.StatusBadRequest, "invalid_post")
	status, body = patch(author, post, map[string]any{"visibility": "secret"})
	expectInteraction(t, "invalid visibility", status, body, http.StatusBadRequest, "invalid_post")
	status, body = patch(author, post, map[string]any{"caption": strings.Repeat("あ", 1001)})
	expectInteraction(t, "long caption", status, body, http.StatusBadRequest, "invalid_post")
	status, body = patch(other, post, map[string]any{"caption": "hijack"})
	expectInteraction(t, "other", status, body, http.StatusForbidden, "post_not_editable")
	status, body = patch(author, missingID, map[string]any{"caption": "x"})
	expectInteraction(t, "missing", status, body, http.StatusNotFound, "post_not_found")
	var caption string
	env.db.Get(&caption, `SELECT caption FROM posts WHERE id = $1`, post)
	if caption != "" {
		t.Errorf("caption after rejected edits = %q", caption)
	}

	// 投稿期間が終わった投稿は編集できない。
	env.mustOpenWindow(admin)
	status, body = patch(author, post, map[string]any{"caption": "late"})
	expectInteraction(t, "ended window", status, body, http.StatusForbidden, "post_not_editable")
	// 削除した投稿は 404。
	current := env.mustPost(author, postForm)
	if s, _ := env.do("DELETE", "/api/posts/"+current, author.token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete: %d", s)
	}
	status, body = patch(author, current, map[string]any{"caption": "gone"})
	expectInteraction(t, "deleted", status, body, http.StatusNotFound, "post_not_found")
}

func TestDeletePostBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, other := env.user("asia"), env.user("asia")
	post := env.mustPost(author, postForm)
	otherPost := env.mustPost(other, postForm)
	del := func(u lockTestUser, id string) (int, map[string]any) {
		return env.do("DELETE", "/api/posts/"+id, u.token, nil, nil)
	}

	status, body := del(other, post)
	expectInteraction(t, "other", status, body, http.StatusForbidden, "post_not_editable")
	status, body = del(author, missingID)
	expectInteraction(t, "missing", status, body, http.StatusNotFound, "post_not_found")
	status, body = del(author, post)
	expectInteraction(t, "delete", status, body, http.StatusNoContent, "")
	if body != nil {
		t.Errorf("delete body: %v", body)
	}
	status, body = del(author, post)
	expectInteraction(t, "delete again", status, body, http.StatusNotFound, "post_not_found")
	if s, _ := env.do("GET", "/api/posts/"+post, author.token, nil, nil); s != http.StatusNotFound {
		t.Errorf("get deleted post: %d", s)
	}

	// その日の削除が2回に達していたら 409 で、投稿は消えない。
	second := env.mustPost(author, postForm)
	env.db.MustExec(`
  INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at, deleted_at)
  SELECT gen_random_uuid()::text, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at, clock_timestamp()
  FROM posts WHERE id = $1
 `, second)
	status, body = del(author, second)
	expectInteraction(t, "third deletion", status, body, http.StatusConflict, "post_slot_unavailable")
	var deleted bool
	env.db.Get(&deleted, `SELECT deleted_at IS NOT NULL FROM posts WHERE id = $1`, second)
	if deleted {
		t.Errorf("post deleted despite 409")
	}

	// 投稿期間が終わった投稿も削除できる（今の実装のまま）。
	env.mustOpenWindow(admin)
	status, body = del(other, otherPost)
	expectInteraction(t, "ended window", status, body, http.StatusNoContent, "")
}

func TestDeleteCommentAndRealMojiBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, friend, stranger := env.user("asia"), env.user("asia"), env.user("asia")
	env.befriend(author, friend)
	post := env.mustPost(author, postForm)
	env.mustPost(friend, postForm)
	strangerPost := env.mustPost(stranger, postForm)
	mustComment := func(u lockTestUser, postID, text string) string {
		t.Helper()
		status, body := env.comment(u, postID, text)
		env.mustStatus("comment", http.StatusCreated, status, body)
		return body["id"].(string)
	}
	mustReact := func(u lockTestUser, postID, kind string) string {
		t.Helper()
		status, body := env.realmoji(u, postID, kind)
		env.mustStatus("realmoji", http.StatusCreated, status, body)
		return body["id"].(string)
	}
	delC := func(u lockTestUser, id string) (int, map[string]any) {
		return env.do("DELETE", "/api/comments/"+id, u.token, nil, nil)
	}
	delR := func(u lockTestUser, id string) (int, map[string]any) {
		return env.do("DELETE", "/api/realmojis/"+id, u.token, nil, nil)
	}

	c1, c2, c3 := mustComment(friend, post, "c1"), mustComment(friend, post, "c2"), mustComment(stranger, post, "c3")
	status, body := delC(stranger, c1)
	expectInteraction(t, "comment by third party", status, body, http.StatusForbidden, "interaction_forbidden")
	status, body = delC(friend, c1)
	expectInteraction(t, "comment by its author", status, body, http.StatusNoContent, "")
	status, body = delC(friend, c1)
	expectInteraction(t, "comment again", status, body, http.StatusNotFound, "interaction_not_found")
	status, body = delC(author, c2)
	expectInteraction(t, "comment by post author", status, body, http.StatusNoContent, "")
	status, body = delC(friend, missingID)
	expectInteraction(t, "missing comment", status, body, http.StatusNotFound, "interaction_not_found")
	status, body = delC(stranger, c3)
	expectInteraction(t, "own comment on others post", status, body, http.StatusNoContent, "")
	var remaining int
	env.db.Get(&remaining, `SELECT count(*) FROM comments WHERE post_id = $1 AND deleted_at IS NULL`, post)
	if remaining != 0 {
		t.Errorf("remaining comments = %d", remaining)
	}
	if s, list := env.do("GET", "/api/posts/"+post+"/comments", author.token, nil, nil); s != http.StatusOK || len(items(list)) != 0 {
		t.Errorf("list after deletes: %d %v", s, list)
	}

	like := mustReact(friend, post, "like")
	heart := mustReact(friend, post, "heart") // like は置き換えられる
	smile := mustReact(stranger, post, "smile")
	status, body = delR(stranger, heart)
	expectInteraction(t, "realmoji by third party", status, body, http.StatusForbidden, "interaction_forbidden")
	// 置き換えられた RealMoji も、削除されていなければ削除できる（今の実装のまま）。
	status, body = delR(friend, like)
	expectInteraction(t, "replaced realmoji", status, body, http.StatusNoContent, "")
	status, body = delR(friend, heart)
	expectInteraction(t, "realmoji by reactor", status, body, http.StatusNoContent, "")
	status, body = delR(friend, heart)
	expectInteraction(t, "realmoji again", status, body, http.StatusNotFound, "interaction_not_found")
	status, body = delR(author, smile)
	expectInteraction(t, "realmoji by post author", status, body, http.StatusNoContent, "")
	status, body = delR(friend, missingID)
	expectInteraction(t, "missing realmoji", status, body, http.StatusNotFound, "interaction_not_found")
	if s, list := env.do("GET", "/api/posts/"+post+"/realmojis", author.token, nil, nil); s != http.StatusOK || len(items(list)) != 0 {
		t.Errorf("realmoji list after deletes: %d %v", s, list)
	}

	// 投稿を削除した後でも、投稿期間が終わった後でも、コメントと RealMoji は削除できる（今の実装のまま）。
	onDeleted := mustComment(friend, strangerPost, "on deleted post")
	reactOnDeleted := mustReact(friend, strangerPost, "like")
	ended := mustComment(friend, post, "ended")
	reactEnded := mustReact(stranger, post, "laugh")
	if s, _ := env.do("DELETE", "/api/posts/"+strangerPost, stranger.token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete post: %d", s)
	}
	status, body = delC(friend, onDeleted)
	expectInteraction(t, "comment on deleted post", status, body, http.StatusNoContent, "")
	status, body = delR(stranger, reactOnDeleted)
	expectInteraction(t, "realmoji on deleted post by its author", status, body, http.StatusNoContent, "")
	env.mustOpenWindow(admin)
	status, body = delC(friend, ended)
	expectInteraction(t, "comment in ended window", status, body, http.StatusNoContent, "")
	status, body = delR(author, reactEnded)
	expectInteraction(t, "realmoji in ended window", status, body, http.StatusNoContent, "")
}

// 同じものを同時に削除すると、1件だけ成功し、残りは 404 になる（500 やデッドロックにならない）。
func TestConcurrentDeletes(t *testing.T) {
	env := newLockTestEnv(t)
	env.mustOpenWindow(env.adminToken())
	author, friend := env.user("asia"), env.user("asia")
	env.befriend(author, friend)
	post := env.mustPost(author, postForm)
	env.mustPost(friend, postForm)
	expectOne := func(name string, statuses []int, notFound int) {
		t.Helper()
		counts := map[int]int{}
		for _, s := range statuses {
			counts[s]++
		}
		if counts[http.StatusNoContent] != 1 || counts[notFound] != len(statuses)-1 {
			t.Errorf("%s: statuses = %v", name, counts)
		}
	}
	for round := range 3 {
		_, c := env.comment(friend, post, "race")
		_, r := env.realmoji(friend, post, "like")
		comment, reaction := c["id"].(string), r["id"].(string)
		cs, rs := make([]int, 8), make([]int, 8)
		parallel(16, func(i int) {
			// 半分はコメント・RealMoji の作者、半分は投稿者が消す。
			u := friend
			if i%2 == 0 {
				u = author
			}
			if i < 8 {
				cs[i], _ = env.do("DELETE", "/api/comments/"+comment, u.token, nil, nil)
			} else {
				rs[i-8], _ = env.do("DELETE", "/api/realmojis/"+reaction, u.token, nil, nil)
			}
		})
		expectOne("comment round "+string(rune('0'+round)), cs, http.StatusNotFound)
		expectOne("realmoji round "+string(rune('0'+round)), rs, http.StatusNotFound)
	}
	ps := make([]int, 8)
	parallel(len(ps), func(i int) { ps[i], _ = env.do("DELETE", "/api/posts/"+post, author.token, nil, nil) })
	expectOne("post", ps, http.StatusNotFound)
}
