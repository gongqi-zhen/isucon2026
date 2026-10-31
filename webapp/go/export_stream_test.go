package isureal

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// エクスポートを1回のクエリとストリーミングにした変更（#68）を確かめる。
// ISUREAL_TEST_DSN を指定したときだけ実行する（lock_contention_test.go と同じ DB を使う）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55432/isureal?sslmode=disable' go test -run TestExportStream -v ./...

// bufferedExportZip は変更前の exportPostsHandler と同じクエリと組み立てで zip を作る（getUser、投稿の一覧、loadImageData を1回）。
func bufferedExportZip(ctx context.Context, db *sqlx.DB, userID string) ([]byte, error) {
	author, err := getUser(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	var posts []Post
	if err := db.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `, userID); err != nil {
		return nil, err
	}
	images, err := loadImageData(ctx, db, posts)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	items := make([]exportPostResponse, 0, len(posts))
	for _, post := range posts {
		data, ok := images[post.ImageID]
		if !ok {
			continue
		}
		imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
		image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
		if err != nil {
			return nil, err
		}
		if _, err := image.Write(data); err != nil {
			return nil, err
		}
		items = append(items, exportPostResponse{createPostResponse: newPostResponse(post, author), ImagePath: imagePath})
	}
	manifest, err := writer.Create("posts.json")
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(manifest).Encode(items); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// createExportPosts は、いくつもの月にまたがる投稿を count 件作る。
// 削除済みの投稿、画像の行がない投稿、画像の created_at が posted_at と違う投稿、同じ画像を使う投稿も混ぜる。
func createExportPosts(env *lockTestEnv, userID string, count int, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	base := time.Date(2024, 10, 3, 0, 0, 0, 0, time.UTC)
	var sharedImage string
	var previousPostedAt time.Time
	for i := range count {
		postedAt := base.AddDate(0, 0, i*11).Add(time.Duration(rng.Intn(3600)) * time.Second)
		if i >= count-2 && i > 0 {
			// 最後の3件を同じ posted_at にして、id での並び順も確かめる。
			postedAt = previousPostedAt
		}
		previousPostedAt = postedAt
		windowID := uuid.NewString()
		env.db.MustExec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			windowID, "test-"+uuid.NewString()[:8], postedAt.Format(time.DateOnly), postedAt.Add(-time.Hour), postedAt.Add(-30*time.Minute), postedAt.Add(time.Hour))
		imageID := uuid.NewString()
		switch {
		case i%9 == 4:
			// 画像の行がない投稿（含めない）。
		case i%10 == 7 && sharedImage != "":
			imageID = sharedImage
		default:
			data := make([]byte, 500+rng.Intn(40000))
			rng.Read(data)
			createdAt := postedAt
			if i%8 == 2 {
				createdAt = postedAt.AddDate(0, 1, 0)
			}
			env.db.MustExec(`INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)`, imageID, userID, data, createdAt)
			sharedImage = imageID
		}
		var deletedAt *time.Time
		if i%7 == 3 {
			deletedAt = &postedAt
		}
		location := []any{"exact", rng.Float64()*180 - 90, rng.Float64()*360 - 180, nil}
		if i%3 == 1 {
			location = []any{"approximate", nil, nil, fmt.Sprintf("場所 %d", i)}
		}
		env.db.MustExec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, latitude, longitude, locality, posted_at, deleted_at)
   VALUES ($1, $2, $3, $4, $5, 'friends', $6, $7, $8, $9, $10, $11)`,
			uuid.NewString(), userID, windowID, imageID, fmt.Sprintf("caption <%d> & \"x\"", i), location[0], location[1], location[2], location[3], postedAt, deletedAt)
	}
}

func getExport(t *testing.T, env *lockTestEnv, token string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", env.server.URL+"/api/me/export", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Client-IP", "10.68."+fmt.Sprint(time.Now().UnixNano()%250)+"."+fmt.Sprint(time.Now().UnixNano()/7%250))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return res, body, err
}

// getExportHTTP10 は、nginx の既定（proxy_http_version 1.0）と同じく HTTP/1.0 で要求する。応答は接続を閉じるまで読む。
func getExportHTTP10(t *testing.T, env *lockTestEnv, token string) (*http.Response, []byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(env.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /api/me/export HTTP/1.0\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nX-Client-IP: 10.69.%d.%d\r\n\r\n",
		token, time.Now().UnixNano()%250, time.Now().UnixNano()/7%250)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return res, body, err
}

// 署名付き URL の exp は60秒単位なので、分の境目をまたいだら作り直して比べる。
func assertSameExport(t *testing.T, env *lockTestEnv, user lockTestUser, label string) {
	t.Helper()
	assertSameExportVia(t, env, user, label, getExport)
}

func assertSameExportVia(t *testing.T, env *lockTestEnv, user lockTestUser, label string, fetch func(*testing.T, *lockTestEnv, string) (*http.Response, []byte, error)) {
	t.Helper()
	ctx := context.Background()
	for range 3 {
		before, err := bufferedExportZip(ctx, env.db, user.id)
		if err != nil {
			t.Fatal(err)
		}
		res, body, err := fetch(t, env, user.token)
		if err != nil {
			t.Fatalf("%s: read body: %v", label, err)
		}
		after, err := bufferedExportZip(ctx, env.db, user.id)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/zip" ||
			res.Header.Get("Content-Disposition") != `attachment; filename="isureal-posts.zip"` || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status %d headers %v", label, res.StatusCode, res.Header)
		}
		if !bytes.Equal(before, after) {
			continue
		}
		if !bytes.Equal(body, before) {
			gotFiles, gotManifest := exportContents(t, body)
			wantFiles, wantManifest := exportContents(t, before)
			t.Fatalf("%s: zip bytes differ (len %d vs %d, files %d vs %d, items %d vs %d)", label, len(body), len(before), len(gotFiles), len(wantFiles), len(gotManifest), len(wantManifest))
		}
		return
	}
	t.Fatalf("%s: legacy export kept changing (attempt limit)", label)
}

func TestExportStreamMatchesBufferedBytes(t *testing.T) {
	env := newLockTestEnv(t)
	user := env.user("asia")
	createExportPosts(env, user.id, 45, 68)

	defer func(first, max int) { exportImageChunkFirst, exportImageChunkMax = first, max }(exportImageChunkFirst, exportImageChunkMax)
	for _, size := range [][2]int{{1, 1}, {3, 3}, {1, 8}, {exportImageChunkFirst, exportImageChunkMax}, {1000, 1000}} {
		exportImageChunkFirst, exportImageChunkMax = size[0], size[1]
		// キャッシュが空の状態、キャッシュにある状態の両方で比べる。
		resetImageCaches()
		assertSameExport(t, env, user, fmt.Sprintf("chunk %v, cold", size))
		assertSameExport(t, env, user, fmt.Sprintf("chunk %v, cached", size))
	}
}

func TestExportStreamNoPosts(t *testing.T) {
	env := newLockTestEnv(t)
	user := env.user("asia")
	assertSameExport(t, env, user, "no posts")

	// すべての投稿の画像の行がない場合も、posts.json だけの zip になる。
	windowID := uuid.NewString()
	env.db.MustExec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline) VALUES ($1, $2, '2025-03-01', now(), now())`, windowID, "test-"+uuid.NewString()[:8])
	env.db.MustExec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at) VALUES ($1, $2, $3, $4, 'public', 'none', now())`,
		uuid.NewString(), user.id, windowID, uuid.NewString())
	assertSameExport(t, env, user, "no images")
}

func TestExportStreamInactiveAuthor(t *testing.T) {
	env := newLockTestEnv(t)
	user := env.user("asia")
	createExportPosts(env, user.id, 3, 1)
	ctx := context.Background()
	if _, posts, err := loadExportPosts(ctx, env.db, user.id); err != nil || len(posts) == 0 {
		t.Fatalf("active author: posts %d err %v", len(posts), err)
	}
	env.db.MustExec(`UPDATE users SET status = 'deleted' WHERE id = $1`, user.id)
	// 変更前の getUser と同じく sql.ErrNoRows を返す（ハンドラは 500 になる）。
	if _, _, err := loadExportPosts(ctx, env.db, user.id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted author: err %v", err)
	}
	if _, _, err := loadExportPosts(ctx, env.db, uuid.NewString()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown author: err %v", err)
	}
}

// 最初のまとまりの画像を読めなければ、ヘッダーを送る前なので 500 を返す。
// 2つ目以降で失敗したら、接続を切り、正常に終わった zip として受け取れないようにする。
func TestExportStreamFailure(t *testing.T) {
	env := newLockTestEnv(t)
	user := env.user("asia")
	createExportPosts(env, user.id, 12, 2)

	defer func(first, max int, load func(context.Context, sqlx.QueryerContext, []Post) (map[string][]byte, error)) {
		exportImageChunkFirst, exportImageChunkMax, exportLoadImages = first, max, load
	}(exportImageChunkFirst, exportImageChunkMax, exportLoadImages)
	exportImageChunkFirst, exportImageChunkMax = 3, 3
	_, posts, err := loadExportPosts(context.Background(), env.db, user.id)
	if err != nil || len(posts) < 9 {
		t.Fatalf("posts %d err %v", len(posts), err)
	}
	// 読み込みは同時に進むので、呼び出しの順ではなく、まとまりに含まれる投稿で失敗させる。
	failAt := func(failing Post) {
		exportLoadImages = func(ctx context.Context, db sqlx.QueryerContext, chunk []Post) (map[string][]byte, error) {
			for _, post := range chunk {
				if post.ID == failing.ID {
					return nil, errors.New("injected image load failure")
				}
			}
			return loadImageData(ctx, db, chunk)
		}
	}

	failAt(posts[0])
	res, _, err := getExport(t, env, user.token)
	if err != nil || res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first chunk failure: status %d err %v", res.StatusCode, err)
	}

	// 3つ目のまとまり（posts[6]〜posts[8]）で失敗させる。
	failAt(posts[7])
	res, body, err := getExport(t, env, user.token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("later chunk failure: status %d", res.StatusCode)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("later chunk failure: body read err %v (len %d), want unexpected EOF", err, len(body))
	}
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err == nil {
		t.Fatal("later chunk failure: truncated body is a valid zip")
	}

	// HTTP/1.0 では途中で切っても完全な応答と区別できないので、全部の画像を読んでから書き始める。
	// 後ろのまとまりで失敗しても、途中までの 200 ではなく 500 になる。
	res, _, err = getExportHTTP10(t, env, user.token)
	if err != nil || res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("HTTP/1.0 later chunk failure: status %d err %v", res.StatusCode, err)
	}

	// 失敗のあとも、普通にエクスポートできる（先読みの goroutine が残って詰まらない）。
	exportLoadImages = loadImageData
	assertSameExport(t, env, user, "after failure")
	assertSameExportVia(t, env, user, "after failure, HTTP/1.0", getExportHTTP10)
}

func TestExportStreamHTTP10MatchesBufferedBytes(t *testing.T) {
	env := newLockTestEnv(t)
	user := env.user("asia")
	createExportPosts(env, user.id, 30, 10)
	defer func(first, max int) { exportImageChunkFirst, exportImageChunkMax = first, max }(exportImageChunkFirst, exportImageChunkMax)
	exportImageChunkFirst, exportImageChunkMax = 2, 4
	resetImageCaches()
	assertSameExportVia(t, env, user, "HTTP/1.0, cold", getExportHTTP10)
	assertSameExportVia(t, env, user, "HTTP/1.0, cached", getExportHTTP10)
	empty := env.user("asia")
	assertSameExportVia(t, env, empty, "HTTP/1.0, no posts", getExportHTTP10)
}

// 以下は DB を使わない。先読みが投稿の順に返すこと、まとまりの大きさ、同時に抱えるまとまりの数を確かめる。

func withExportPrefetch(t *testing.T, first, max int, load func(context.Context, sqlx.QueryerContext, []Post) (map[string][]byte, error)) {
	t.Helper()
	oldFirst, oldMax, oldLoad := exportImageChunkFirst, exportImageChunkMax, exportLoadImages
	t.Cleanup(func() { exportImageChunkFirst, exportImageChunkMax, exportLoadImages = oldFirst, oldMax, oldLoad })
	exportImageChunkFirst, exportImageChunkMax, exportLoadImages = first, max, load
}

func fakeExportPosts(n int) []Post {
	posts := make([]Post, n)
	for i := range posts {
		posts[i] = Post{ID: fmt.Sprintf("post-%03d", i), ImageID: fmt.Sprintf("image-%03d", i)}
	}
	return posts
}

func TestPrefetchExportImagesOrderAndBound(t *testing.T) {
	var mu sync.Mutex
	running, maxRunning := 0, 0 // 同時に進んでいる読み込みの数
	withExportPrefetch(t, 2, 8, func(ctx context.Context, _ sqlx.QueryerContext, chunk []Post) (map[string][]byte, error) {
		mu.Lock()
		running++
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		time.Sleep(3 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		images := map[string][]byte{}
		for _, post := range chunk {
			images[post.ImageID] = []byte(post.ImageID)
		}
		return images, nil
	})
	posts := fakeExportPosts(61)
	var gotIDs []string
	var sizes []int
	for chunk := range prefetchExportImages(context.Background(), nil, posts) {
		if chunk.err != nil {
			t.Fatal(chunk.err)
		}
		sizes = append(sizes, len(chunk.posts))
		for _, post := range chunk.posts {
			gotIDs = append(gotIDs, post.ID)
			if string(chunk.images[post.ImageID]) != post.ImageID {
				t.Fatalf("image for %s missing", post.ID)
			}
		}
	}
	wantIDs := make([]string, len(posts))
	for i, post := range posts {
		wantIDs[i] = post.ID
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("order differs: %v", gotIDs)
	}
	if want := []int{2, 4, 8, 8, 8, 8, 8, 8, 7}; !reflect.DeepEqual(sizes, want) {
		t.Fatalf("chunk sizes %v, want %v", sizes, want)
	}
	if maxRunning != 2 {
		t.Fatalf("loaded %d chunks at once, want 2", maxRunning)
	}
}

// 受け取る側が遅くても、読み終えて渡していないまとまりは2つまで（それ以上は先に読まない）。
func TestPrefetchExportImagesSlowConsumer(t *testing.T) {
	var loaded atomic.Int32
	withExportPrefetch(t, 1, 1, func(ctx context.Context, _ sqlx.QueryerContext, chunk []Post) (map[string][]byte, error) {
		loaded.Add(1)
		return map[string][]byte{chunk[0].ImageID: {1}}, nil
	})
	chunks := prefetchExportImages(context.Background(), nil, fakeExportPosts(20))
	for received := 1; received <= 5; received++ {
		<-chunks
		time.Sleep(20 * time.Millisecond)
		// 受け取った数 + 受け取る側に渡そうとしている1つ + 待っている1つ
		if n := int(loaded.Load()); n > received+2 {
			t.Fatalf("after receiving %d chunks, loaded %d", received, n)
		}
	}
	for range chunks {
	}
}

func TestPrefetchExportImagesStopsOnErrorAndCancel(t *testing.T) {
	var calls atomic.Int32
	withExportPrefetch(t, 1, 1, func(ctx context.Context, _ sqlx.QueryerContext, chunk []Post) (map[string][]byte, error) {
		calls.Add(1)
		if chunk[0].ID == "post-002" {
			return nil, errors.New("boom")
		}
		return map[string][]byte{chunk[0].ImageID: {1}}, nil
	})
	var got []string
	for chunk := range prefetchExportImages(context.Background(), nil, fakeExportPosts(10)) {
		got = append(got, chunk.posts[0].ID)
		if chunk.err != nil {
			got = append(got, "error")
		}
	}
	// 失敗したまとまりを返したら、そこで終える。
	if want := []string{"post-000", "post-001", "post-002", "error"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if calls.Load() > 4 {
		t.Fatalf("loaded %d chunks after the failure", calls.Load())
	}

	// 受け取る側がやめて ctx を取り消したら、先読みの goroutine は終わり、チャネルが閉じる。
	withExportPrefetch(t, 1, 1, func(ctx context.Context, _ sqlx.QueryerContext, chunk []Post) (map[string][]byte, error) {
		return map[string][]byte{chunk[0].ImageID: {1}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	chunks := prefetchExportImages(ctx, nil, fakeExportPosts(100))
	<-chunks
	cancel()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-chunks:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("prefetch did not stop after cancel")
		}
	}
}
