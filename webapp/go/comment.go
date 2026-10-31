package isureal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

type Comment struct {
	ID        string     `db:"id"`
	PostID    string     `db:"post_id"`
	AuthorID  string     `db:"author_id"`
	Body      string     `db:"body"`
	CreatedAt time.Time  `db:"created_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

func getComment(ctx context.Context, db sqlx.QueryerContext, id string) (Comment, error) {
	var comment Comment
	err := sqlx.GetContext(ctx, db, &comment, `SELECT id,post_id,author_id,body,created_at,deleted_at FROM comments WHERE id=$1`, id)
	return comment, err
}

type CommentWithAuthor struct {
	Comment
	Author User `db:"author"`
}

// クエリの文字列は起動時に1回だけ組み立てる（#112）。
var selectCommentsWithAuthorQuery = `
  SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ` + userColumnsAs("u", "author") + `
  FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.deleted_at IS NULL `

func selectCommentsWithAuthor(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]CommentWithAuthor, error) {
	var comments []CommentWithAuthor
	if err := sqlx.SelectContext(ctx, db, &comments, selectCommentsWithAuthorQuery+conditions, args...); err != nil {
		return nil, err
	}
	for i := range comments {
		if err := comments[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return comments, nil
}

// interactionDeletionOutcome は、isureal_delete_comment と isureal_delete_realmoji（interaction_functions.sql）の結果を API のエラーに変換する（#66）。
func interactionDeletionOutcome(result string) error {
	switch result {
	case "ok":
		return nil
	case "not_found":
		return NewAPIError(http.StatusNotFound, "interaction_not_found")
	case "forbidden":
		return NewAPIError(http.StatusForbidden, "interaction_forbidden")
	case "user_not_found":
		// 変更前は、本人の行のロックが sql.ErrNoRows になり 500 を返していた。
		return sql.ErrNoRows
	default:
		return fmt.Errorf("unexpected interaction deletion result: %q", result)
	}
}

// commentListRow は listCommentsIfCommentable の1行。権限がないか、コメントが1件もないときは、コメントの列が NULL の1行になる。
type commentListRow struct {
	Allowed   bool         `db:"allowed"`
	ID        *string      `db:"id"`
	PostID    *string      `db:"post_id"`
	AuthorID  *string      `db:"author_id"`
	Body      *string      `db:"body"`
	CreatedAt *time.Time   `db:"created_at"`
	DeletedAt *time.Time   `db:"deleted_at"`
	Author    nullableUser `db:"author"`
}

// listCommentsIfCommentable は、コメントの権限の確認と一覧を1回のクエリで読む（#65）。
// 権限は canComment と同じ判定（isureal_can_comment）。権限がなければ false を返す。
// 一覧は selectCommentsWithAuthor と同じ条件と並び順で、after より後のものを最大51件返す。
var listCommentsIfCommentableQuery = `
  WITH access AS MATERIALIZED (SELECT isureal_can_comment($1, $2) AS allowed)
  SELECT access.allowed, c.*
  FROM access
  LEFT JOIN LATERAL (
    SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ` + userColumnsAs("u", "author") + `
    FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
    WHERE access.allowed AND c.deleted_at IS NULL AND c.post_id = $2
      AND ($3::timestamptz IS NULL OR (c.created_at, c.id) > ($3, $4))
    ORDER BY c.created_at, c.id LIMIT 51
  ) c ON true
  ORDER BY c.created_at, c.id
 `

func listCommentsIfCommentable(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string, afterCreatedAt *time.Time, afterID string) (bool, []CommentWithAuthor, error) {
	var rows []commentListRow
	if err := sqlx.SelectContext(ctx, db, &rows, listCommentsIfCommentableQuery, viewerID, postID, afterCreatedAt, afterID); err != nil {
		return false, nil, err
	}
	if len(rows) == 0 || !rows[0].Allowed {
		return false, nil, nil
	}
	comments := make([]CommentWithAuthor, 0, len(rows))
	for _, row := range rows {
		if row.ID == nil {
			continue
		}
		author, err := row.Author.user()
		if err != nil {
			return false, nil, err
		}
		comments = append(comments, CommentWithAuthor{
			Comment: Comment{ID: *row.ID, PostID: *row.PostID, AuthorID: *row.AuthorID, Body: *row.Body, CreatedAt: *row.CreatedAt, DeletedAt: row.DeletedAt},
			Author:  author,
		})
	}
	return true, comments, nil
}

func canComment(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	access, err := loadPostAccess(ctx, db, viewerID, postID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !access.ViewerActive {
		return false, nil
	}
	post, viewer := access.PostWithAuthor, access.viewer(viewerID)
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
