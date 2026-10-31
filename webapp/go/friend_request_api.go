package isureal

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createFriendRequestRequest struct {
	RecipientID string `json:"recipientId"`
}

type createFriendRequestResponse struct {
	RelationID string    `json:"relationId"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		if err := relationshipTarget(actor, input.RecipientID); err != nil {
			return err
		}
		// ロック、ブロック・友達関係の確認、相手からの申請の承認か新しい申請の作成を、
		// isureal_create_friend_request（migrate.sql）の1回の呼び出しで行う（#33 案A）。
		result, err := callFriendRequestFunc(ctx, h.DB, `SELECT * FROM isureal_create_friend_request($1, $2, $3, $4, $5, $6)`,
			actor, input.RecipientID, uuid.NewString(), uuid.NewString(), min(actor, input.RecipientID), max(actor, input.RecipientID))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: *result.ID, Status: *result.Status, UpdatedAt: *result.UpdatedAt})
	}
}

type transitionFriendRequestRequest struct {
	RequestID string `param:"requestId" json:"-"`
	Action    string `param:"action" json:"-"`
}

type transitionFriendRequestResponse createFriendRequestResponse

func transitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input transitionFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var status string
		switch input.Action {
		case "accept":
			status = "accepted"
		case "reject":
			status = "rejected"
		case "cancel":
			status = "cancelled"
		default:
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		// 申請の取得と権限の確認、ロック、状態とブロックの確認、更新を、
		// isureal_transition_friend_request（migrate.sql）の1回の呼び出しで行う（#33 案A）。
		result, err := callFriendRequestFunc(ctx, h.DB, `SELECT * FROM isureal_transition_friend_request($1, $2, $3, $4)`,
			actor, input.RequestID, status, uuid.NewString())
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: *result.ID, Status: *result.Status, UpdatedAt: *result.UpdatedAt})
	}
}
