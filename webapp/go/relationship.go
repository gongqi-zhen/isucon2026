package isureal

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

// relationshipTarget は、友達関係の操作の相手として不正な値（自分自身や空）を 400 にする。
func relationshipTarget(actor, target string) error {
	if actor == target || target == "" {
		return NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	return nil
}

// relationshipOutcome は、migrate.sql の isureal_* 関数が返す結果を API のエラーに変換する。
func relationshipOutcome(result string) error {
	switch result {
	case "ok":
		return nil
	case "not_found":
		return NewAPIError(http.StatusNotFound, "relationship_not_found")
	case "conflict":
		return NewAPIError(http.StatusConflict, "relationship_conflict")
	case "forbidden":
		return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
	default:
		return fmt.Errorf("unexpected relationship result: %q", result)
	}
}

// callRelationshipFunc は、ロックの取得から確定までを DB の中で行う関数を autocommit の1文で呼ぶ（#33 案A）。
// users 行のロックを持つのは関数の実行中だけになる。
func callRelationshipFunc(ctx context.Context, db sqlx.QueryerContext, query string, args ...any) error {
	var result string
	if err := sqlx.GetContext(ctx, db, &result, query, args...); err != nil {
		return err
	}
	return relationshipOutcome(result)
}

// friendRequestResult は、友達申請の関数が返す行。
type friendRequestResult struct {
	Result    string     `db:"out_result"`
	ID        *string    `db:"out_id"`
	Status    *string    `db:"out_status"`
	UpdatedAt *time.Time `db:"out_updated_at"`
}

func callFriendRequestFunc(ctx context.Context, db sqlx.QueryerContext, query string, args ...any) (friendRequestResult, error) {
	var result friendRequestResult
	if err := sqlx.GetContext(ctx, db, &result, query, args...); err != nil {
		return result, err
	}
	return result, relationshipOutcome(result.Result)
}

func isBlocked(ctx context.Context, db sqlx.QueryerContext, actor, target string) (bool, error) {
	var blocked bool
	err := sqlx.GetContext(ctx, db, &blocked, `SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`, actor, target)
	return blocked, err
}
