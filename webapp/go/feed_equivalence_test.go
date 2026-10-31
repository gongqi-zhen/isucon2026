//go:build equivalence

// 旧実装（#8 #9 の変更前）と新実装を、同じ DB に対して比較するテスト。
// 実行方法は TestEquivalenceWithLegacy のコメントを参照。

package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// ===== 旧実装（変更前のコードをそのまま写したもの） =====

func legacyLoadViewerContext(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}}
	var err error
	if viewer.User, err = getUser(ctx, db, viewerID); err != nil {
		return viewer, err
	}
	if err := sqlx.GetContext(ctx, db, &viewer.HasPostedToday, `
  SELECT EXISTS(
    SELECT 1 FROM posts p
    JOIN posting_windows posted ON posted.id = p.posting_window_id
    JOIN posting_windows current ON current.region = $2 AND current.ended_at IS NULL
                                AND current.local_date = posted.local_date
    WHERE p.author_id = $1 AND p.deleted_at IS NULL
  )
 `, viewer.ID, viewer.Region); err != nil {
		return viewer, err
	}
	if viewer.Blocked, err = getBlockedUserIDs(ctx, db, viewer.ID); err != nil {
		return viewer, err
	}
	friends, err := getFriendIDs(ctx, db, viewer.ID)
	if err != nil {
		return viewer, err
	}
	if len(friends) == 0 {
		return viewer, nil
	}
	for _, id := range friends {
		viewer.Friends[id] = true
	}
	query, args, err := sqlx.In(`SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (?) OR user_high_id IN (?)`, friends, friends)
	if err != nil {
		return viewer, err
	}
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return viewer, err
	}
	for _, friendship := range friendships {
		if viewer.Friends[friendship.UserLowID] {
			viewer.FriendsOfFriends[friendship.UserHighID] = true
			if friendship.UserHighID != viewer.ID {
				viewer.MutualFriends[friendship.UserHighID]++
			}
		}
		if viewer.Friends[friendship.UserHighID] {
			viewer.FriendsOfFriends[friendship.UserLowID] = true
			if friendship.UserLowID != viewer.ID {
				viewer.MutualFriends[friendship.UserLowID]++
			}
		}
	}
	return viewer, nil
}

func legacyCanViewPost(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	post, err := getPostWithAuthor(ctx, db, postID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	viewer, err := legacyLoadViewerContext(ctx, db, viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return viewer.canView(post.Post), nil
}

func legacyCanComment(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	post, err := getPostWithAuthor(ctx, db, postID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	viewer, err := legacyLoadViewerContext(ctx, db, viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !viewer.canView(post.Post) || post.WindowEndedAt != nil {
		return false, nil
	}
	if viewer.ID == post.AuthorID {
		return viewer.HasPostedToday, nil
	}
	if post.Visibility == "public" {
		return true, nil
	}
	return viewer.Friends[post.AuthorID], nil
}

func legacyBuildFeed(ctx context.Context, db *sqlx.DB, viewerID string, input getFeedRequest) (getFeedResponse, error) {
	ranked := input.Feed == "friends-of-friends"
	viewer, err := legacyLoadViewerContext(ctx, db, viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return getFeedResponse{Items: []feedItemResponse{}}, nil
	}
	if err != nil {
		return getFeedResponse{}, err
	}
	following := map[string]bool{}
	if input.Feed == "following" {
		var followeeIDs []string
		if err := db.SelectContext(ctx, &followeeIDs, `SELECT followee_id FROM follows WHERE follower_id = $1`, viewer.ID); err != nil {
			return getFeedResponse{}, err
		}
		for _, id := range followeeIDs {
			following[id] = true
		}
	}
	beforePostedAt, beforeID := input.BeforePostedAt, input.BeforeID
	if ranked {
		beforePostedAt, beforeID = nil, ""
	}
	posts, err := selectPostsWithAuthor(ctx, db, `
   AND p.author_id <> $1 AND w.ended_at IS NULL
   AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
   ORDER BY p.posted_at DESC, p.id DESC
  `, viewer.ID, beforePostedAt, beforeID)
	if err != nil {
		return getFeedResponse{}, err
	}
	candidates := make([]PostWithAuthor, 0)
	for _, post := range posts {
		if viewer.canView(post.Post) && legacyMatchesFeed(viewer, following, post.Post, input.Feed) {
			candidates = append(candidates, post)
		}
	}
	var scores map[string]int64
	if ranked {
		if scores, err = legacyRecommendationScores(ctx, db, viewer, candidates); err != nil {
			return getFeedResponse{}, err
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			return scores[candidates[i].ID] > scores[candidates[j].ID]
		})
	}
	items := make([]feedItemResponse, 0)
	for _, post := range candidates {
		score := scores[post.ID]
		if ranked && input.BeforeScore != nil && !rankedBefore(score, post.Post, *input.BeforeScore, *input.BeforePostedAt, input.BeforeID) {
			continue
		}
		item := feedItemResponse{createPostResponse: newPostResponse(post.Post, post.Author)}
		if ranked {
			item.Score = &score
		}
		items = append(items, item)
		if len(items) == 51 {
			break
		}
	}
	hasMore := len(items) > 50
	if hasMore {
		items = items[:50]
	}
	return getFeedResponse{Items: items, HasMore: hasMore}, nil
}

func legacyRecommendationScores(ctx context.Context, db sqlx.QueryerContext, viewer viewerContext, posts []PostWithAuthor) (map[string]int64, error) {
	scores := make(map[string]int64, len(posts))
	if len(posts) == 0 {
		return scores, nil
	}
	authorIDs := make([]string, 0, len(posts))
	postIDs := make([]string, 0, len(posts))
	for _, post := range posts {
		authorIDs = append(authorIDs, post.AuthorID)
		postIDs = append(postIDs, post.ID)
	}

	query, args, err := sqlx.In(`SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (?) OR user_high_id IN (?)`, authorIDs, authorIDs)
	if err != nil {
		return nil, err
	}
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
	}
	connections := map[string]int64{}
	for _, friendship := range friendships {
		connections[friendship.UserLowID] += int64(viewer.MutualFriends[friendship.UserHighID])
		connections[friendship.UserHighID] += int64(viewer.MutualFriends[friendship.UserLowID])
	}

	query, args, err = sqlx.In(`
  SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
  JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.post_id IN (?) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
  UNION
  SELECT c.post_id, c.author_id AS user_id FROM comments c
  JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.post_id IN (?) AND c.deleted_at IS NULL
 `, postIDs, postIDs)
	if err != nil {
		return nil, err
	}
	var engagements []struct {
		PostID string `db:"post_id"`
		UserID string `db:"user_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &engagements, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
	}
	friendEngagements := map[string]int64{}
	for _, engagement := range engagements {
		if viewer.Friends[engagement.UserID] {
			friendEngagements[engagement.PostID]++
		}
	}

	for _, post := range posts {
		scores[post.ID] = 10*friendEngagements[post.ID] + 3*int64(viewer.MutualFriends[post.AuthorID]) + min(connections[post.AuthorID], 30)
	}
	return scores, nil
}

func legacyMatchesFeed(viewer viewerContext, following map[string]bool, post Post, feed string) bool {
	switch feed {
	case "public":
		return post.Visibility == "public"
	case "following":
		return following[post.AuthorID]
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends-of-friends":
		return !viewer.Friends[post.AuthorID] && viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}

// ===== 比較テスト =====

// 署名付き画像URLの exp と sig は生成した秒によって変わるので、比較の前に取り除く。
var signedURLQuery = regexp.MustCompile(`\?exp=[0-9]+&sig=[A-Za-z0-9_-]+`)

func normalizedJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return signedURLQuery.ReplaceAllString(string(data), "")
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return value
	}
	return fallback
}

// TestEquivalenceWithLegacy は、DB の内容を変えずに読み取りだけで、旧実装と新実装の結果を比べる。
//
//	ISUREAL_TEST_DSN=postgres://isureal:isureal@127.0.0.1:5432/isureal go test -tags equivalence -run TestEquivalenceWithLegacy -v ./...
//
// 開いている投稿期間に投稿があるほど比較の意味が大きいので、負荷走行の直後の DB に対して実行するとよい。
// ISUREAL_TEST_SEED=1 を付けると、001-schema.sql だけを流した空の DB に合成データを入れてから比較する。
// ISUREAL_TEST_USERS（既定 300）、ISUREAL_TEST_POSTS（既定 300）、ISUREAL_TEST_PAGES（既定 3）で件数を変えられる。
func TestEquivalenceWithLegacy(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if os.Getenv("ISUREAL_TEST_SEED") == "1" {
		seedEquivalenceData(t, db)
	}

	var userIDs []string
	if err := db.SelectContext(ctx, &userIDs, `SELECT id FROM users ORDER BY md5(id) LIMIT $1`, envInt("ISUREAL_TEST_USERS", 300)); err != nil {
		t.Fatal(err)
	}
	// 開いている投稿期間の投稿を優先し、終了した期間の投稿と削除済みの投稿も混ぜる。
	var postIDs []string
	if err := db.SelectContext(ctx, &postIDs, `
  SELECT p.id FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  ORDER BY (w.ended_at IS NULL) DESC, md5(p.id) LIMIT $1
 `, envInt("ISUREAL_TEST_POSTS", 300)); err != nil {
		t.Fatal(err)
	}
	t.Logf("users=%d posts=%d", len(userIDs), len(postIDs))
	pages := envInt("ISUREAL_TEST_PAGES", 3)

	compared := map[string]int{}
	for _, userID := range userIDs {
		legacyViewer, legacyErr := legacyLoadViewerContext(ctx, db, userID)
		newViewer, newErr := loadViewerContext(ctx, db, userID)
		if errors.Is(legacyErr, sql.ErrNoRows) != errors.Is(newErr, sql.ErrNoRows) || (legacyErr == nil) != (newErr == nil) {
			t.Fatalf("loadViewerContext(%s) error: legacy=%v new=%v", userID, legacyErr, newErr)
		}
		if legacyErr == nil {
			if !reflect.DeepEqual(legacyViewer, newViewer) {
				t.Errorf("loadViewerContext(%s) differs:\nlegacy=%+v\nnew   =%+v", userID, legacyViewer, newViewer)
			}
			compared["viewer"]++
		}

		for _, feed := range []string{"friends", "friends-of-friends", "public", "following"} {
			input := getFeedRequest{Feed: feed}
			for page := 0; page < pages; page++ {
				legacy, err := legacyBuildFeed(ctx, db, userID, input)
				if err != nil {
					t.Fatal(err)
				}
				current, err := buildFeed(ctx, db, userID, input)
				if err != nil {
					t.Fatal(err)
				}
				if l, n := normalizedJSON(t, legacy), normalizedJSON(t, current); l != n {
					t.Errorf("feed %s user=%s page=%d differs:\nlegacy=%s\nnew   =%s", feed, userID, page, l, n)
					break
				}
				compared["feed:"+feed]++
				if len(legacy.Items) > 0 {
					compared["feed-nonempty:"+feed]++
				}
				if !legacy.HasMore {
					break
				}
				last := legacy.Items[len(legacy.Items)-1]
				postedAt := last.PostedAt
				input = getFeedRequest{Feed: feed, BeforePostedAt: &postedAt, BeforeID: last.ID, BeforeScore: last.Score}
			}
		}

		for _, postID := range postIDs {
			legacyView, err := legacyCanViewPost(ctx, db, userID, postID)
			if err != nil {
				t.Fatal(err)
			}
			newView, err := canViewPost(ctx, db, userID, postID)
			if err != nil {
				t.Fatal(err)
			}
			if legacyView != newView {
				t.Errorf("canViewPost(user=%s, post=%s): legacy=%v new=%v", userID, postID, legacyView, newView)
			}
			legacyComment, err := legacyCanComment(ctx, db, userID, postID)
			if err != nil {
				t.Fatal(err)
			}
			newComment, err := canComment(ctx, db, userID, postID)
			if err != nil {
				t.Fatal(err)
			}
			if legacyComment != newComment {
				t.Errorf("canComment(user=%s, post=%s): legacy=%v new=%v", userID, postID, legacyComment, newComment)
			}
			compared["post"]++
			if legacyView {
				compared["post-visible"]++
			}
		}
	}
	t.Logf("compared: %v", compared)
}

// seedEquivalenceData は空の DB に、友達・フォロー・ブロック・投稿期間・投稿・RealMoji・コメントの合成データを入れる。
// 乱数の種は固定なので、何度実行しても同じデータになる。
func seedEquivalenceData(t *testing.T, db *sqlx.DB) {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, query)
		}
	}
	regions := []string{"asia", "europe", "america"}
	const users = 80
	userID := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }
	for i := 0; i < users; i++ {
		accountType, status := "regular", "active"
		if i%10 == 0 {
			accountType = "certified_public"
		}
		if i%23 == 22 {
			status = "deleted"
		}
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region, account_type, status) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			userID(i), fmt.Sprintf("user%03d", i), fmt.Sprintf("%064x", i+1), fmt.Sprintf("User %d", i), regions[i%3], accountType, status)
	}
	for i := 0; i < users; i++ {
		for j := i + 1; j < users; j++ {
			low, high := min(userID(i), userID(j)), max(userID(i), userID(j))
			switch r := rng.Float64(); {
			case r < 0.08:
				exec(`INSERT INTO friendships (id, user_low_id, user_high_id) VALUES ($1,$2,$3)`, fmt.Sprintf("f-%d-%d", i, j), low, high)
			case r < 0.10:
				exec(`INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)`, fmt.Sprintf("b-%d-%d", i, j), userID(j), userID(i))
			}
		}
		for j := 0; j < users; j += 10 {
			if j != i && rng.Float64() < 0.4 {
				exec(`INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)`, fmt.Sprintf("fo-%d-%d", i, j), userID(i), userID(j))
			}
		}
	}
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	windowIDs := map[string][]string{}
	for _, region := range regions {
		for day := 0; day < 2; day++ {
			id := fmt.Sprintf("w-%s-%d", region, day)
			startsAt := base.Add(time.Duration(day) * 24 * time.Hour)
			var endedAt *time.Time
			if day == 0 {
				ended := startsAt.Add(24 * time.Hour)
				endedAt = &ended
			}
			exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1,$2,$3,$4,$5,$6)`,
				id, region, startsAt, startsAt, startsAt.Add(2*time.Minute), endedAt)
			windowIDs[region] = append(windowIDs[region], id)
		}
	}
	visibilities := []string{"public", "friends", "friends_of_friends"}
	var postIDs []string
	for i := 0; i < users; i++ {
		for day, windowID := range windowIDs[regions[i%3]] {
			// 2日目は一部のユーザーが投稿していない（HasPostedToday が false になる）。
			if day == 1 && rng.Float64() < 0.2 {
				continue
			}
			id := fmt.Sprintf("p-%d-%d", i, day)
			postedAt := base.Add(time.Duration(day)*24*time.Hour + time.Duration(rng.IntN(3600))*time.Second + time.Duration(rng.IntN(1000))*time.Microsecond)
			var deletedAt *time.Time
			if rng.Float64() < 0.05 {
				deletedAt = &postedAt
			}
			exec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at, deleted_at) VALUES ($1,$2,$3,$4,'',$5,'none',$6,$7)`,
				id, userID(i), windowID, "img-"+id, visibilities[rng.IntN(3)], postedAt, deletedAt)
			postIDs = append(postIDs, id)
		}
	}
	for n, postID := range postIDs {
		for k := 0; k < 4; k++ {
			reactor := userID(rng.IntN(users))
			at := base.Add(time.Duration(n*10+k) * time.Second)
			if rng.Float64() < 0.5 {
				exec(`INSERT INTO comments (id, post_id, author_id, body, created_at) VALUES ($1,$2,$3,'hi',$4) ON CONFLICT DO NOTHING`,
					fmt.Sprintf("c-%d-%d", n, k), postID, reactor, at)
			} else {
				exec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at) VALUES ($1,$2,$3,$4,'like',$5) ON CONFLICT DO NOTHING`,
					fmt.Sprintf("r-%d-%d", n, k), postID, reactor, fmt.Sprintf("ri-%d-%d", n, k), at)
			}
		}
	}
}
