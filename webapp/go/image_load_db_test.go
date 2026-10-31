package isureal

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// キャッシュにない画像の読み込み（#101）を、実際の PostgreSQL で確かめる。ISUREAL_TEST_DSN を指定したときだけ実行する。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55464/isureal?sslmode=disable' go test -run 'TestImageLoadDB' -v ./...

// scannedImagePartitions は、loadValidImageObjectQuery の実行計画のうち、実際に読んだ image_objects のパーティションの数を返す。
func scannedImagePartitions(t *testing.T, env *lockTestEnv, imageID string, createdAt *time.Time) int {
	t.Helper()
	var plan string
	if err := env.db.Get(&plan, `EXPLAIN (ANALYZE, FORMAT JSON) `+loadValidImageObjectQuery, imageID, createdAt); err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]any
	if err := json.Unmarshal([]byte(plan), &nodes); err != nil {
		t.Fatal(err)
	}
	scanned := map[string]bool{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if name, _ := node["Relation Name"].(string); strings.HasPrefix(name, "image_objects") {
			if loops, _ := node["Actual Loops"].(float64); loops > 0 {
				scanned[name] = true
			}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if c, ok := child.(map[string]any); ok {
				walk(c)
			}
		}
	}
	for _, node := range nodes {
		if root, ok := node["Plan"].(map[string]any); ok {
			walk(root)
		}
	}
	return len(scanned)
}

// 投稿の応答を作ったプロセスは画像の created_at（posted_at）を覚え、キャッシュにない画像を1つのパーティションだけで読む。
// created_at が分からないか違っていれば、以前と同じく id だけで引く（見つかるまで、パーティションを順に調べる）。
func TestImageLoadDBPrunesPartitions(t *testing.T) {
	env := newImageInvalidationEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author := env.user("asia")
	_, post := env.post(author, postForm)
	imageID := imageIDFromURL(t, post["imageUrl"].(string))

	var createdAt time.Time
	if err := env.db.Get(&createdAt, `SELECT created_at FROM image_objects WHERE id = $1`, imageID); err != nil {
		t.Fatal(err)
	}
	owner, ok := imageOwnerKeyCache.Get(imageID)
	if !ok || owner.OwnerID != author.id || !owner.CreatedAt.Equal(createdAt) {
		t.Fatalf("owner cache = %+v, %v; image created_at = %v", owner, ok, createdAt)
	}

	if got := scannedImagePartitions(t, env, imageID, owner.createdAtArg()); got != 1 {
		t.Errorf("with created_at: scanned %d partitions, want 1", got)
	}
	// id だけで引くと、行のあるパーティション（2026-10 以降）に着くまで、前のパーティションの索引も調べる。
	if got := scannedImagePartitions(t, env, imageID, nil); got <= 1 {
		t.Errorf("without created_at: scanned %d partitions, want more than 1", got)
	}
	wrong := createdAt.Add(-24 * time.Hour)
	if got := scannedImagePartitions(t, env, imageID, &wrong); got <= 1 {
		t.Errorf("with a wrong created_at: scanned %d partitions, want the fallback over more than 1", got)
	}
}

// 中身のキャッシュにない画像を同時に取りにきても、全員が 200 を受け取る。削除の後は 403 になる。
func TestImageLoadDBConcurrentMisses(t *testing.T) {
	env := newImageInvalidationEnv(t)
	admin := env.adminToken()
	env.mustOpenWindow(admin)
	author := env.user("asia")
	_, post := env.post(author, postForm)
	imageURL := post["imageUrl"].(string)
	imageID := imageIDFromURL(t, imageURL)

	fetchAll := func() []int {
		imageObjectCache.Remove(imageID)
		imageValidityCache.Remove(imageID)
		const n = 16
		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses[i] = imageStatus(env, imageURL)
			}()
		}
		wg.Wait()
		return statuses
	}
	for i, status := range fetchAll() {
		if status != http.StatusOK {
			t.Fatalf("request %d: %d", i, status)
		}
	}
	if _, ok := imageObjectCache.Get(imageID); !ok {
		t.Fatal("the loaded image should be cached")
	}

	env.mustDo("DELETE", "/api/posts/"+post["id"].(string), author.token, http.StatusNoContent)
	imageValidity.reset()
	deadline := time.Now().Add(5 * time.Second)
	for {
		forbidden := true
		for _, status := range fetchAll() {
			forbidden = forbidden && status == http.StatusForbidden
		}
		if forbidden {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted image is still served 5s after the deletion")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
