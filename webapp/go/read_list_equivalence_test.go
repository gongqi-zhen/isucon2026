//go:build equivalence

// #65 のプロフィールと RealMoji・コメントの一覧を、変更前の実装（権限の確認と一覧を別々のクエリで読む）と、同じ DB に対して比べる。
//
//	ISUREAL_TEST_DSN=postgres://isureal:isureal@127.0.0.1:5432/isureal ISUREAL_TEST_SEED=1 go test -tags equivalence -run TestReadListEquivalence -v ./...
//
// ISUREAL_TEST_SEED=<正の整数> を付けると、001-schema.sql、migrate.sql、interaction_functions.sql だけを流した空の DB に、
// seedEquivalenceData の合成データと、削除・置き換えた RealMoji とコメント、自己紹介を入れてから比べる。
// 付けなければ、DB を変えずに読み取りだけで比べる。
// ISUREAL_TEST_USERS（既定 80）、ISUREAL_TEST_POSTS（既定 200）で件数を変えられる。

package isureal

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// ===== 旧実装（#65 の変更前のコードをそのまま写したもの） =====

func legacyLoadProfileUser(ctx context.Context, db sqlx.QueryerContext, viewerID, userID string) (User, error) {
	user, err := getUser(ctx, db, userID)
	if err != nil {
		return user, err
	}
	blocked, err := isBlocked(ctx, db, viewerID, user.ID)
	if err != nil {
		return user, err
	}
	if blocked {
		return User{}, sql.ErrNoRows
	}
	friends, err := getFriendIDs(ctx, db, viewerID)
	if err != nil {
		return user, err
	}
	if user.AccountType != "certified_public" && !slices.Contains(friends, user.ID) {
		user.Bio = ""
	}
	return user, nil
}

func legacyListComments(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string, after *time.Time, afterID string) (bool, []CommentWithAuthor, error) {
	allowed, err := canComment(ctx, db, viewerID, postID)
	if err != nil || !allowed {
		return false, nil, err
	}
	comments, err := selectCommentsWithAuthor(ctx, db, `AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51`, postID, after, afterID)
	return true, comments, err
}

func legacyListRealMojis(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string, before *time.Time, beforeID string) (bool, []RealMojiWithReactor, error) {
	allowed, err := canViewPost(ctx, db, viewerID, postID)
	if err != nil || !allowed {
		return false, nil, err
	}
	reactions, err := selectCurrentRealMojis(ctx, db, `AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51`, postID, before, beforeID)
	return true, reactions, err
}

func commentResponses(comments []CommentWithAuthor) []createCommentResponse {
	responses := make([]createCommentResponse, 0, len(comments))
	for _, comment := range comments {
		responses = append(responses, newCommentResponse(comment.Comment, comment.Author))
	}
	return responses
}

func realMojiResponses(reactions []RealMojiWithReactor) []replaceRealMojiResponse {
	responses := make([]replaceRealMojiResponse, 0, len(reactions))
	for _, reaction := range reactions {
		responses = append(responses, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
	}
	return responses
}

func TestReadListEquivalence(t *testing.T) {
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
	if seed := envInt("ISUREAL_TEST_SEED", 0); seed > 0 {
		seedEquivalenceData(t, db, uint64(seed))
		// 一覧の条件（削除、置き換え）と、自己紹介を空にする条件を確かめるためのデータを足す。
		db.MustExec(`UPDATE comments SET deleted_at = created_at WHERE hashtext(id) & 7 = 0`)
		db.MustExec(`UPDATE realmojis SET replaced_at = created_at WHERE hashtext(id) & 7 = 1`)
		db.MustExec(`UPDATE realmojis SET deleted_at = created_at WHERE hashtext(id) & 7 = 2`)
		db.MustExec(`UPDATE users SET bio = 'bio of ' || username`)
	}

	var userIDs []string
	if err := db.SelectContext(ctx, &userIDs, `SELECT id FROM users ORDER BY md5(id) LIMIT $1`, envInt("ISUREAL_TEST_USERS", 80)); err != nil {
		t.Fatal(err)
	}
	var postIDs []string
	if err := db.SelectContext(ctx, &postIDs, `
  SELECT p.id FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  ORDER BY (w.ended_at IS NULL) DESC, md5(p.id) LIMIT $1
 `, envInt("ISUREAL_TEST_POSTS", 200)); err != nil {
		t.Fatal(err)
	}
	t.Logf("users=%d posts=%d", len(userIDs), len(postIDs))

	compared := map[string]int{}
	for _, viewerID := range userIDs {
		for _, userID := range userIDs {
			legacy, legacyErr := legacyLoadProfileUser(ctx, db, viewerID, userID)
			current, currentErr := loadProfileUser(ctx, db, viewerID, userID)
			if errors.Is(legacyErr, sql.ErrNoRows) != errors.Is(currentErr, sql.ErrNoRows) || (legacyErr == nil) != (currentErr == nil) {
				t.Fatalf("profile(viewer=%s, user=%s) error: legacy=%v new=%v", viewerID, userID, legacyErr, currentErr)
			}
			if legacyErr != nil {
				compared["profile-not-found"]++
				continue
			}
			if l, n := normalizedJSON(t, newRelationshipUserResponse(legacy)), normalizedJSON(t, newRelationshipUserResponse(current)); l != n {
				t.Errorf("profile(viewer=%s, user=%s) differs:\nlegacy=%s\nnew   =%s", viewerID, userID, l, n)
			}
			compared["profile"]++
			if current.Bio != "" {
				compared["profile-with-bio"]++
			}
		}

		for _, postID := range postIDs {
			var after *time.Time
			afterID := ""
			for page := range 2 {
				legacyAllowed, legacyComments, err := legacyListComments(ctx, db, viewerID, postID, after, afterID)
				if err != nil {
					t.Fatal(err)
				}
				allowed, comments, err := listCommentsIfCommentable(ctx, db, viewerID, postID, after, afterID)
				if err != nil {
					t.Fatal(err)
				}
				if legacyAllowed != allowed {
					t.Errorf("comments(viewer=%s, post=%s, page=%d) allowed: legacy=%v new=%v", viewerID, postID, page, legacyAllowed, allowed)
					break
				}
				if l, n := normalizedJSON(t, commentResponses(legacyComments)), normalizedJSON(t, commentResponses(comments)); l != n {
					t.Errorf("comments(viewer=%s, post=%s, page=%d) differ:\nlegacy=%s\nnew   =%s", viewerID, postID, page, l, n)
					break
				}
				compared["comments"]++
				if allowed {
					compared["comments-allowed"]++
				}
				if len(comments) < 2 {
					break
				}
				compared["comments-with-cursor"]++
				after, afterID = &comments[0].CreatedAt, comments[0].ID
			}

			var before *time.Time
			beforeID := ""
			for page := range 2 {
				legacyAllowed, legacyReactions, err := legacyListRealMojis(ctx, db, viewerID, postID, before, beforeID)
				if err != nil {
					t.Fatal(err)
				}
				allowed, reactions, err := listRealMojisIfViewable(ctx, db, viewerID, postID, before, beforeID)
				if err != nil {
					t.Fatal(err)
				}
				if legacyAllowed != allowed {
					t.Errorf("realmojis(viewer=%s, post=%s, page=%d) allowed: legacy=%v new=%v", viewerID, postID, page, legacyAllowed, allowed)
					break
				}
				if l, n := normalizedJSON(t, realMojiResponses(legacyReactions)), normalizedJSON(t, realMojiResponses(reactions)); l != n {
					t.Errorf("realmojis(viewer=%s, post=%s, page=%d) differ:\nlegacy=%s\nnew   =%s", viewerID, postID, page, l, n)
					break
				}
				compared["realmojis"]++
				if allowed {
					compared["realmojis-allowed"]++
				}
				if len(reactions) < 2 {
					break
				}
				compared["realmojis-with-cursor"]++
				before, beforeID = &reactions[0].CreatedAt, reactions[0].ID
			}
		}
	}
	t.Logf("compared: %v", compared)
}
