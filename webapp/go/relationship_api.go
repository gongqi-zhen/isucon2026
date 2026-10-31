package isureal

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// 以下の操作は、ロックの取得から確定までを migrate.sql の isureal_* 関数の1回の呼び出しで行う（#33 案A）。
// users 行のロックを持つのは DB の中で関数が動いている間だけになる。

type deleteFriendshipRequest struct {
	UserID string `param:"userId" json:"-"`
}

func deleteFriendshipHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteFriendshipRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		actor := GetSession(c).UserID
		if err := relationshipTarget(actor, input.UserID); err != nil {
			return err
		}
		if err := callRelationshipFunc(c.Request().Context(), h.DB, `SELECT isureal_delete_friendship($1, $2, $3, $4)`,
			actor, input.UserID, min(actor, input.UserID), max(actor, input.UserID)); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type followRequest struct {
	UserID string `param:"userId" json:"-"`
}

func followHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input followRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		actor := GetSession(c).UserID
		if err := relationshipTarget(actor, input.UserID); err != nil {
			return err
		}
		if err := callRelationshipFunc(c.Request().Context(), h.DB, `SELECT isureal_follow($1, $2, $3)`,
			actor, input.UserID, uuid.NewString()); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type unfollowRequest struct {
	UserID string `param:"userId" json:"-"`
}

func unfollowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unfollowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		actor := GetSession(c).UserID
		if err := relationshipTarget(actor, input.UserID); err != nil {
			return err
		}
		if err := callRelationshipFunc(c.Request().Context(), h.DB, `SELECT isureal_unfollow($1, $2)`, actor, input.UserID); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type blockRequest struct {
	UserID string `param:"userId" json:"-"`
}

func blockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input blockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		actor := GetSession(c).UserID
		if err := relationshipTarget(actor, input.UserID); err != nil {
			return err
		}
		if err := callRelationshipFunc(c.Request().Context(), h.DB, `SELECT isureal_block($1, $2, $3, $4, $5)`,
			actor, input.UserID, uuid.NewString(), min(actor, input.UserID), max(actor, input.UserID)); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type unblockRequest struct {
	UserID string `param:"userId" json:"-"`
}

func unblockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unblockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		actor := GetSession(c).UserID
		if err := relationshipTarget(actor, input.UserID); err != nil {
			return err
		}
		if err := callRelationshipFunc(c.Request().Context(), h.DB, `SELECT isureal_unblock($1, $2)`, actor, input.UserID); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
