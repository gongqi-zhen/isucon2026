package isureal

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// #64 の無効化の一括検知を、実際の PostgreSQL と HTTP ハンドラで確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql、image_invalidation.sql を流した DB を
// 指定したときだけ実行する（データは追加される）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55464/isureal?sslmode=disable' go test -run 'TestImageInvalidationDB|TestImageValidityDB' -v ./...

func newImageInvalidationEnv(t *testing.T) *lockTestEnv {
	t.Helper()
	env := newLockTestEnv(t)
	var exists bool
	if err := env.db.Get(&exists, `SELECT to_regclass('image_invalidation_epoch') IS NOT NULL`); err != nil || !exists {
		t.Skip("image_invalidation.sql is not applied")
	}
	return env
}

// 前回のスナップショットの xmin 以上の xid を読むので、読み込みの時点で実行中だったトランザクションの無効化も、
// コミットした後の読み込みで読める。
func TestImageInvalidationDBSnapshotChain(t *testing.T) {
	env := newImageInvalidationEnv(t)
	ctx := context.Background()
	source := dbImageInvalidationSource{db: env.db}
	contains := func(result imageInvalidationPoll, id string) bool {
		for _, row := range result.Rows {
			if row.ImageID == id {
				return true
			}
		}
		return false
	}
	first, err := source.poll(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Epoch == "" || first.XMin == "" || len(first.Rows) != 0 {
		t.Fatalf("first poll = %+v", first)
	}

	a, b := "chain-a-"+uuid.NewString(), "chain-b-"+uuid.NewString()
	txA, err := env.db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()
	txA.MustExec(`INSERT INTO image_invalidations (image_id) VALUES ($1)`, a)

	poll1, err := source.poll(ctx, first.XMin)
	if err != nil {
		t.Fatal(err)
	}
	if contains(poll1, a) {
		t.Fatal("uncommitted invalidation must not be visible")
	}
	env.db.MustExec(`INSERT INTO image_invalidations (image_id) VALUES ($1)`, b)
	poll2, err := source.poll(ctx, poll1.XMin)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(poll2, b) || contains(poll2, a) {
		t.Fatalf("poll2 should contain only the committed row: b=%v a=%v", contains(poll2, b), contains(poll2, a))
	}
	// A は poll1 と poll2 の時点で実行中だった。コミットした後、poll2 の xmin から読めば必ず読める。
	if err := txA.Commit(); err != nil {
		t.Fatal(err)
	}
	poll3, err := source.poll(ctx, poll2.XMin)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(poll3, a) {
		t.Fatalf("invalidation committed after an in-progress snapshot must be read: xmin1=%s xmin2=%s xmin3=%s", poll1.XMin, poll2.XMin, poll3.XMin)
	}
}

func imageIDFromURL(t *testing.T, imageURL string) string {
	t.Helper()
	parsed, err := url.Parse(imageURL)
	if err != nil || !strings.HasPrefix(parsed.Path, "/api/images/") {
		t.Fatalf("image url %q", imageURL)
	}
	return strings.TrimPrefix(parsed.Path, "/api/images/")
}

func imageStatus(env *lockTestEnv, imageURL string) int {
	status, _ := env.do("GET", imageURL, "", nil, nil)
	return status
}

// putProfileImage はプロフィール画像を設定し、その URL を返す。
func putProfileImage(env *lockTestEnv, u lockTestUser) string {
	env.t.Helper()
	req, _ := http.NewRequest("PUT", env.server.URL+"/api/me/profile-image", bytes.NewReader(env.jpeg))
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("X-Client-IP", "10.64.0."+uuid.NewString()[:2])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		env.t.Fatalf("put profile image: %d", res.StatusCode)
	}
	_, me := env.do("GET", "/api/me", u.token, nil, nil)
	profileURL, _ := me["profileImageUrl"].(string)
	if profileURL == "" {
		env.t.Fatalf("profile image url missing: %v", me)
	}
	return profileURL
}

func (env *lockTestEnv) mustDo(method, path, token string, want int) {
	env.t.Helper()
	if status, body := env.do(method, path, token, nil, nil); status != want {
		env.t.Fatalf("%s %s: %d %v", method, path, status, body)
	}
}

// imageScenario は、いろいろな状態の画像を作る。
type imageScenario struct {
	valid   map[string]string // 有効な画像の id -> 持ち主
	invalid map[string]string // 無効な画像の id -> 持ち主
	rows    map[string]bool   // 作られるはずの無効化の記録（"image:id"、"user:id"、"post:id"）
}

func buildImageScenario(env *lockTestEnv) imageScenario {
	t := env.t
	t.Helper()
	s := imageScenario{valid: map[string]string{}, invalid: map[string]string{}, rows: map[string]bool{}}
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, reactor, leaver, remover := env.user("asia"), env.user("asia"), env.user("asia"), env.user("asia")

	_, authorPost := env.post(author, postForm)
	authorPostID, authorPostImage := authorPost["id"].(string), imageIDFromURL(t, authorPost["imageUrl"].(string))
	_, reactorPost := env.post(reactor, postForm)
	_, leaverPost := env.post(leaver, postForm)
	leaverPostID, leaverPostImage := leaverPost["id"].(string), imageIDFromURL(t, leaverPost["imageUrl"].(string))
	_, removerPost := env.post(remover, postForm)
	removerPostID, removerPostImage := removerPost["id"].(string), imageIDFromURL(t, removerPost["imageUrl"].(string))
	s.valid[imageIDFromURL(t, reactorPost["imageUrl"].(string))] = reactor.id

	// RealMoji：置き換えた古いもの、削除したもの、今のもの。
	_, first := env.realmoji(reactor, authorPostID, "like")
	_, current := env.realmoji(reactor, authorPostID, "heart")
	s.invalid[imageIDFromURL(t, first["imageUrl"].(string))] = reactor.id
	s.rows["image:"+imageIDFromURL(t, first["imageUrl"].(string))] = true
	s.valid[imageIDFromURL(t, current["imageUrl"].(string))] = reactor.id
	_, deleted := env.realmoji(reactor, removerPostID, "smile")
	env.mustDo("DELETE", "/api/realmojis/"+deleted["id"].(string), reactor.token, http.StatusNoContent)
	s.invalid[imageIDFromURL(t, deleted["imageUrl"].(string))] = reactor.id
	s.rows["image:"+imageIDFromURL(t, deleted["imageUrl"].(string))] = true

	// 削除した投稿の画像と、その投稿への RealMoji の画像。
	_, onRemoved := env.realmoji(reactor, removerPostID, "laugh")
	env.mustDo("DELETE", "/api/posts/"+removerPostID, remover.token, http.StatusNoContent)
	s.invalid[removerPostImage] = remover.id
	s.invalid[imageIDFromURL(t, onRemoved["imageUrl"].(string))] = reactor.id
	s.rows["post:"+removerPostID] = true

	// 退会した利用者の画像と、その利用者の投稿への RealMoji の画像。
	_, onLeaver := env.realmoji(author, leaverPostID, "surprise")
	leaverProfile := imageIDFromURL(t, putProfileImage(env, leaver))
	env.mustDo("POST", "/api/me/account-deletion", leaver.token, http.StatusNoContent)
	s.invalid[leaverPostImage] = leaver.id
	s.invalid[leaverProfile] = leaver.id
	s.invalid[imageIDFromURL(t, onLeaver["imageUrl"].(string))] = author.id
	s.rows["user:"+leaver.id] = true

	// プロフィール画像：差し替えた古いもの、今のもの、削除したもの。
	oldProfile := imageIDFromURL(t, putProfileImage(env, author))
	newProfile := imageIDFromURL(t, putProfileImage(env, author))
	s.invalid[oldProfile] = author.id
	s.valid[newProfile] = author.id
	s.rows["image:"+oldProfile] = true
	removedProfile := imageIDFromURL(t, putProfileImage(env, reactor))
	env.mustDo("DELETE", "/api/me/profile-image", reactor.token, http.StatusNoContent)
	s.invalid[removedProfile] = reactor.id
	s.rows["image:"+removedProfile] = true

	s.valid[authorPostImage] = author.id
	s.invalid[uuid.NewString()] = ""
	return s
}

// トリガが、画像を無効にする変更をすべて記録する。
func TestImageInvalidationDBTriggers(t *testing.T) {
	env := newImageInvalidationEnv(t)
	s := buildImageScenario(env)
	for key := range s.rows {
		kind, id, _ := strings.Cut(key, ":")
		var n int
		if err := env.db.Get(&n, `SELECT count(*) FROM image_invalidations WHERE `+kind+`_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("no invalidation recorded for %s", key)
		}
	}
	// 有効な画像について、無効化を記録していない（投稿と RealMoji の作成、ほかの更新では記録しない）。
	for id := range s.valid {
		var n int
		env.db.Get(&n, `SELECT count(*) FROM image_invalidations WHERE image_id = $1`, id)
		if n != 0 {
			t.Errorf("valid image %s has %d invalidations", id, n)
		}
	}
}

// 元の validImageFrom（変更前の image_api.go）。新しいクエリと結果を比べるために残す。
const legacyValidImageFrom = `
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
   WHERE i.id = $1 AND (
     u.profile_image_id = i.id
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   )
`

// 新しい有効性のクエリ（持ち主が分かる場合と分からない場合）と、画像の中身のクエリが、元のクエリと同じ判定になる。
func TestImageValidityDBQueriesMatchLegacy(t *testing.T) {
	env := newImageInvalidationEnv(t)
	s := buildImageScenario(env)
	ctx := context.Background()
	for _, set := range []map[string]string{s.valid, s.invalid} {
		for id, owner := range set {
			var legacy bool
			if err := env.db.Get(&legacy, `SELECT EXISTS(SELECT 1 `+legacyValidImageFrom+`)`, id); err != nil {
				t.Fatal(err)
			}
			_, wantValid := s.valid[id]
			if legacy != wantValid {
				t.Errorf("%s: legacy = %v, scenario says %v", id, legacy, wantValid)
			}
			for _, ownerID := range []string{owner, ""} {
				_, valid, err := checkImageValidity(ctx, env.db, id, ownerID)
				if err != nil {
					t.Fatal(err)
				}
				if valid != legacy {
					t.Errorf("%s (owner %q): checkImageValidity = %v, legacy = %v", id, ownerID, valid, legacy)
				}
			}
			row, err := loadValidImageObject(ctx, env.db, id)
			if (err == nil) != legacy {
				t.Errorf("%s: loadValidImageObject err = %v, legacy = %v", id, err, legacy)
			}
			if err == nil && (len(row.Data) == 0 || row.MediaType != "image/jpeg" || row.OwnerID != owner) {
				t.Errorf("%s: loaded = %d bytes %q owner %q", id, len(row.Data), row.MediaType, row.OwnerID)
			}
		}
	}
}

// waitTrusted は、画像の取得を繰り返して、一括検知で覚えられる（DB を引かずに返せる）まで待つ。
func waitTrusted(env *lockTestEnv, imageURL string) {
	env.t.Helper()
	id := imageIDFromURL(env.t, imageURL)
	deadline := time.Now().Add(10 * time.Second)
	for !imageValidity.valid(id) {
		if time.Now().After(deadline) {
			env.t.Fatalf("image %s was never trusted", id)
		}
		if status := imageStatus(env, imageURL); status != http.StatusOK {
			env.t.Fatalf("image %s: %d before invalidation", id, status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForbidden は、取得が 403 になるまでの時間を返す。5秒を超えたら失敗にする。
func waitForbidden(env *lockTestEnv, imageURL string, since time.Time) time.Duration {
	env.t.Helper()
	for {
		if imageStatus(env, imageURL) == http.StatusForbidden {
			return time.Since(since)
		}
		if time.Since(since) > 5*time.Second {
			env.t.Fatalf("image %s is still served %v after the deletion", imageURL, time.Since(since))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// HTTP の削除の後、一括検知で覚えていた画像も 5秒より十分早く 403 になる。
func TestImageInvalidationDBForbiddenAfterDeletion(t *testing.T) {
	env := newImageInvalidationEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author, reactor := env.user("asia"), env.user("asia")
	_, post := env.post(author, postForm)
	env.mustPost(reactor, postForm)
	_, first := env.realmoji(reactor, post["id"].(string), "like")
	profile := putProfileImage(env, author)

	// アップロードした画像は imageValidityTTL の間だけ有効とみなす。それを過ぎてから、DB で確かめて一括検知で覚える。
	time.Sleep(imageValidityTTL + 100*time.Millisecond)
	for _, imageURL := range []string{post["imageUrl"].(string), first["imageUrl"].(string), profile} {
		waitTrusted(env, imageURL)
	}
	// 覚えた後は、TTL を過ぎても DB を引かずに返す。
	time.Sleep(imageValidityTTL + 100*time.Millisecond)
	postImage := imageIDFromURL(t, post["imageUrl"].(string))
	if _, ttl := imageValidityCache.Get(postImage); ttl || !imageValidity.valid(postImage) {
		t.Fatalf("expected the post image to be served from the invalidation tracker: ttl=%v tracked=%v", ttl, imageValidity.valid(postImage))
	}
	if status := imageStatus(env, post["imageUrl"].(string)); status != http.StatusOK {
		t.Fatalf("tracked image: %d", status)
	}

	// RealMoji の置き換え。
	start := time.Now()
	env.realmoji(reactor, post["id"].(string), "heart")
	t.Logf("replaced realmoji image forbidden after %v", waitForbidden(env, first["imageUrl"].(string), start))
	// プロフィール画像の差し替え。
	start = time.Now()
	putProfileImage(env, author)
	t.Logf("replaced profile image forbidden after %v", waitForbidden(env, profile, start))
	// 投稿の削除。
	start = time.Now()
	env.mustDo("DELETE", "/api/posts/"+post["id"].(string), author.token, http.StatusNoContent)
	t.Logf("deleted post image forbidden after %v", waitForbidden(env, post["imageUrl"].(string), start))
}

// 退会すると、その利用者の画像と、その利用者の投稿への RealMoji の画像が 403 になる。
func TestImageInvalidationDBForbiddenAfterAccountDeletion(t *testing.T) {
	env := newImageInvalidationEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	leaver, reactor := env.user("asia"), env.user("asia")
	_, post := env.post(leaver, postForm)
	env.mustPost(reactor, postForm)
	_, reaction := env.realmoji(reactor, post["id"].(string), "like")
	time.Sleep(imageValidityTTL + 100*time.Millisecond)
	for _, imageURL := range []string{post["imageUrl"].(string), reaction["imageUrl"].(string)} {
		waitTrusted(env, imageURL)
	}
	start := time.Now()
	env.mustDo("POST", "/api/me/account-deletion", leaver.token, http.StatusNoContent)
	t.Logf("post image forbidden after %v", waitForbidden(env, post["imageUrl"].(string), start))
	t.Logf("realmoji on the post forbidden after %v", waitForbidden(env, reaction["imageUrl"].(string), start))
}

// 初期化（image_invalidation.sql の最後で世代を変える）を、初期化のリクエストを受けていないプロセスも検知して、
// 覚えている画像を捨てる。画像は DB で確かめ直して、有効なら返す。
func TestImageInvalidationDBEpochChangeClears(t *testing.T) {
	env := newImageInvalidationEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author := env.user("asia")
	_, post := env.post(author, postForm)
	time.Sleep(imageValidityTTL + 100*time.Millisecond)
	waitTrusted(env, post["imageUrl"].(string))
	id := imageIDFromURL(t, post["imageUrl"].(string))

	env.db.MustExec(`
  BEGIN;
  TRUNCATE image_invalidations;
  UPDATE image_invalidation_epoch SET epoch = gen_random_uuid()::text WHERE id = 1;
  COMMIT;
 `)
	deadline := time.Now().Add(3 * time.Second)
	for imageValidity.valid(id) {
		if time.Now().After(deadline) {
			t.Fatal("epoch change was not detected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status := imageStatus(env, post["imageUrl"].(string)); status != http.StatusOK {
		t.Fatalf("valid image after epoch change: %d", status)
	}
}
