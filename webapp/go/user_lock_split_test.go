package isureal

import (
	"net/http"
	"testing"
	"time"
)

// #86 の、users の行のロックを用途で分けたことを、実際の PostgreSQL と HTTP ハンドラで確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql を流した DB を指定したときだけ実行する。
//
// 本人のログイン・投稿・ほかの投稿へのコメントと RealMoji は、本人の投稿へのコメントや RealMoji（投稿者の行の FOR SHARE）を待たない。
// 投稿の編集のように権限を減らしうる変更は、今まで通り待つ。

// lockSplitLimit は「待たされていない」とみなす時間。ロックを待てば、手放すまで終わらない。
const lockSplitLimit = 3 * time.Second

// holdInteractionLock は、actor が author の投稿にコメントや RealMoji を送っている途中の状態
// （isureal_lock_interaction_users で2人の行をロックした状態）を、返した関数を呼ぶまで保つ。
func (env *lockTestEnv) holdInteractionLock(actor, author string) func() {
	env.t.Helper()
	tx, err := env.db.Beginx()
	if err != nil {
		env.t.Fatal(err)
	}
	var locked bool
	if err := tx.Get(&locked, `SELECT isureal_lock_interaction_users($1, $2)`, actor, author); err != nil || !locked {
		tx.Rollback()
		env.t.Fatalf("lock interaction users: locked=%v err=%v", locked, err)
	}
	return func() { tx.Rollback() }
}

// holdRowLock は、users の行を mode（FOR NO KEY UPDATE など）でロックした状態を、返した関数を呼ぶまで保つ。
func (env *lockTestEnv) holdRowLock(mode, userID string) func() {
	env.t.Helper()
	tx, err := env.db.Beginx()
	if err != nil {
		env.t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT 1 FROM users WHERE id = $1 `+mode, userID); err != nil {
		tx.Rollback()
		env.t.Fatal(err)
	}
	return func() { tx.Rollback() }
}

// finishesWithin は f を別の goroutine で動かし、limit 以内に終われば true を返す。
// 終わらなければ release を呼んでロックを手放し、f が終わるのを待ってから false を返す。
func finishesWithin(limit time.Duration, release func(), f func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		release()
		<-done
		return false
	}
}

// 投稿者の投稿へのコメントが途中のときも、投稿者の投稿、ほかの投稿へのコメントと RealMoji、ログインは待たされない。
// 投稿の編集（FOR NO KEY UPDATE）は、今まで通り待たされる。
func TestLockSplitAuthorNotBlockedByInteractions(t *testing.T) {
	env := newLockTestEnv(t)
	env.mustOpenWindow(env.adminToken())
	name, secret, author := env.acctNewUser("asia")
	other, commenter := env.user("asia"), env.user("asia")
	otherPost := env.mustPost(other, postForm)

	release := env.holdInteractionLock(commenter.id, author.id)
	defer release()
	var authorPost string
	steps := []struct {
		name string
		want int
		do   func() (int, map[string]any)
	}{
		{"post", http.StatusCreated, func() (int, map[string]any) {
			status, body := env.post(author, postForm)
			if id, ok := body["id"].(string); ok {
				authorPost = id
			}
			return status, body
		}},
		{"comment on another post", http.StatusCreated, func() (int, map[string]any) { return env.comment(author, otherPost, "hi") }},
		{"realmoji on another post", http.StatusCreated, func() (int, map[string]any) { return env.realmoji(author, otherPost, "like") }},
		// ログインはトークンを置き換えるので最後にする。
		{"login", http.StatusOK, func() (int, map[string]any) { return env.acctLogin(name, secret) }},
	}
	var token string
	for _, step := range steps {
		var status int
		var body map[string]any
		if !finishesWithin(lockSplitLimit, release, func() { status, body = step.do() }) {
			t.Fatalf("%s waited for the interaction lock on the author", step.name)
		}
		env.mustStatus(step.name, step.want, status, body)
		token, _ = body["token"].(string)
	}

	// 投稿の編集は投稿者の行を FOR NO KEY UPDATE でロックするので、コメントが終わるまで待つ。
	var status int
	var body map[string]any
	if finishesWithin(500*time.Millisecond, release, func() {
		status, body = env.do("PATCH", "/api/posts/"+authorPost, token, map[string]string{"caption": "edited"}, nil)
	}) {
		t.Fatalf("edit did not wait for the interaction lock: %d %v", status, body)
	}
	env.mustStatus("edit after release", http.StatusOK, status, body)
}

// 投稿者の行に FOR SHARE がかかっている間に同じ利用者が同時にログインしても、待たされず、有効なセッションは1つだけ残る。
func TestLockSplitConcurrentLoginsWhileShared(t *testing.T) {
	env := newLockTestEnv(t)
	name, secret, u := env.acctNewUser("asia")
	commenter := env.user("asia")
	release := env.holdInteractionLock(commenter.id, u.id)
	defer release()

	tokens := make([]string, 12)
	statuses := make([]int, len(tokens))
	if !finishesWithin(lockSplitLimit, release, func() {
		parallel(len(tokens), func(i int) {
			var body map[string]any
			statuses[i], body = env.acctLogin(name, secret)
			tokens[i], _ = body["token"].(string)
		})
	}) {
		t.Fatal("logins waited for the interaction lock")
	}
	release()
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

// 投稿者の行に FOR SHARE がかかっている間に同じ利用者が同時に投稿しても、待たされず、受け付けるのは1件だけ。
func TestLockSplitConcurrentPostsWhileShared(t *testing.T) {
	env := newLockTestEnv(t)
	env.mustOpenWindow(env.adminToken())
	u, commenter := env.user("asia"), env.user("asia")
	release := env.holdInteractionLock(commenter.id, u.id)
	defer release()

	statuses := make([]int, 12)
	if !finishesWithin(lockSplitLimit, release, func() {
		parallel(len(statuses), func(i int) { statuses[i], _ = env.post(u, postForm) })
	}) {
		t.Fatal("posts waited for the interaction lock")
	}
	counts := map[int]int{}
	for _, s := range statuses {
		counts[s]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != len(statuses)-1 {
		t.Fatalf("statuses = %v", counts)
	}
	var posts, images int
	env.db.Get(&posts, `SELECT count(*) FROM posts WHERE author_id = $1`, u.id)
	env.db.Get(&images, `SELECT count(*) FROM image_objects WHERE owner_user_id = $1`, u.id)
	if posts != 1 || images != 1 {
		t.Fatalf("posts=%d images=%d", posts, images)
	}
}

// 正しくない SecretKey のログインは行をロックしないので、ほかの処理が行を FOR NO KEY UPDATE でロックしていても待たずに 401 になる。
// 正しい SecretKey のログインは、今まで通りその処理を待つ。
func TestLockSplitInvalidLoginDoesNotLock(t *testing.T) {
	env := newLockTestEnv(t)
	name, secret, u := env.acctNewUser("asia")
	release := env.holdRowLock("FOR NO KEY UPDATE", u.id)
	defer release()

	var status int
	if !finishesWithin(lockSplitLimit, release, func() { status, _ = env.acctLogin(name, "wrong-"+secret) }) {
		t.Fatal("invalid login waited for the row lock")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("invalid login: %d", status)
	}
	var body map[string]any
	if finishesWithin(500*time.Millisecond, release, func() { status, body = env.acctLogin(name, secret) }) {
		t.Fatalf("valid login did not wait for the row lock: %d %v", status, body)
	}
	env.mustStatus("valid login after release", http.StatusOK, status, body)
}
