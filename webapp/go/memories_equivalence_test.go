package isureal

import (
	"context"
	"encoding/json"
	"fmt"
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

// Memories を1回のクエリで読むようにした変更が、以前の実装と同じ応答を返すことを確かめる。
// ISUREAL_TEST_DSN を指定したときだけ実行する（lock_contention_test.go と同じ DB を使う）。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55444/isureal?sslmode=disable' go test -run TestMemories -v ./...

// legacyListMemories は変更前の listMemories と loadMemoryInteractions と同じクエリと組み立てで応答を作る。
func legacyListMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	posts, err := selectPostsWithAuthor(ctx, db, `
  AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
  AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
  ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
 `, userID, start, end, beforeAt, beforeID)
	if err != nil {
		return response, err
	}
	items := make([]memoryPostResponse, 0, len(posts))
	for _, post := range posts {
		items = append(items, memoryPostResponse{
			createPostResponse: newPostResponse(post.Post, post.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		})
	}
	response.HasMore = len(items) > 50
	if response.HasMore {
		items = items[:50]
	}
	if len(items) > 0 {
		postIDs := make([]string, len(items))
		itemByPostID := make(map[string]*memoryPostResponse, len(items))
		for i := range items {
			postIDs[i] = items[i].ID
			itemByPostID[items[i].ID] = &items[i]
		}
		query, args, err := sqlx.In(`AND r.post_id IN (?) ORDER BY r.created_at DESC, r.id DESC`, postIDs)
		if err != nil {
			return response, err
		}
		reactions, err := selectCurrentRealMojis(ctx, db, sqlx.Rebind(sqlx.DOLLAR, query), args...)
		if err != nil {
			return response, err
		}
		for _, reaction := range reactions {
			item := itemByPostID[reaction.PostID]
			if len(item.RealMojis) < 50 {
				item.RealMojis = append(item.RealMojis, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
			}
		}
		query, args, err = sqlx.In(`AND c.post_id IN (?) ORDER BY c.created_at, c.id`, postIDs)
		if err != nil {
			return response, err
		}
		comments, err := selectCommentsWithAuthor(ctx, db, sqlx.Rebind(sqlx.DOLLAR, query), args...)
		if err != nil {
			return response, err
		}
		for _, comment := range comments {
			item := itemByPostID[comment.PostID]
			if len(item.Comments) < 50 {
				item.Comments = append(item.Comments, newCommentResponse(comment.Comment, comment.Author))
			}
		}
	}
	response.Items = items
	return response, nil
}

// memoriesJSON は応答を JSON にして、署名付きURLの時刻で変わる exp と sig を除いた値を返す。
func memoriesJSON(t *testing.T, response any) any {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	var strip func(v any)
	strip = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				if raw, ok := value.(string); ok && (key == "imageUrl" || key == "profileImageUrl") {
					u, _ := url.Parse(raw)
					if u.Query().Get("sig") == "" || u.Query().Get("exp") == "" {
						t.Fatalf("unsigned url %q", raw)
					}
					v[key] = u.Path
					continue
				}
				strip(value)
			}
		case []any:
			for _, value := range v {
				strip(value)
			}
		}
	}
	strip(decoded)
	return decoded
}

func TestMemoriesMatchesLegacy(t *testing.T) {
	env := newLockTestEnv(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	author := env.user("asia")

	// リアクションする利用者を直接作る。退会済み、プロフィール画像あり、JSON で特殊な文字を含む表示名と自己紹介も混ぜる。
	names := []string{`plain`, `quote "q" and \backslash`, "改行\nとタブ\t", "絵文字 😀🎉", "<html>&amp;", "", "日本語の名前"}
	others := make([]string, 0, 80)
	for i := range 80 {
		id := uuid.NewString()
		status := "active"
		var deletedAt *time.Time
		if i%9 == 4 {
			status = "deleted"
			now := time.Now()
			deletedAt = &now
		}
		var profileImageID *string
		if i%3 == 0 {
			pid := uuid.NewString()
			profileImageID = &pid
		}
		accountType := "regular"
		if i%7 == 1 {
			accountType = "certified_public"
		}
		env.db.MustExec(`INSERT INTO users (id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status, deleted_at)
   VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, "mem"+strings.ReplaceAll(id, "-", "")[:16], fmt.Sprintf("%064x", rng.Uint64()), names[i%len(names)]+fmt.Sprint(i), names[(i+3)%len(names)],
			profileImageID, []string{"asia", "europe", "america"}[i%3], accountType, status, deletedAt)
		others = append(others, id)
	}

	// 3か月にまたがる130件の投稿。削除済み、同じ posted_at の投稿、終了していない投稿期間も混ぜる。
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var postIDs []string
	for i := range 130 {
		day := base.AddDate(0, 0, i%90)
		postedAt := day.Add(time.Duration(rng.Intn(86400))*time.Second + time.Duration(rng.Intn(1000000))*time.Microsecond)
		if i%17 == 0 && i > 0 {
			postedAt = day.Add(12 * time.Hour) // 同じ日に同じ posted_at の投稿を作り、id での並びを確かめる
		}
		windowID := uuid.NewString()
		var endedAt *time.Time
		if i%5 != 0 {
			ended := postedAt.Add(time.Hour)
			endedAt = &ended
		}
		env.db.MustExec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			windowID, "test-"+uuid.NewString()[:8], day.Format(time.DateOnly), day, day.Add(time.Duration(rng.Intn(80000))*time.Second), endedAt)
		var deletedAt *time.Time
		if i%11 == 3 {
			deletedAt = &postedAt
		}
		postID := uuid.NewString()
		env.db.MustExec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, locality, posted_at, deleted_at)
   VALUES ($1, $2, $3, $4, $5, $6, 'approximate', $7, $8, $9)`,
			postID, author.id, windowID, uuid.NewString(), names[i%len(names)], []string{"public", "friends", "friends_of_friends"}[i%3], "場所"+fmt.Sprint(i), postedAt, deletedAt)
		postIDs = append(postIDs, postID)
		if i%4 == 1 {
			continue // リアクションもコメントもない投稿
		}
		// 多くの投稿は数件、いくつかの投稿は50件を超えるリアクションとコメントを持つ。
		n := 1 + rng.Intn(4)
		if i%23 == 2 {
			n = 95
		}
		tie := postedAt.Add(time.Minute)
		for j := range n {
			createdAt := postedAt.Add(time.Duration(j+1)*time.Second + time.Duration(rng.Intn(1000))*time.Microsecond)
			if j%10 == 0 {
				createdAt = tie // 同じ created_at を作り、id での並びを確かめる
			}
			reactor := others[(i+j)%len(others)]
			// 同じ (投稿, 利用者) の現在の RealMoji は1件だけなので、j が利用者の数を超えたら差し替え済みにする。
			var replacedAt, rDeletedAt *time.Time
			switch {
			case j >= len(others):
				replacedAt = &createdAt
			case j%13 == 6:
				rDeletedAt = &createdAt
			}
			env.db.MustExec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				uuid.NewString(), postID, reactor, uuid.NewString(), []string{"like", "smile", "surprise", "heart", "laugh", "instant"}[j%6], createdAt, replacedAt, rDeletedAt)
			var cDeletedAt *time.Time
			if j%8 == 5 {
				cDeletedAt = &createdAt
			}
			env.db.MustExec(`INSERT INTO comments (id, post_id, author_id, body, created_at, deleted_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				uuid.NewString(), postID, others[(i+2*j)%len(others)], names[j%len(names)]+fmt.Sprint(j), createdAt, cDeletedAt)
		}
	}
	// 退会していない投稿者の、別の利用者の投稿が混ざらないことも確かめる。
	other := env.user("asia")
	env.db.MustExec(`UPDATE posts SET author_id = $1 WHERE id = $2`, other.id, postIDs[7])

	compare := func(label string, start, end time.Time, beforeAt *time.Time, beforeID string) getMemoriesResponse {
		t.Helper()
		want, err := legacyListMemories(ctx, env.db, author.id, start, end, beforeAt, beforeID)
		if err != nil {
			t.Fatalf("%s: legacy: %v", label, err)
		}
		got, err := listMemories(ctx, env.db, author.id, start, end, beforeAt, beforeID)
		if err != nil {
			t.Fatalf("%s: new: %v", label, err)
		}
		if g, w := memoriesJSON(t, got), memoriesJSON(t, want); !reflect.DeepEqual(g, w) {
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			t.Fatalf("%s: response differs\n got: %s\nwant: %s", label, gj, wj)
		}
		// 時刻の値（タイムゾーンを含む）もそのまま一致すること。
		for i := range want.Items {
			for j := range want.Items[i].RealMojis {
				if g, w := got.Items[i].RealMojis[j].CreatedAt, want.Items[i].RealMojis[j].CreatedAt; !g.Equal(w) || g.Location() != w.Location() {
					t.Fatalf("%s: realmoji time %v vs %v", label, g, w)
				}
			}
			for j := range want.Items[i].Comments {
				if g, w := got.Items[i].Comments[j].CreatedAt, want.Items[i].Comments[j].CreatedAt; !g.Equal(w) || g.Location() != w.Location() {
					t.Fatalf("%s: comment time %v vs %v", label, g, w)
				}
			}
		}
		return want
	}

	sawMore, sawFull := false, false
	months := []time.Time{base.AddDate(0, -1, 0), base, base.AddDate(0, 1, 0), base.AddDate(0, 2, 0), base.AddDate(0, 3, 0)}
	for _, start := range months {
		end := start.AddDate(0, 1, 0)
		var beforeAt *time.Time
		beforeID := ""
		for page := 0; ; page++ {
			response := compare(fmt.Sprintf("%s page %d", start.Format("2006-01"), page), start, end, beforeAt, beforeID)
			for _, item := range response.Items {
				if len(item.RealMojis) == 50 && len(item.Comments) == 50 {
					sawFull = true
				}
			}
			if !response.HasMore {
				break
			}
			sawMore = true
			last := response.Items[len(response.Items)-1]
			at := last.PostedAt
			beforeAt, beforeID = &at, last.ID
		}
	}
	// 日付単位と、途中の位置から始めるページング。
	for i := range 90 {
		day := base.AddDate(0, 0, i)
		compare(day.Format(time.DateOnly), day, day.AddDate(0, 0, 1), nil, "")
	}
	middle := base.AddDate(0, 0, 20).Add(12 * time.Hour)
	compare("cursor in the middle", base, base.AddDate(0, 3, 0), &middle, postIDs[20])
	if !sawMore || !sawFull {
		t.Fatalf("test data did not cover hasMore=%v and 50-item limits=%v", sawMore, sawFull)
	}

	// HTTP の応答（ハンドラと JSON の形）も、以前の実装と一致すること。
	for _, path := range []string{"/api/memories?month=2026-04", "/api/memories/2026-03-10"} {
		status, body := env.do("GET", path, author.token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d", path, status)
		}
		var start, end time.Time
		if strings.Contains(path, "month=") {
			start = base.AddDate(0, 1, 0)
			end = start.AddDate(0, 1, 0)
		} else {
			start = base.AddDate(0, 0, 9)
			end = start.AddDate(0, 0, 1)
		}
		want, err := legacyListMemories(ctx, env.db, author.id, start, end, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if g, w := memoriesJSON(t, body), memoriesJSON(t, want); !reflect.DeepEqual(g, w) {
			t.Fatalf("%s: HTTP response differs", path)
		}
	}
	if status, _ := env.do("GET", "/api/memories?month=2026-04&beforeId=x", author.token, nil, nil); status != http.StatusBadRequest {
		t.Fatalf("pagination without beforePostedAt: status %d", status)
	}
}
