package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

type PostLocation struct {
	Kind      string   `json:"kind" db:"location_kind"`
	Latitude  *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude *float64 `json:"longitude,omitempty" db:"longitude"`
	Locality  *string  `json:"locality,omitempty" db:"locality"`
}

func (location PostLocation) valid() bool {
	switch location.Kind {
	case "none":
		return location.Latitude == nil && location.Longitude == nil && location.Locality == nil
	case "exact":
		return location.Latitude != nil && location.Longitude != nil && location.Locality == nil &&
			*location.Latitude >= -90 && *location.Latitude <= 90 && *location.Longitude >= -180 && *location.Longitude <= 180
	case "approximate":
		return location.Latitude == nil && location.Longitude == nil && location.Locality != nil && validUserText(*location.Locality, 1, 100)
	default:
		return false
	}
}

type Post struct {
	ID              string `db:"id"`
	AuthorID        string `db:"author_id"`
	PostingWindowID string `db:"posting_window_id"`
	ImageID         string `db:"image_id"`
	Caption         string `db:"caption"`
	Visibility      string `db:"visibility"`
	PostLocation
	PostedAt       time.Time  `db:"posted_at"`
	OnTimeDeadline time.Time  `db:"on_time_deadline"`
	WindowEndedAt  *time.Time `db:"window_ended_at"`
}

const postColumns = `p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
  p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
  w.on_time_deadline, w.ended_at AS window_ended_at`

func getPost(ctx context.Context, db sqlx.QueryerContext, postID string) (Post, error) {
	var post Post
	err := sqlx.GetContext(ctx, db, &post, `
  SELECT `+postColumns+`
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = $1 AND p.deleted_at IS NULL
 `, postID)
	return post, err
}

type PostWithAuthor struct {
	Post
	Author User `db:"author"`
}

func selectPostsWithAuthor(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]PostWithAuthor, error) {
	var posts []PostWithAuthor
	if err := sqlx.SelectContext(ctx, db, &posts, `
  SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL `+conditions, args...); err != nil {
		return nil, err
	}
	for i := range posts {
		if err := posts[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return posts, nil
}

func getPostWithAuthor(ctx context.Context, db sqlx.QueryerContext, postID string) (PostWithAuthor, error) {
	posts, err := selectPostsWithAuthor(ctx, db, `AND p.id = $1`, postID)
	if err != nil {
		return PostWithAuthor{}, err
	}
	if len(posts) == 0 {
		return PostWithAuthor{}, sql.ErrNoRows
	}
	return posts[0], nil
}

type viewerContext struct {
	User
	HasPostedToday   bool
	Friends          map[string]bool
	FriendsOfFriends map[string]bool
	MutualFriends    map[string]int
	Blocked          map[string]bool
}

// viewerFriendsCTE は $1 の閲覧者の友達IDを viewer_friends として定義する。
const viewerFriendsCTE = `viewer_friends AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  )`

type viewerContextRow struct {
	User
	HasPostedToday      bool   `db:"has_posted_today"`
	BlockedIDs          string `db:"blocked_ids"`
	FriendIDs           string `db:"friend_ids"`
	FriendsOfFriendsIDs string `db:"friends_of_friends_ids"`
}

// loadViewerContext は閲覧者の情報を1回のクエリでまとめて取得する。
func loadViewerContext(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}, Blocked: map[string]bool{}}
	var row viewerContextRow
	if err := sqlx.GetContext(ctx, db, &row, `
  WITH `+viewerFriendsCTE+`
  SELECT `+qualifiedUserColumns("u")+`,
    EXISTS(
      SELECT 1 FROM posts p
      JOIN posting_windows posted ON posted.id = p.posting_window_id
      JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE p.author_id = u.id AND p.deleted_at IS NULL
    ) AS has_posted_today,
    to_json(ARRAY(
      SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
      FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
    ))::text AS blocked_ids,
    to_json(ARRAY(SELECT id FROM viewer_friends))::text AS friend_ids,
    to_json(ARRAY(
      SELECT f.user_high_id FROM friendships f WHERE f.user_low_id IN (SELECT id FROM viewer_friends)
      UNION ALL
      SELECT f.user_low_id FROM friendships f WHERE f.user_high_id IN (SELECT id FROM viewer_friends)
    ))::text AS friends_of_friends_ids
  FROM users u WHERE u.id = $1 AND u.status = 'active'
 `, viewerID); err != nil {
		return viewer, err
	}
	viewer.User = row.User
	if err := viewer.User.setImageKey(); err != nil {
		return viewer, err
	}
	viewer.HasPostedToday = row.HasPostedToday
	if err := viewer.applyRelationLists(row.BlockedIDs, row.FriendIDs, row.FriendsOfFriendsIDs); err != nil {
		return viewer, err
	}
	return viewer, nil
}

// applyRelationLists は、SQL で JSON の配列にしたブロック・友達・友達の友達の ID を viewer に入れる。
// friendsOfFriendsJSON は「友達を片側に持つ友達関係」の反対側を、関係ごとに1件ずつ含むこと（MutualFriends の数に使う）。
func (viewer *viewerContext) applyRelationLists(blockedJSON, friendsJSON, friendsOfFriendsJSON string) error {
	var blocked, friends, friendsOfFriends []string
	for _, list := range []struct {
		raw string
		ids *[]string
	}{{blockedJSON, &blocked}, {friendsJSON, &friends}, {friendsOfFriendsJSON, &friendsOfFriends}} {
		if err := json.Unmarshal([]byte(list.raw), list.ids); err != nil {
			return err
		}
	}
	for _, id := range blocked {
		viewer.Blocked[id] = true
	}
	for _, id := range friends {
		viewer.Friends[id] = true
	}
	// friends_of_friends_ids は「友達を片側に持つ友達関係」の反対側を、関係ごとに1件ずつ含む。
	for _, id := range friendsOfFriends {
		viewer.FriendsOfFriends[id] = true
		if id != viewer.ID {
			viewer.MutualFriends[id]++
		}
	}
	return nil
}

func (viewer viewerContext) canView(post Post) bool {
	if post.AuthorID == viewer.ID {
		return true
	}
	if post.WindowEndedAt != nil || !viewer.HasPostedToday || viewer.Blocked[post.AuthorID] {
		return false
	}
	switch post.Visibility {
	case "public":
		return true
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends_of_friends":
		return viewer.Friends[post.AuthorID] || viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}

// postAccess は1件の投稿と、その作者に対する閲覧者の関係をまとめたもの。
type postAccess struct {
	PostWithAuthor
	ViewerActive         bool `db:"viewer_active"`
	ViewerHasPostedToday bool `db:"viewer_has_posted_today"`
	Blocked              bool `db:"blocked"`
	Friend               bool `db:"friend"`
	FriendOfFriend       bool `db:"friend_of_friend"`
}

// loadPostAccess は投稿と、権限判定に必要な閲覧者の情報を1回のクエリで取得する。
// 投稿がない場合は sql.ErrNoRows を返す。
func loadPostAccess(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (postAccess, error) {
	var access postAccess
	err := sqlx.GetContext(ctx, db, &access, `
  WITH viewer AS (SELECT id, region FROM users WHERE id = $1 AND status = 'active'),
  `+viewerFriendsCTE+`
  SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`,
    EXISTS(SELECT 1 FROM viewer) AS viewer_active,
    EXISTS(
      SELECT 1 FROM posts vp
      JOIN viewer ON viewer.id = vp.author_id
      JOIN posting_windows posted ON posted.id = vp.posting_window_id
      JOIN posting_windows current ON current.region = viewer.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE vp.deleted_at IS NULL
    ) AS viewer_has_posted_today,
    EXISTS(
      SELECT 1 FROM block_relations b
      WHERE (b.blocker_id = $1 AND b.blocked_id = p.author_id) OR (b.blocker_id = p.author_id AND b.blocked_id = $1)
    ) AS blocked,
    EXISTS(SELECT 1 FROM viewer_friends vf WHERE vf.id = p.author_id) AS friend,
    EXISTS(
      SELECT 1 FROM friendships af JOIN viewer_friends vf
        ON (af.user_low_id = vf.id AND af.user_high_id = p.author_id)
        OR (af.user_high_id = vf.id AND af.user_low_id = p.author_id)
    ) AS friend_of_friend
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL AND p.id = $2
 `, viewerID, postID)
	if err != nil {
		return access, err
	}
	return access, access.Author.setImageKey()
}

// viewer は loadViewerContext と同じ判定結果になるよう、この投稿の作者の分だけを持つ viewerContext を返す。
func (access postAccess) viewer(viewerID string) viewerContext {
	viewer := viewerContext{
		User: User{ID: viewerID}, HasPostedToday: access.ViewerHasPostedToday,
		Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}, Blocked: map[string]bool{},
	}
	if access.Blocked {
		viewer.Blocked[access.AuthorID] = true
	}
	if access.Friend {
		viewer.Friends[access.AuthorID] = true
	}
	if access.FriendOfFriend {
		viewer.FriendsOfFriends[access.AuthorID] = true
	}
	return viewer
}

func (access postAccess) canView(viewerID string) bool {
	return access.ViewerActive && access.viewer(viewerID).canView(access.Post)
}

func canViewPost(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	access, err := loadPostAccess(ctx, db, viewerID, postID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return access.canView(viewerID), nil
}

func getFriendIDs(ctx context.Context, db sqlx.QueryerContext, userID string) ([]string, error) {
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1`, userID); err != nil {
		return nil, err
	}
	var ids []string
	for _, friendship := range friendships {
		if friendship.UserLowID == userID {
			ids = append(ids, friendship.UserHighID)
		} else {
			ids = append(ids, friendship.UserLowID)
		}
	}
	return ids, nil
}

func getBlockedUserIDs(ctx context.Context, db sqlx.QueryerContext, userID string) (map[string]bool, error) {
	var ids []string
	if err := sqlx.SelectContext(ctx, db, &ids, `
  SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
 `, userID); err != nil {
		return nil, err
	}
	blocked := make(map[string]bool, len(ids))
	for _, id := range ids {
		blocked[id] = true
	}
	return blocked, nil
}
