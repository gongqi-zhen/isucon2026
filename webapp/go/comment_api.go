package isureal

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createCommentRequest struct {
	PostID string `param:"postId" json:"-"`
	Body   string `json:"body"`
}

type createCommentResponse struct {
	ID        string                   `json:"id"`
	Author    relationshipUserResponse `json:"author"`
	Body      string                   `json:"body"`
	CreatedAt time.Time                `json:"createdAt"`
}

func newCommentResponse(comment Comment, user User) createCommentResponse {
	return createCommentResponse{ID: comment.ID, Author: newRelationshipUserResponse(user), Body: comment.Body, CreatedAt: comment.CreatedAt}
}

func createCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Body, 1, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_interaction")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		// 投稿の確認、2人のロック、コメントの権限の確認、追加を、DB の関数1回の呼び出しで行う（#50）。
		var row struct {
			Result    string     `db:"out_result"`
			ID        *string    `db:"out_id"`
			PostID    *string    `db:"out_post_id"`
			AuthorID  *string    `db:"out_author_id"`
			Body      *string    `db:"out_body"`
			CreatedAt *time.Time `db:"out_created_at"`
			funcUser
		}
		if err := h.DB.GetContext(ctx, &row, `
   SELECT out_result, out_id, out_post_id, out_author_id, out_body, out_created_at, `+funcUserColumns+`
   FROM isureal_create_comment($1, $2, $3, $4)
  `, actor, input.PostID, uuid.NewString(), input.Body); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "forbidden":
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		case "user_not_found":
			return sql.ErrNoRows
		default:
			return fmt.Errorf("unexpected comment result: %q", row.Result)
		}
		user, err := activeUser(row.user())
		if err != nil {
			return err
		}
		comment := Comment{ID: *row.ID, PostID: *row.PostID, AuthorID: *row.AuthorID, Body: *row.Body, CreatedAt: *row.CreatedAt}
		return c.JSON(http.StatusCreated, newCommentResponse(comment, user))
	}
}

type getCommentsRequest struct {
	PostID         string     `param:"postId"`
	AfterCreatedAt *time.Time `query:"afterCreatedAt"`
	AfterID        string     `query:"afterId"`
}

type getCommentsResponse struct {
	Items   []createCommentResponse `json:"items"`
	HasMore bool                    `json:"hasMore"`
}

func getCommentsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getCommentsRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.AfterCreatedAt == nil) != (input.AfterID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		allowed, err := canComment(ctx, h.DB, GetSession(c).UserID, input.PostID)
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		comments, err := selectCommentsWithAuthor(ctx, h.DB, `AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51`, input.PostID, input.AfterCreatedAt, input.AfterID)
		if err != nil {
			return err
		}
		items := make([]createCommentResponse, 0, len(comments))
		for _, comment := range comments {
			items = append(items, newCommentResponse(comment.Comment, comment.Author))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getCommentsResponse{Items: items, HasMore: hasMore})
	}
}

type deleteCommentRequest struct {
	CommentID string `param:"commentId" json:"-"`
}

func deleteCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedID string
		if err := tx.GetContext(ctx, &lockedID, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor); err != nil {
			return err
		}
		var record struct {
			AuthorID     string `db:"author_id"`
			PostAuthorID string `db:"post_author_id"`
		}
		err = tx.GetContext(ctx, &record, `SELECT c.author_id,p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id=c.post_id WHERE c.id=$1 AND c.deleted_at IS NULL FOR UPDATE OF c`, input.CommentID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		}
		if err != nil {
			return err
		}
		if actor != record.AuthorID && actor != record.PostAuthorID {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE comments SET deleted_at=clock_timestamp() WHERE id=$1`, input.CommentID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
