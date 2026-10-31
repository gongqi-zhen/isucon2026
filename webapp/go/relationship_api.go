package isureal

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type deleteFriendshipRequest struct {
	UserID string `param:"userId" json:"-"`
}

func deleteFriendshipHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteFriendshipRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, min(actor, input.UserID), max(actor, input.UserID))
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
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
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 相手の種別、ブロック、既存のフォローの確認と INSERT を1つの文で行う（#33）。
		// フォロー先は人気の認定公開アカウントで、その行のロックを持つ時間は DB への往復回数に比例するため。
		// 相手は beginRelationshipTx で active のままロックしてあるので、行は必ず返る。
		var followed bool
		if err := tx.GetContext(ctx, &followed, `
   WITH chk AS (
     SELECT u.account_type = 'certified_public'
            AND NOT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))
            AND NOT EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND followee_id=$2) AS allowed
     FROM users u WHERE u.id = $2 AND u.status = 'active'
   ), inserted AS (
     INSERT INTO follows (id, follower_id, followee_id) SELECT $3, $1, $2 FROM chk WHERE chk.allowed RETURNING id
   )
   SELECT EXISTS(SELECT 1 FROM inserted)
  `, actor, input.UserID, uuid.NewString()); err != nil {
			return err
		}
		if !followed {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if err := tx.Commit(); err != nil {
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
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE follower_id=$1 AND followee_id=$2`, actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
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
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 既存のブロックの確認、INSERT、友達関係・双方向のフォロー・pending の申請の後始末を1つの文で行う（#33）。
		// 後始末は、ブロックを新しく作ったときだけ行う。
		var blocked bool
		if err := tx.GetContext(ctx, &blocked, `
   WITH inserted AS (
     INSERT INTO block_relations (id, blocker_id, blocked_id)
     SELECT $3::text, $1::text, $2::text WHERE NOT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id=$1::text AND blocked_id=$2::text)
     RETURNING id
   ), unfriended AS (
     DELETE FROM friendships WHERE user_low_id=$4::text AND user_high_id=$5::text AND EXISTS(SELECT 1 FROM inserted)
   ), unfollowed AS (
     DELETE FROM follows WHERE ((follower_id=$1::text AND followee_id=$2::text) OR (follower_id=$2::text AND followee_id=$1::text)) AND EXISTS(SELECT 1 FROM inserted)
   ), cancelled AS (
     UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp()
     WHERE status='pending' AND ((requester_id=$1::text AND recipient_id=$2::text) OR (requester_id=$2::text AND recipient_id=$1::text)) AND EXISTS(SELECT 1 FROM inserted)
   )
   SELECT EXISTS(SELECT 1 FROM inserted)
  `, actor, input.UserID, uuid.NewString(), min(actor, input.UserID), max(actor, input.UserID)); err != nil {
			return err
		}
		if !blocked {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if err := tx.Commit(); err != nil {
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
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2`, actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusForbidden, "relationship_action_forbidden"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
