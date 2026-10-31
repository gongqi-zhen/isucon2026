package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// #25 のロック緩和を、実際の PostgreSQL と HTTP ハンドラで確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql と migrate.sql を流した DB を指定したときだけ実行する（データは追加される）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55432/isureal?sslmode=disable' go test -run TestLock -v ./...

var (
	lockTestServerOnce sync.Once
	lockTestServer     *httptest.Server
)

type lockTestEnv struct {
	t      *testing.T
	db     *sqlx.DB
	server *httptest.Server
	jpeg   []byte
}

func newLockTestEnv(t *testing.T) *lockTestEnv {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	config := &Config{
		Region: "asia", DBHost: u.Hostname(), DBPort: u.Port(), DBUser: u.User.Username(), DBPassword: password,
		DBName: strings.TrimPrefix(u.Path, "/"), DBSSLMode: "disable", LogLevel: "error", PprofAddr: "off",
	}
	// サーバは接続プールを持つので、テスト間で1つを共有する。
	lockTestServerOnce.Do(func() { lockTestServer = httptest.NewServer(NewServer(config)) })
	server := lockTestServer
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 64)), nil); err != nil {
		t.Fatal(err)
	}
	return &lockTestEnv{t: t, db: db, server: server, jpeg: buf.Bytes()}
}

func (env *lockTestEnv) do(method, path, token string, body any, form map[string]string) (int, map[string]any) {
	var reader *bytes.Buffer
	contentType := ""
	switch {
	case form != nil:
		reader = &bytes.Buffer{}
		writer := multipart.NewWriter(reader)
		for key, value := range form {
			writer.WriteField(key, value)
		}
		part, _ := writer.CreateFormFile("image", "image.jpg")
		part.Write(env.jpeg)
		writer.Close()
		contentType = writer.FormDataContentType()
	case body != nil:
		data, _ := json.Marshal(body)
		reader = bytes.NewBuffer(data)
		contentType = "application/json"
	default:
		reader = &bytes.Buffer{}
	}
	req, _ := http.NewRequest(method, env.server.URL+path, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// IP ごとの BAN に引っかからないよう、リクエストごとに別の IP にする。
	req.Header.Set("X-Client-IP", "10.0."+fmt.Sprint(time.Now().UnixNano()%250)+"."+fmt.Sprint(time.Now().UnixNano()/7%250))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

type lockTestUser struct{ id, token string }

func (env *lockTestEnv) user(region string) lockTestUser {
	name := "lt" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	status, registered := env.do("POST", "/api/auth/register", "", map[string]string{"username": name, "displayName": name, "region": region}, nil)
	if status != http.StatusCreated {
		env.t.Fatalf("register: %d %v", status, registered)
	}
	status, login := env.do("POST", "/api/auth/login", "", map[string]string{"username": name, "secretKey": registered["secretKey"].(string)}, nil)
	if status != http.StatusOK {
		env.t.Fatalf("login: %d %v", status, login)
	}
	return lockTestUser{id: login["user"].(map[string]any)["id"].(string), token: login["token"].(string)}
}

func (env *lockTestEnv) adminToken() string {
	name := "lockadmin"
	hash := sha256.Sum256([]byte("password"))
	env.db.MustExec(`INSERT INTO administrators (id, username, password_hash) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, "lock-test-admin", name, hex.EncodeToString(hash[:]))
	status, body := env.do("POST", "/api/admin/auth/login", "", map[string]string{"username": name, "password": "password"}, nil)
	if status != http.StatusOK {
		env.t.Fatalf("admin login: %d %v", status, body)
	}
	return body["token"].(string)
}

var lockTestDate = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
var lockTestDateMu sync.Mutex

// openWindow は、これまでより後の日付で asia の投稿期間を開く。
func (env *lockTestEnv) openWindow(admin string) (int, map[string]any) {
	lockTestDateMu.Lock()
	var latest time.Time
	env.db.Get(&latest, `SELECT coalesce(max(local_date), '2200-01-01') FROM posting_windows WHERE region = 'asia'`)
	if latest.After(lockTestDate) {
		lockTestDate = latest
	}
	lockTestDate = lockTestDate.AddDate(0, 0, 1)
	date := lockTestDate.Format(time.DateOnly)
	lockTestDateMu.Unlock()
	return env.do("POST", "/api/admin/posting-window", admin, map[string]string{"region": "asia", "localDate": date}, nil)
}

func parallel(n int, f func(i int)) {
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); f(i) }()
	}
	wg.Wait()
}

// 同じユーザー名での同時登録は、テーブルロックをやめても1件だけ成功し、残りは 409 になる。
func TestLockConcurrentRegisterSameUsername(t *testing.T) {
	env := newLockTestEnv(t)
	name := "ltdup" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	statuses := make([]int, 20)
	parallel(len(statuses), func(i int) {
		statuses[i], _ = env.do("POST", "/api/auth/register", "", map[string]string{"username": name, "displayName": "dup", "region": "asia"}, nil)
	})
	counts := map[int]int{}
	for _, status := range statuses {
		counts[status]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != len(statuses)-1 {
		t.Fatalf("statuses = %v", counts)
	}
	var n int
	env.db.Get(&n, `SELECT count(*) FROM users WHERE username = $1`, name)
	if n != 1 {
		t.Fatalf("users with username = %d", n)
	}
}

// 同じリージョンの投稿は並行して受け付けられ、投稿期間の切り替えと同時に投稿しても、
// 受け付けた投稿はどれも、その時点で開いていた投稿期間に属し、1人1件の制約も守られる。
func TestLockConcurrentPostsAndWindowSwitch(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	if status, body := env.openWindow(admin); status != http.StatusCreated {
		t.Fatalf("open window: %d %v", status, body)
	}
	users := make([]lockTestUser, 30)
	for i := range users {
		users[i] = env.user("asia")
	}
	statuses := make([]int, len(users))
	windowIDs := make([]string, len(users))
	var switched int
	parallel(len(users)+1, func(i int) {
		if i == len(users) {
			time.Sleep(5 * time.Millisecond)
			switched, _ = env.openWindow(admin)
			return
		}
		var body map[string]any
		statuses[i], body = env.do("POST", "/api/posts", users[i].token, nil, map[string]string{"visibility": "public", "location_kind": "none"})
		if statuses[i] == http.StatusCreated {
			windowIDs[i] = body["postingWindowId"].(string)
		}
	})
	if switched != http.StatusCreated {
		t.Fatalf("switch window: %d", switched)
	}
	for i, status := range statuses {
		if status != http.StatusCreated {
			t.Errorf("user %d post: %d", i, status)
		}
	}
	// 受け付けた投稿の posted_at は、その投稿期間の開始以降。
	// 終了した投稿期間に属する投稿は、終了の確定より前に確定している（終了の UPDATE は投稿の FOR SHARE を待つ）ので、ここでは数えない。
	var bad int
	env.db.Get(&bad, `
  SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.author_id = ANY($1) AND p.posted_at < w.starts_at
 `, userIDs(users))
	if bad != 0 {
		t.Fatalf("%d posts are outside their posting window", bad)
	}
	// 1人1件。2回目の投稿は 409 になる。
	status, _ := env.do("POST", "/api/posts", users[0].token, nil, map[string]string{"visibility": "public", "location_kind": "none"})
	if windowIDs[0] != "" {
		var current string
		env.db.Get(&current, `SELECT id FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`)
		if windowIDs[0] == current && status != http.StatusConflict {
			t.Fatalf("second post in the same window: %d", status)
		}
	}
}

// 同じ投稿者の投稿に、多数の利用者が同時に RealMoji とコメントを送っても、すべて受け付けられる。
// 同じ利用者が同じ投稿に同時に RealMoji を送った場合も、500 にならず、有効な RealMoji は1件だけ残る。
func TestLockConcurrentInteractions(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	if status, body := env.openWindow(admin); status != http.StatusCreated {
		t.Fatalf("open window: %d %v", status, body)
	}
	author := env.user("asia")
	status, post := env.do("POST", "/api/posts", author.token, nil, map[string]string{"visibility": "public", "location_kind": "none"})
	if status != http.StatusCreated {
		t.Fatalf("author post: %d %v", status, post)
	}
	postID := post["id"].(string)
	reactors := make([]lockTestUser, 20)
	for i := range reactors {
		reactors[i] = env.user("asia")
		if status, body := env.do("POST", "/api/posts", reactors[i].token, nil, map[string]string{"visibility": "public", "location_kind": "none"}); status != http.StatusCreated {
			t.Fatalf("reactor post: %d %v", status, body)
		}
	}
	realmoji := make([]int, len(reactors))
	comment := make([]int, len(reactors))
	parallel(len(reactors)*2, func(i int) {
		r := reactors[i%len(reactors)]
		if i < len(reactors) {
			realmoji[i], _ = env.do("POST", "/api/posts/"+postID+"/realmojis", r.token, nil, map[string]string{"reactionType": "like"})
		} else {
			comment[i-len(reactors)], _ = env.do("POST", "/api/posts/"+postID+"/comments", r.token, map[string]string{"body": "hi"}, nil)
		}
	})
	for i := range reactors {
		if realmoji[i] != http.StatusCreated || comment[i] != http.StatusCreated {
			t.Errorf("reactor %d: realmoji=%d comment=%d", i, realmoji[i], comment[i])
		}
	}

	same := make([]int, 8)
	parallel(len(same), func(i int) {
		same[i], _ = env.do("POST", "/api/posts/"+postID+"/realmojis", reactors[0].token, nil, map[string]string{"reactionType": "smile"})
	})
	for i, status := range same {
		if status != http.StatusCreated {
			t.Errorf("same reactor %d: %d", i, status)
		}
	}
	var active int
	env.db.Get(&active, `SELECT count(*) FROM realmojis WHERE post_id = $1 AND reactor_id = $2 AND replaced_at IS NULL AND deleted_at IS NULL`, postID, reactors[0].id)
	if active != 1 {
		t.Fatalf("active realmojis for the same reactor = %d", active)
	}
}

// ブロックと RealMoji を同時に送っても、デッドロックや 500 にならない。
// ブロックが先に確定した場合、その後に確定した RealMoji は存在しない（ブロック後は 403 になる）。
func TestLockInteractionVersusBlockNoDeadlock(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	if status, body := env.openWindow(admin); status != http.StatusCreated {
		t.Fatalf("open window: %d %v", status, body)
	}
	pairs := 15
	type pair struct {
		a, b         lockTestUser
		postA, postB string
	}
	ps := make([]pair, pairs)
	for i := range ps {
		ps[i].a, ps[i].b = env.user("asia"), env.user("asia")
		_, pa := env.do("POST", "/api/posts", ps[i].a.token, nil, map[string]string{"visibility": "public", "location_kind": "none"})
		_, pb := env.do("POST", "/api/posts", ps[i].b.token, nil, map[string]string{"visibility": "public", "location_kind": "none"})
		ps[i].postA, ps[i].postB = pa["id"].(string), pb["id"].(string)
	}
	results := make([][3]int, pairs)
	parallel(pairs*3, func(i int) {
		p := ps[i%pairs]
		switch i / pairs {
		case 0: // A が B の投稿に RealMoji
			results[i%pairs][0], _ = env.do("POST", "/api/posts/"+p.postB+"/realmojis", p.a.token, nil, map[string]string{"reactionType": "like"})
		case 1: // B が A の投稿に RealMoji（逆向き）
			results[i%pairs][1], _ = env.do("POST", "/api/posts/"+p.postA+"/realmojis", p.b.token, nil, map[string]string{"reactionType": "like"})
		case 2: // A が B をブロック
			results[i%pairs][2], _ = env.do("PUT", "/api/blocks/"+p.b.id, p.a.token, nil, nil)
		}
	})
	for i, r := range results {
		for j, status := range r {
			if status >= 500 {
				t.Errorf("pair %d op %d: %d", i, j, status)
			}
		}
		if r[2] != http.StatusOK && r[2] != http.StatusNoContent && r[2] != http.StatusCreated {
			t.Errorf("pair %d block: %d", i, r[2])
		}
		// ブロックの確定より後に確定した RealMoji があってはいけない。
		var after int
		env.db.Get(&after, `
   SELECT count(*) FROM realmojis r JOIN block_relations b ON b.blocker_id = $1 AND b.blocked_id = $2
   WHERE r.post_id IN ($3, $4) AND r.created_at > b.created_at
  `, ps[i].a.id, ps[i].b.id, ps[i].postA, ps[i].postB)
		if after != 0 {
			t.Errorf("pair %d: %d realmojis committed after the block", i, after)
		}
	}
	// ブロック後は、どちら向きの RealMoji も受け付けない。
	if status, _ := env.do("POST", "/api/posts/"+ps[0].postB+"/realmojis", ps[0].a.token, nil, map[string]string{"reactionType": "like"}); status != http.StatusForbidden {
		t.Fatalf("realmoji after block: %d", status)
	}
}

// lockInteractionUsers は、行為者を FOR NO KEY UPDATE、投稿者を FOR SHARE でロックする。
func TestLockInteractionUsersModes(t *testing.T) {
	env := newLockTestEnv(t)
	ctx := context.Background()
	for _, order := range []string{"actor<author", "actor>author"} {
		a, b := env.user("asia"), env.user("asia")
		actor, author := min(a.id, b.id), max(a.id, b.id)
		if order == "actor>author" {
			actor, author = author, actor
		}
		tx, err := env.db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		users, err := lockInteractionUsers(ctx, tx, actor, author)
		if err != nil || len(users) != 2 || users[actor].ID != actor || users[author].ID != author {
			t.Fatalf("%s: users=%v err=%v", order, users, err)
		}
		other, _ := env.db.BeginTxx(ctx, nil)
		other.MustExec(`SET LOCAL lock_timeout = '300ms'`)
		// 投稿者は、別の行為者も FOR SHARE で同時にロックできる。
		if _, err := other.Exec(`SELECT 1 FROM users WHERE id = $1 FOR SHARE`, author); err != nil {
			t.Errorf("%s: author FOR SHARE blocked: %v", order, err)
		}
		other.Rollback()
		// 投稿者への FOR NO KEY UPDATE（ブロックなど）は待たされる。
		other, _ = env.db.BeginTxx(ctx, nil)
		other.MustExec(`SET LOCAL lock_timeout = '300ms'`)
		if _, err := other.Exec(`SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, author); err == nil {
			t.Errorf("%s: author FOR NO KEY UPDATE was not blocked", order)
		}
		other.Rollback()
		// 行為者は FOR SHARE でもロックできない（同じ行為者の操作は直列）。
		other, _ = env.db.BeginTxx(ctx, nil)
		other.MustExec(`SET LOCAL lock_timeout = '300ms'`)
		if _, err := other.Exec(`SELECT 1 FROM users WHERE id = $1 FOR SHARE`, actor); err == nil {
			t.Errorf("%s: actor FOR SHARE was not blocked", order)
		}
		other.Rollback()
		tx.Rollback()
	}
}

func userIDs(users []lockTestUser) []string {
	ids := make([]string, len(users))
	for i, user := range users {
		ids[i] = user.id
	}
	return ids
}
