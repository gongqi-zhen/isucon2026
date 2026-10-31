package isureal

import (
	"database/sql"
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
		// コメントの権限の確認と一覧を、1回のクエリで読む（#65）。
		allowed, comments, err := listCommentsIfCommentable(c.Request().Context(), h.DB, GetSession(c).UserID, input.PostID, input.AfterCreatedAt, input.AfterID)
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
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
		// 本人の行のロック、コメントの行のロック、権限の確認、削除を、DB の関数1回の呼び出しで行う（#66）。
		var result string
		if err := h.DB.GetContext(c.Request().Context(), &result, `SELECT isureal_delete_comment($1, $2)`, GetSession(c).UserID, input.CommentID); err != nil {
			return err
		}
		if err := interactionDeletionOutcome(result); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
