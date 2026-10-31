package isureal

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type replaceRealMojiRequest struct {
	PostID       string                `param:"postId" json:"-"`
	Image        *multipart.FileHeader `form:"image"`
	ReactionType string                `form:"reactionType"`
}

type replaceRealMojiResponse struct {
	ID           string                   `json:"id"`
	Reactor      relationshipUserResponse `json:"reactor"`
	ReactionType string                   `json:"reactionType"`
	ImageURL     string                   `json:"imageUrl"`
	CreatedAt    time.Time                `json:"createdAt"`
}

func newRealMojiResponse(reaction RealMoji, user User) replaceRealMojiResponse {
	return replaceRealMojiResponse{ID: reaction.ID, Reactor: newRelationshipUserResponse(user), ReactionType: reaction.ReactionType, ImageURL: signedImageURL(user, reaction.ImageID), CreatedAt: reaction.CreatedAt}
}

func replaceRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input replaceRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		switch input.ReactionType {
		case "like", "smile", "surprise", "heart", "laugh", "instant":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		file, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		if !validJPEGWithin(data, 512) {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}

		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		// 投稿の確認、2人のロック、閲覧権限の確認、置き換えを、DB の関数1回の呼び出しで行う（#50）。
		// アップロードした画像の有効期限は、確定より前に取った時刻から数える。
		imageID := uuid.NewString()
		validatedAt := time.Now()
		var row struct {
			Result       string     `db:"out_result"`
			ID           *string    `db:"out_id"`
			PostID       *string    `db:"out_post_id"`
			ReactorID    *string    `db:"out_reactor_id"`
			ImageID      *string    `db:"out_image_id"`
			ReactionType *string    `db:"out_reaction_type"`
			CreatedAt    *time.Time `db:"out_created_at"`
			funcUser
		}
		if err := h.DB.GetContext(ctx, &row, `
   SELECT out_result, out_id, out_post_id, out_reactor_id, out_image_id, out_reaction_type, out_created_at, `+funcUserColumns+`
   FROM isureal_create_realmoji($1, $2, $3, $4, $5, $6)
  `, actor, input.PostID, uuid.NewString(), imageID, data, input.ReactionType); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "forbidden":
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		case "not_found":
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		default:
			return fmt.Errorf("unexpected realmoji result: %q", row.Result)
		}
		user, err := activeUser(row.user())
		if err != nil {
			return err
		}
		reaction := RealMoji{
			ID: *row.ID, PostID: *row.PostID, ReactorID: *row.ReactorID, ImageID: *row.ImageID,
			ReactionType: *row.ReactionType, CreatedAt: *row.CreatedAt,
		}
		cacheUploadedImage(imageID, data, validatedAt)
		return c.JSON(http.StatusCreated, newRealMojiResponse(reaction, user))
	}
}

type getRealMojisRequest struct {
	PostID          string     `param:"postId"`
	BeforeCreatedAt *time.Time `query:"beforeCreatedAt"`
	BeforeID        string     `query:"beforeId"`
}

type getRealMojisResponse struct {
	Items   []replaceRealMojiResponse `json:"items"`
	HasMore bool                      `json:"hasMore"`
}

func getRealMojisHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getRealMojisRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.BeforeCreatedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		// 閲覧権限の確認と一覧を、1回のクエリで読む（#65）。
		allowed, reactions, err := listRealMojisIfViewable(c.Request().Context(), h.DB, GetSession(c).UserID, input.PostID, input.BeforeCreatedAt, input.BeforeID)
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		items := make([]replaceRealMojiResponse, 0, len(reactions))
		for _, reaction := range reactions {
			items = append(items, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getRealMojisResponse{Items: items, HasMore: hasMore})
	}
}

type deleteRealMojiRequest struct {
	RealMojiID string `param:"realMojiId" json:"-"`
}

func deleteRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		// 本人の行のロック、RealMoji の行のロック、権限の確認、削除を、DB の関数1回の呼び出しで行う（#66）。
		var result string
		if err := h.DB.GetContext(c.Request().Context(), &result, `SELECT isureal_delete_realmoji($1, $2)`, GetSession(c).UserID, input.RealMojiID); err != nil {
			return err
		}
		if err := interactionDeletionOutcome(result); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
