package isureal

import (
	"database/sql"
	"errors"
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
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// ロックを取ってから確定するまでの往復を減らすため、確認・承認・作成を1つの文で行う（#33）。
		// ロックの後に別の文として実行するので、確認はロックを取った後の状態を見る。
		//   - ブロック関係か友達関係があれば、何も変えずに conflict を返す
		//   - 相手からの pending の申請があれば、それを承認して友達関係を作る
		//   - 自分からの pending の申請があれば、何も変えない（409）
		//   - どちらもなければ、新しい申請を作る
		var result struct {
			Conflict  bool       `db:"conflict"`
			ID        *string    `db:"id"`
			Status    *string    `db:"status"`
			UpdatedAt *time.Time `db:"updated_at"`
		}
		if err := tx.GetContext(ctx, &result, `
   WITH c AS (
     SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))
         OR EXISTS(SELECT 1 FROM friendships WHERE (user_low_id=$1 AND user_high_id=$2) OR (user_low_id=$2 AND user_high_id=$1)) AS conflict
   ), p AS (
     SELECT id, requester_id, recipient_id FROM friend_requests WHERE status='pending' AND
      ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
     LIMIT 1
   ), accepted AS (
     UPDATE friend_requests f SET status='accepted', updated_at=clock_timestamp()
     FROM p, c WHERE f.id = p.id AND NOT c.conflict AND p.requester_id = $2
     RETURNING f.id, f.status, f.updated_at
   ), friendship AS (
     INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
     SELECT $4, $5, $6, accepted.id FROM accepted
   ), created AS (
     INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
     SELECT $3, $1, $2, 'pending', clock_timestamp(), clock_timestamp() FROM c
     WHERE NOT c.conflict AND NOT EXISTS (SELECT 1 FROM p)
     RETURNING id, status, updated_at
   ), changed AS (
     SELECT id, status, updated_at FROM accepted UNION ALL SELECT id, status, updated_at FROM created
   )
   SELECT c.conflict, changed.id, changed.status, changed.updated_at
   FROM c LEFT JOIN changed ON true
  `, actor, input.RecipientID, uuid.NewString(), uuid.NewString(), min(actor, input.RecipientID), max(actor, input.RecipientID)); err != nil {
			return err
		}
		if result.Conflict || result.ID == nil {
			// ブロック・友達関係があるか、自分からの pending の申請がすでにある
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		request := FriendRequest{ID: *result.ID, Status: *result.Status, UpdatedAt: *result.UpdatedAt}
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
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
		request, err := getFriendRequest(ctx, h.DB, input.RequestID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if (status == "cancelled" && actor != request.RequesterID) || (status != "cancelled" && actor != request.RecipientID) {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		tx, err := beginRelationshipTx(ctx, h.DB, request.RequesterID, request.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// ロックを取ってから確定するまでの往復を減らすため、状態の確認・ブロックの確認・更新を1つの文で行う（#33）。
		// 申請者と受信者は変わらないので、友達関係の user_low_id と user_high_id はロックの前に読んだ値から決める。
		var result struct {
			Status    string     `db:"current_status"`
			Blocked   bool       `db:"blocked"`
			ID        *string    `db:"id"`
			NewStatus *string    `db:"status"`
			UpdatedAt *time.Time `db:"updated_at"`
		}
		if err := tx.GetContext(ctx, &result, `
   WITH r AS (
     SELECT id, requester_id, recipient_id, status FROM friend_requests WHERE id = $1
   ), b AS (
     SELECT $3::boolean AND EXISTS(
       SELECT 1 FROM block_relations, r
       WHERE (blocker_id = r.requester_id AND blocked_id = r.recipient_id) OR (blocker_id = r.recipient_id AND blocked_id = r.requester_id)
     ) AS blocked
   ), ok AS (
     SELECT r.id FROM r, b WHERE r.status = 'pending' AND NOT b.blocked
   ), friendship AS (
     INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
     SELECT $4, $5, $6, ok.id FROM ok WHERE $3::boolean
   ), updated AS (
     UPDATE friend_requests f SET status = $2, updated_at = clock_timestamp()
     FROM ok WHERE f.id = ok.id
     RETURNING f.id, f.status, f.updated_at
   )
   SELECT r.status AS current_status, b.blocked, updated.id, updated.status, updated.updated_at
   FROM r CROSS JOIN b LEFT JOIN updated ON true
  `, input.RequestID, status, status == "accepted", uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID)); err != nil {
			return err
		}
		if result.Status != "pending" {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		if result.Blocked {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		request = FriendRequest{ID: *result.ID, Status: *result.NewStatus, UpdatedAt: *result.UpdatedAt}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}
