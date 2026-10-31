package isureal

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// エクスポートの画像をキャッシュ経由で読むようにした変更が、以前の実装と同じ zip を返すことを確かめる。
// ISUREAL_TEST_DSN を指定したときだけ実行する（lock_contention_test.go と同じ DB を使う）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55432/isureal?sslmode=disable' go test -run TestExport -v ./...

// legacyExportZip は変更前の exportPostsHandler と同じクエリと組み立てで zip を作る。
func legacyExportZip(ctx context.Context, db *sqlx.DB, userID string) ([]byte, error) {
	author, err := getUser(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	var posts []struct {
		Post
		ImageData []byte `db:"image_data"`
	}
	if err := db.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`, i.data AS image_data
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN image_objects i ON i.id = p.image_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `, userID); err != nil {
		return nil, err
	}
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	items := make([]exportPostResponse, 0, len(posts))
	for _, post := range posts {
		imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
		image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
		if err != nil {
			return nil, err
		}
		image.Write(post.ImageData)
		items = append(items, exportPostResponse{createPostResponse: newPostResponse(post.Post, author), ImagePath: imagePath})
	}
	manifest, _ := writer.Create("posts.json")
	json.NewEncoder(manifest).Encode(items)
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// exportContents は zip のファイル名と中身を返す。posts.json の署名付きURLは、時刻で変わる exp と sig を除く。
func exportContents(t *testing.T, data []byte) (map[string][]byte, []map[string]any) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	var manifest []map[string]any
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(rc)
		rc.Close()
		if file.Name == "posts.json" {
			if err := json.Unmarshal(content, &manifest); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if file.Method != zip.Store {
			t.Fatalf("%s: method %d", file.Name, file.Method)
		}
		files[file.Name] = content
	}
	for _, item := range manifest {
		for _, holder := range []map[string]any{item, item["author"].(map[string]any)} {
			for _, key := range []string{"imageUrl", "profileImageUrl"} {
				if raw, ok := holder[key].(string); ok {
					u, _ := url.Parse(raw)
					holder[key] = u.Path
				}
			}
		}
	}
	return files, manifest
}

func TestExportMatchesLegacy(t *testing.T) {
	env := newLockTestEnv(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))
	user := env.user("asia")

	// 日付の違う投稿を、いくつものパーティション（月）にまたがって直接作る。
	// 削除済みの投稿と、画像の行がない投稿（以前は内部結合で除外されていた）も混ぜる。
	base := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	for i := range 40 {
		postedAt := base.AddDate(0, 0, i*9).Add(time.Duration(rng.Intn(3600)) * time.Second)
		windowID := uuid.NewString()
		env.db.MustExec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			windowID, "test-"+uuid.NewString()[:8], postedAt.Format(time.DateOnly), postedAt.Add(-time.Hour), postedAt.Add(-30*time.Minute), postedAt.Add(time.Hour))
		imageID := uuid.NewString()
		if i%13 != 5 {
			data := make([]byte, 1000+rng.Intn(50000))
			rng.Read(data)
			env.db.MustExec(`INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)`, imageID, user.id, data, postedAt)
		}
		var deletedAt *time.Time
		if i%7 == 3 {
			deletedAt = &postedAt
		}
		env.db.MustExec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, latitude, longitude, posted_at, deleted_at)
   VALUES ($1, $2, $3, $4, $5, 'friends', 'exact', $6, $7, $8, $9)`,
			uuid.NewString(), user.id, windowID, imageID, fmt.Sprintf("caption %d", i), rng.Float64()*90, rng.Float64()*180, postedAt, deletedAt)
	}
	// 同じ posted_at の投稿を2件作り、id での並び順も確かめる。
	same := base.AddDate(1, 0, 0)
	for range 2 {
		windowID := uuid.NewString()
		env.db.MustExec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline) VALUES ($1, $2, $3, $4, $5)`,
			windowID, "test-"+uuid.NewString()[:8], same.Format(time.DateOnly), same, same.Add(time.Hour))
		imageID := uuid.NewString()
		env.db.MustExec(`INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)`, imageID, user.id, []byte("same-"+imageID), same)
		env.db.MustExec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, locality, posted_at)
   VALUES ($1, $2, $3, $4, '', 'public', 'approximate', 'Tokyo', $5)`, uuid.NewString(), user.id, windowID, imageID, same)
	}

	legacy, err := legacyExportZip(ctx, env.db, user.id)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles, wantManifest := exportContents(t, legacy)
	if len(wantFiles) < 30 {
		t.Fatalf("too few exported images: %d", len(wantFiles))
	}

	// 1回目はキャッシュが空、2回目はキャッシュから、3回目は一部だけキャッシュにある状態で比べる。
	resetImageCaches()
	for round := range 3 {
		if round == 2 {
			resetImageCaches()
			var someImage string
			env.db.Get(&someImage, `SELECT image_id FROM posts WHERE author_id = $1 AND deleted_at IS NULL ORDER BY posted_at LIMIT 1`, user.id)
			loadImageData(ctx, env.db, []Post{{ImageID: someImage}})
		}
		req, _ := http.NewRequest("GET", env.server.URL+"/api/me/export", nil)
		req.Header.Set("Authorization", "Bearer "+user.token)
		req.Header.Set("X-Client-IP", "10.9.9."+fmt.Sprint(round+1))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/zip" ||
			!strings.Contains(res.Header.Get("Content-Disposition"), "isureal-posts.zip") || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("round %d: status %d headers %v", round, res.StatusCode, res.Header)
		}
		gotFiles, gotManifest := exportContents(t, body)
		if !reflect.DeepEqual(gotFiles, wantFiles) {
			t.Fatalf("round %d: image files differ (%d vs %d)", round, len(gotFiles), len(wantFiles))
		}
		if !reflect.DeepEqual(gotManifest, wantManifest) {
			got, _ := json.Marshal(gotManifest)
			want, _ := json.Marshal(wantManifest)
			t.Fatalf("round %d: posts.json differs\n got: %s\nwant: %s", round, got, want)
		}
	}
}
