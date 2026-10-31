package isureal

import (
	"context"
	"fmt"
	"net/http"
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
// 絞り込み、並べ替え、ページ送り、51件の切り出しは selectFeedCandidates の SQL で済んでいる（#107）。
// ここでは、閲覧権限のない投稿を返さないための Go 側の再確認と、応答への変換だけを行う。
func buildFeed(ctx context.Context, db sqlx.QueryerContext, viewerID string, input getFeedRequest) (getFeedResponse, error) {
	ranked := input.Feed == "friends-of-friends"
	posts, err := selectFeedCandidates(ctx, db, viewerID, input)
	if err != nil {
		return getFeedResponse{}, err
	}
	items := make([]feedItemResponse, 0, len(posts))
	for _, post := range posts {
		// SQL と同じ条件で Go 側でも確認し、閲覧権限のない投稿を返さないようにする。
		viewer := post.viewer(viewerID)
		if !viewer.canView(post.Post) || !matchesFeed(viewer, post.Post, input.Feed) {
			continue
		}
		item := feedItemResponse{createPostResponse: newPostResponse(post.Post, post.Author)}
		if ranked {
			score := recommendationScore(post)
			if score != post.Score {
				return getFeedResponse{}, fmt.Errorf("feed candidates: score of post %s differs: sql=%d go=%d", post.ID, post.Score, score)
			}
			item.Score = &score
		}
		items = append(items, item)
	}
	hasMore := len(items) > 50
	if hasMore {
		items = items[:50]
	}
	return getFeedResponse{Items: items, HasMore: hasMore}, nil
}

// feedCandidate は候補の投稿と、閲覧者から見た作者の関係、friends-of-friends のスコアに使う数。
// ほかのフィードでは、スコアに使う数と Score は 0 のまま。
type feedCandidate struct {
	PostWithAuthor
	// Blocked、Friend、FriendOfFriend は、閲覧者と作者の関係（loadPostAccess の同名の列と同じ）。
	Blocked        bool `db:"blocked"`
	Friend         bool `db:"friend"`
	FriendOfFriend bool `db:"friend_of_friend"`
	// MutualFriends は、閲覧者と作者の共通の友達の数（viewerContext.MutualFriends[author] と同じ）。
	MutualFriends int64 `db:"mutual_friends"`
	// FriendEngagements は、閲覧者の友達のうち、この投稿に RealMoji かコメントを付けた active な利用者の数。
	FriendEngagements int64 `db:"friend_engagements"`
	// AuthorConnections は、作者の友達それぞれについての、閲覧者との共通の友達の数の合計（上限をかける前）。
	AuthorConnections int64 `db:"author_connections"`
	// Score は SQL で計算した recommendationScore と同じ値。並べ替えとページ送りに使う。
	Score int64 `db:"score"`
}

// viewer は postAccess.viewer と同じく、この投稿の作者の分だけの関係を持つ viewerContext を返す。
// 候補が返っているので、閲覧者は active で今日投稿している（selectFeedCandidates の viewer CTE の条件）。
func (c feedCandidate) viewer(viewerID string) viewerContext {
	viewer := viewerContext{
		User: User{ID: viewerID}, HasPostedToday: true,
		Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}, Blocked: map[string]bool{},
	}
	if c.Blocked {
		viewer.Blocked[c.AuthorID] = true
	}
	if c.Friend {
		viewer.Friends[c.AuthorID] = true
	}
	if c.FriendOfFriend {
		viewer.FriendsOfFriends[c.AuthorID] = true
	}
	viewer.MutualFriends[c.AuthorID] = int(c.MutualFriends)
	return viewer
}

// selectFeedCandidates は、viewer.canView と matchesFeed を満たす投稿を SQL で絞り込み、
// 応答の順（friends-of-friends はスコア DESC, posted_at DESC, id DESC、ほかは posted_at DESC, id DESC）で
// ページ送りの位置より後の先頭51件を返す。閲覧者の関係（ブロック、友達、友達の友達）も同じクエリで求めるので、DB への往復は1回で済む。
//
// 閲覧者の関係の一覧（ブロック・友達・友達の友達の全 ID）は SQL の中だけで使い、応答には候補ごとの判定（blocked、friend、
// friend_of_friend、mutual_friends）だけを返す（#107）。以前は1行目に全 ID を JSON で付けていたので、友達の友達が多い閲覧者では
// 1回のフィードで数十〜数百KB を転送し、Go で map にしていた。
// friends-of-friends では、スコアに使う数（feedCandidate のコメント）とスコアも同じクエリで投稿ごとに数え、
// 並べ替えと51件の切り出しも SQL で行う。以前は候補を全部読んで Go で並べ替えていた。
// 閲覧者が active でないか今日まだ投稿していなければ、候補は0件になる（canView はすべて false なので）。
func selectFeedCandidates(ctx context.Context, db sqlx.QueryerContext, viewerID string, input getFeedRequest) ([]feedCandidate, error) {
	ranked := input.Feed == "friends-of-friends"
	feedCondition := ""
	switch input.Feed {
	case "friends":
		feedCondition = `AND p.author_id = ANY(viewer.friends)`
	case "friends-of-friends":
		feedCondition = `AND p.author_id = ANY(viewer.friends_of_friends) AND NOT (p.author_id = ANY(viewer.friends))`
	case "following":
		feedCondition = `AND p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)`
	case "public":
		feedCondition = `AND p.visibility = 'public'`
	}
	// スコアの数は、旧実装（候補の作者の友達関係と、候補への RealMoji・コメントの重複なしの組を
	// 別のクエリで読み、Go で数える）と同じ数になるように、投稿ごとに集計して返す。
	// 行のまま返すと、作者の友達の数と反応の数だけ行が増えるので、リージョンをまたぐ転送の量を抑えるために数だけにする。
	// mutual は viewerContext.MutualFriends と同じ（友達の友達の配列に現れる回数。閲覧者自身は除く）。
	//
	// ページ送り：posted_at の順のフィードは candidates の中で ($2, $3) より前に絞り、51件に切る。
	// friends-of-friends はスコアを求めてから、rankedBefore と同じ (score, posted_at, id) < ($4, $5, $6) で絞り、51件に切る。
	// 並び順は Go の sort.SliceStable（posted_at DESC, id DESC の候補をスコア DESC で安定に並べ替える）と同じになる。
	candidateLimit := "LIMIT 51"
	selection := `
  SELECT c.*, 0::bigint AS mutual_friends, 0::bigint AS friend_engagements, 0::bigint AS author_connections, 0::bigint AS score
  FROM candidates c
  ORDER BY c.posted_at DESC, c.id DESC`
	args := []any{viewerID, input.BeforePostedAt, input.BeforeID}
	if ranked {
		candidateLimit = ""
		selection = `,
  mutual AS (
    SELECT fof.id, count(*) AS n
    FROM viewer, unnest(viewer.friends_of_friends) AS fof(id)
    WHERE fof.id <> $1
    GROUP BY fof.id
  ),
  scored AS (
    SELECT c.*, COALESCE(m.n, 0)::bigint AS mutual_friends, engagements.n AS friend_engagements, connections.n AS author_connections,
      (10 * engagements.n + 3 * COALESCE(m.n, 0) + LEAST(connections.n, 30))::bigint AS score
    FROM candidates c
    CROSS JOIN viewer
    LEFT JOIN mutual m ON m.id = c.author_id
    CROSS JOIN LATERAL (
      SELECT count(*) AS n FROM (
        SELECT r.reactor_id AS user_id FROM realmojis r
        JOIN users ru ON ru.id = r.reactor_id AND ru.status = 'active'
        WHERE r.post_id = c.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
        UNION
        SELECT cm.author_id FROM comments cm
        JOIN users cu ON cu.id = cm.author_id AND cu.status = 'active'
        WHERE cm.post_id = c.id AND cm.deleted_at IS NULL
      ) e
      WHERE e.user_id = ANY(viewer.friends)
    ) engagements
    CROSS JOIN LATERAL (
      SELECT COALESCE(sum(m.n), 0)::bigint AS n
      FROM friendships f
      JOIN mutual m ON m.id = CASE WHEN f.user_low_id = c.author_id THEN f.user_high_id ELSE f.user_low_id END
      WHERE f.user_low_id = c.author_id OR f.user_high_id = c.author_id
    ) connections
  )
  SELECT s.*
  FROM scored s
  WHERE ($4::bigint IS NULL OR (s.score, s.posted_at, s.id) < ($4, $5::timestamptz, $6))
  ORDER BY s.score DESC, s.posted_at DESC, s.id DESC
  LIMIT 51`
		// candidates の ($2, $3) の絞り込みは使わない（スコアの順でページ送りする）。
		args = []any{viewerID, nil, "", input.BeforeScore, input.BeforePostedAt, input.BeforeID}
	}
	var posts []feedCandidate
	if err := sqlx.SelectContext(ctx, db, &posts, fmt.Sprintf(`
  WITH `+viewerFriendsCTE+`,
  viewer AS (
    SELECT
      ARRAY(
        SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
        FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
      ) AS blocked,
      ARRAY(SELECT id FROM viewer_friends) AS friends,
      ARRAY(
        SELECT f.user_high_id FROM friendships f WHERE f.user_low_id IN (SELECT id FROM viewer_friends)
        UNION ALL
        SELECT f.user_low_id FROM friendships f WHERE f.user_high_id IN (SELECT id FROM viewer_friends)
      ) AS friends_of_friends
    FROM users vu
    WHERE vu.id = $1 AND vu.status = 'active' AND EXISTS(
      SELECT 1 FROM posts vp
      JOIN posting_windows posted ON posted.id = vp.posting_window_id
      JOIN posting_windows current ON current.region = vu.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE vp.author_id = vu.id AND vp.deleted_at IS NULL
    )
  ),
  candidates AS (
    SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`,
      (p.author_id = ANY(viewer.blocked)) AS blocked,
      (p.author_id = ANY(viewer.friends)) AS friend,
      (p.author_id = ANY(viewer.friends_of_friends)) AS friend_of_friend
    FROM viewer
    JOIN posts p ON p.deleted_at IS NULL AND p.author_id <> $1
    JOIN posting_windows w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE w.ended_at IS NULL
      AND NOT (p.author_id = ANY(viewer.blocked))
      AND (p.visibility = 'public'
        OR (p.visibility = 'friends' AND p.author_id = ANY(viewer.friends))
        OR (p.visibility = 'friends_of_friends'
          AND (p.author_id = ANY(viewer.friends) OR p.author_id = ANY(viewer.friends_of_friends))))
      %s
      AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
    ORDER BY p.posted_at DESC, p.id DESC
    %s
  )%s
 `, feedCondition, candidateLimit, selection), args...); err != nil {
		return nil, err
	}
	for i := range posts {
		if err := posts[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return posts, nil
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

// rankedBefore は friends-of-friends のページ送りの判定。selectFeedCandidates は同じ比較を
// SQL の (score, posted_at, id) < (beforeScore, beforePostedAt, beforeID) で行う。
func rankedBefore(score int64, post Post, beforeScore int64, beforePostedAt time.Time, beforeID string) bool {
	if score != beforeScore {
		return score < beforeScore
	}
	if !post.PostedAt.Equal(beforePostedAt) {
		return post.PostedAt.Before(beforePostedAt)
	}
	return post.ID < beforeID
}

// recommendationScore は friends-of-friends のスコア。数は selectFeedCandidates が同じクエリで数えている。
// SQL の score 列も同じ式で計算していて、buildFeed が一致を確かめる。
func recommendationScore(post feedCandidate) int64 {
	return 10*post.FriendEngagements + 3*post.MutualFriends + min(post.AuthorConnections, 30)
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
