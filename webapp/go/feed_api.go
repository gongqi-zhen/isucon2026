package isureal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getFeedRequest struct {
	Feed           string     `param:"feed"`
	BeforeScore    *int64     `query:"beforeScore"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type feedItemResponse struct {
	createPostResponse
	Score *int64 `json:"score,omitempty"`
}

type getFeedResponse struct {
	Items   []feedItemResponse `json:"items"`
	HasMore bool               `json:"hasMore"`
}

func getFeedHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getFeedRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		switch input.Feed {
		case "friends", "friends-of-friends", "public", "following":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if (input.BeforePostedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ranked := input.Feed == "friends-of-friends"
		if ranked && (input.BeforeScore == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		response, err := buildFeed(c.Request().Context(), h.DB, GetSession(c).UserID, input)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

// buildFeed は入力の検証が済んだフィードの要求に対する応答を組み立てる。
func buildFeed(ctx context.Context, db sqlx.QueryerContext, viewerID string, input getFeedRequest) (getFeedResponse, error) {
	ranked := input.Feed == "friends-of-friends"
	viewer, err := loadViewerContext(ctx, db, viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return getFeedResponse{Items: []feedItemResponse{}}, nil
	}
	if err != nil {
		return getFeedResponse{}, err
	}
	beforePostedAt, beforeID := input.BeforePostedAt, input.BeforeID
	if ranked {
		beforePostedAt, beforeID = nil, ""
	}
	posts, err := selectFeedCandidates(ctx, db, viewer, input.Feed, beforePostedAt, beforeID, !ranked)
	if err != nil {
		return getFeedResponse{}, err
	}
	// SQL と同じ条件で Go 側でも確認し、閲覧権限のない投稿を返さないようにする。
	candidates := make([]PostWithAuthor, 0, len(posts))
	for _, post := range posts {
		if viewer.canView(post.Post) && matchesFeed(viewer, post.Post, input.Feed) {
			candidates = append(candidates, post)
		}
	}
	var scores map[string]int64
	if ranked {
		if scores, err = recommendationScores(ctx, db, viewer, candidates); err != nil {
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

// selectFeedCandidates は、viewer.canView と matchesFeed を満たす投稿を
// posted_at DESC, id DESC の順で SQL で絞り込んで返す。limited のときは先頭51件だけを返す。
func selectFeedCandidates(ctx context.Context, db sqlx.QueryerContext, viewer viewerContext, feed string, beforePostedAt *time.Time, beforeID string, limited bool) ([]PostWithAuthor, error) {
	// 投稿者が自分以外なので、今日まだ投稿していなければ canView はすべて false になる。
	if !viewer.HasPostedToday {
		return nil, nil
	}
	friends := mapKeys(viewer.Friends)
	friendsOrFriendsOfFriends := mapKeys(viewer.Friends, viewer.FriendsOfFriends)
	args := []any{viewer.ID, mapKeys(viewer.Blocked), friends, friendsOrFriendsOfFriends}
	feedCondition := ""
	switch feed {
	case "friends":
		if len(friends) == 0 {
			return nil, nil
		}
		feedCondition = `AND p.author_id = ANY($3::text[])`
	case "friends-of-friends":
		friendsOfFriendsOnly := make([]string, 0, len(viewer.FriendsOfFriends))
		for id := range viewer.FriendsOfFriends {
			if !viewer.Friends[id] {
				friendsOfFriendsOnly = append(friendsOfFriendsOnly, id)
			}
		}
		if len(friendsOfFriendsOnly) == 0 {
			return nil, nil
		}
		args = append(args, friendsOfFriendsOnly)
		feedCondition = fmt.Sprintf(`AND p.author_id = ANY($%d::text[])`, len(args))
	case "following":
		feedCondition = `AND p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)`
	case "public":
		feedCondition = `AND p.visibility = 'public'`
	}
	args = append(args, beforePostedAt, beforeID)
	limit := ""
	if limited {
		limit = "LIMIT 51"
	}
	return selectPostsWithAuthor(ctx, db, fmt.Sprintf(`
   AND p.author_id <> $1 AND w.ended_at IS NULL
   AND NOT (p.author_id = ANY($2::text[]))
   AND (p.visibility = 'public'
     OR (p.visibility = 'friends' AND p.author_id = ANY($3::text[]))
     OR (p.visibility = 'friends_of_friends' AND p.author_id = ANY($4::text[])))
   %s
   AND ($%d::timestamptz IS NULL OR (p.posted_at, p.id) < ($%d, $%d))
   ORDER BY p.posted_at DESC, p.id DESC
   %s
  `, feedCondition, len(args)-1, len(args)-1, len(args), limit), args...)
}

// mapKeys は値が true のキーの和集合を返す。SQL の配列パラメータに使うので、空でも nil にはしない。
func mapKeys(sets ...map[string]bool) []string {
	seen := map[string]bool{}
	keys := make([]string, 0)
	for _, set := range sets {
		for key, ok := range set {
			if ok && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func rankedBefore(score int64, post Post, beforeScore int64, beforePostedAt time.Time, beforeID string) bool {
	if score != beforeScore {
		return score < beforeScore
	}
	if !post.PostedAt.Equal(beforePostedAt) {
		return post.PostedAt.Before(beforePostedAt)
	}
	return post.ID < beforeID
}

func recommendationScores(ctx context.Context, db sqlx.QueryerContext, viewer viewerContext, posts []PostWithAuthor) (map[string]int64, error) {
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

	// kind='f' は作者の友達関係（a=user_low_id, b=user_high_id）、
	// kind='e' は投稿へのリアクションかコメント（a=post_id, b=user_id）の重複なしの組。
	var rows []struct {
		Kind string `db:"kind"`
		A    string `db:"a"`
		B    string `db:"b"`
	}
	if err := sqlx.SelectContext(ctx, db, &rows, `
  SELECT 'f' AS kind, user_low_id AS a, user_high_id AS b FROM friendships
  WHERE user_low_id = ANY($1::text[]) OR user_high_id = ANY($1::text[])
  UNION ALL
  (
    SELECT 'e', r.post_id, r.reactor_id FROM realmojis r
    JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
    WHERE r.post_id = ANY($2::text[]) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
    UNION
    SELECT 'e', c.post_id, c.author_id FROM comments c
    JOIN users u ON u.id = c.author_id AND u.status = 'active'
    WHERE c.post_id = ANY($2::text[]) AND c.deleted_at IS NULL
  )
 `, authorIDs, postIDs); err != nil {
		return nil, err
	}
	connections := map[string]int64{}
	friendEngagements := map[string]int64{}
	for _, row := range rows {
		switch row.Kind {
		case "f":
			connections[row.A] += int64(viewer.MutualFriends[row.B])
			connections[row.B] += int64(viewer.MutualFriends[row.A])
		case "e":
			if viewer.Friends[row.B] {
				friendEngagements[row.A]++
			}
		}
	}

	for _, post := range posts {
		scores[post.ID] = 10*friendEngagements[post.ID] + 3*int64(viewer.MutualFriends[post.AuthorID]) + min(connections[post.AuthorID], 30)
	}
	return scores, nil
}

// matchesFeed は SQL での絞り込みを Go 側で再確認する。following は SQL の follows で絞り込み済み。
func matchesFeed(viewer viewerContext, post Post, feed string) bool {
	switch feed {
	case "public":
		return post.Visibility == "public"
	case "following":
		return true
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends-of-friends":
		return !viewer.Friends[post.AuthorID] && viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}
