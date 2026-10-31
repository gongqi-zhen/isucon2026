package isureal

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createPostRequest struct {
	Image        *multipart.FileHeader `form:"image"`
	Caption      string                `form:"caption"`
	Visibility   string                `form:"visibility"`
	LocationKind string                `form:"location_kind"`
	Latitude     *float64              `form:"latitude"`
	Longitude    *float64              `form:"longitude"`
	Locality     *string               `form:"locality"`
}

type postAuthorResponse struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type createPostResponse struct {
	ID              string             `json:"id"`
	Author          postAuthorResponse `json:"author"`
	PostingWindowID string             `json:"postingWindowId"`
	ImageURL        string             `json:"imageUrl"`
	Caption         string             `json:"caption"`
	Visibility      string             `json:"visibility"`
	Location        PostLocation       `json:"location"`
	PostedAt        time.Time          `json:"postedAt"`
	OnTime          bool               `json:"onTime"`
	LateSeconds     int64              `json:"lateSeconds"`
}

func createPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil || !validUserText(input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		switch input.Visibility {
		case "friends", "friends_of_friends", "public":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		location := PostLocation{
			Kind: input.LocationKind, Latitude: input.Latitude,
			Longitude: input.Longitude, Locality: input.Locality,
		}
		if !location.valid() {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		imageFile, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer imageFile.Close()
		imageData, err := io.ReadAll(imageFile)
		if err != nil {
			return err
		}
		if !validJPEGWithin(imageData, 4096) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 本人の行のロック、投稿期間の FOR SHARE、その日の件数の確認、画像と投稿の INSERT を、DB の関数1回の呼び出しで行う（#50）。
		// アップロードした画像の有効期限は、確定より前に取った時刻から数える。
		imageID, postID := uuid.NewString(), uuid.NewString()
		validatedAt := time.Now()
		var row struct {
			Result         string     `db:"out_result"`
			WindowID       *string    `db:"out_posting_window_id"`
			PostedAt       *time.Time `db:"out_posted_at"`
			OnTimeDeadline *time.Time `db:"out_on_time_deadline"`
			Latitude       *float64   `db:"out_latitude"`
			Longitude      *float64   `db:"out_longitude"`
			funcUser
		}
		if err := h.DB.GetContext(ctx, &row, `
   SELECT out_result, out_posting_window_id, out_posted_at, out_on_time_deadline, out_latitude, out_longitude, `+funcUserColumns+`
   FROM isureal_create_post($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
  `, userID, postID, imageID, imageData, input.Caption, input.Visibility,
			location.Kind, location.Latitude, location.Longitude, location.Locality); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "slot_unavailable":
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		case "user_not_found":
			return sql.ErrNoRows
		default:
			return fmt.Errorf("unexpected create post result: %q", row.Result)
		}
		user, err := activeUser(row.user())
		if err != nil {
			return err
		}
		post := Post{
			ID: postID, AuthorID: userID, PostingWindowID: *row.WindowID, ImageID: imageID,
			Caption: input.Caption, Visibility: input.Visibility,
			PostLocation: PostLocation{Kind: location.Kind, Latitude: row.Latitude, Longitude: row.Longitude, Locality: location.Locality},
			PostedAt:     *row.PostedAt, OnTimeDeadline: *row.OnTimeDeadline,
		}
		cacheUploadedImage(imageID, imageData, validatedAt)
		return c.JSON(http.StatusCreated, newPostResponse(post, user))
	}
}

func newPostResponse(post Post, user User) createPostResponse {
	response := createPostResponse{
		ID: post.ID,
		Author: postAuthorResponse{
			ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
			ProfileImageURL: newGetMeResponse(user).ProfileImageURL,
			Bio:             user.Bio, AccountType: user.AccountType,
		},
		PostingWindowID: post.PostingWindowID, ImageURL: signedImageURL(user, post.ImageID),
		Caption: post.Caption, Visibility: post.Visibility, Location: post.PostLocation,
		PostedAt: post.PostedAt, OnTime: !post.PostedAt.After(post.OnTimeDeadline),
	}
	if !response.OnTime {
		response.LateSeconds = int64(post.PostedAt.Sub(post.OnTimeDeadline) / time.Second)
	}
	return response
}

type getMyCurrentPostResponse createPostResponse

func getMyCurrentPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 作者（自分）が active でなければ getUser と同じくエラーにするため、作者は status で絞り込まずに取得する。
		var post struct {
			Post
			Author User `db:"author"`
		}
		err := h.DB.GetContext(ctx, &post, `
   SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN users u ON u.id = p.author_id
   JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                              AND current.local_date = w.local_date
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   LIMIT 1
  `, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		user, err := activeUser(post.Author)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(post.Post, user)))
	}
}

type getPostRequest struct {
	PostID string `param:"postId"`
}

type getPostResponse createPostResponse

func getPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		access, err := loadPostAccess(ctx, h.DB, GetSession(c).UserID, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if !access.canView(GetSession(c).UserID) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		return c.JSON(http.StatusOK, getPostResponse(newPostResponse(access.Post, access.Author)))
	}
}

type updatePostRequest struct {
	PostID     string  `param:"postId" json:"-"`
	Caption    *string `json:"caption"`
	Visibility *string `json:"visibility"`
}

type updatePostResponse createPostResponse

func updatePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input updatePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Caption == nil && input.Visibility == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Caption != nil && !validUserText(*input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Visibility != nil {
			switch *input.Visibility {
			case "friends", "friends_of_friends", "public":
			default:
				return NewAPIError(http.StatusBadRequest, "invalid_post")
			}
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		lockedUser, err := lockUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		// getPost は投稿期間の ended_at も取得している。
		if post.WindowEndedAt != nil {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET caption = COALESCE($2, caption), visibility = COALESCE($3, visibility) WHERE id = $1`, post.ID, input.Caption, input.Visibility); err != nil {
			return err
		}
		post, err = getPost(ctx, tx, post.ID)
		if err != nil {
			return err
		}
		user, err := activeUser(lockedUser)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updatePostResponse(newPostResponse(post, user)))
	}
}

type deletePostRequest struct {
	PostID string `param:"postId" json:"-"`
}

func deletePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deletePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedUserID string
		if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		var deletions int
		if err := tx.GetContext(ctx, &deletions, `
   SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND p.deleted_at IS NOT NULL
     AND w.local_date = (SELECT local_date FROM posting_windows WHERE id = $2)
  `, userID, post.PostingWindowID); err != nil {
			return err
		}
		if deletions >= 2 {
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1`, post.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
