package isureal

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
)

type RealMoji struct {
	ID           string     `db:"id"`
	PostID       string     `db:"post_id"`
	ReactorID    string     `db:"reactor_id"`
	ImageID      string     `db:"image_id"`
	ReactionType string     `db:"reaction_type"`
	CreatedAt    time.Time  `db:"created_at"`
	ReplacedAt   *time.Time `db:"replaced_at"`
	DeletedAt    *time.Time `db:"deleted_at"`
}

func getRealMoji(ctx context.Context, db sqlx.QueryerContext, id string) (RealMoji, error) {
	var reaction RealMoji
	err := sqlx.GetContext(ctx, db, &reaction, `SELECT id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at FROM realmojis WHERE id=$1`, id)
	return reaction, err
}

type RealMojiWithReactor struct {
	RealMoji
	Reactor User `db:"reactor"`
}

// realMojiListRow は listRealMojisIfViewable の1行。権限がないか、RealMoji が1件もないときは、RealMoji の列が NULL の1行になる。
type realMojiListRow struct {
	Allowed      bool         `db:"allowed"`
	ID           *string      `db:"id"`
	PostID       *string      `db:"post_id"`
	ReactorID    *string      `db:"reactor_id"`
	ImageID      *string      `db:"image_id"`
	ReactionType *string      `db:"reaction_type"`
	CreatedAt    *time.Time   `db:"created_at"`
	ReplacedAt   *time.Time   `db:"replaced_at"`
	DeletedAt    *time.Time   `db:"deleted_at"`
	Reactor      nullableUser `db:"reactor"`
}

// listRealMojisIfViewable は、閲覧権限の確認と、現在の RealMoji の一覧を1回のクエリで読む（#65）。
// 権限は canViewPost と同じ判定（isureal_can_view）。権限がなければ false を返す。
// 一覧は selectCurrentRealMojis と同じ条件と並び順で、before より前のものを最大51件返す。
func listRealMojisIfViewable(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string, beforeCreatedAt *time.Time, beforeID string) (bool, []RealMojiWithReactor, error) {
	var rows []realMojiListRow
	if err := sqlx.SelectContext(ctx, db, &rows, `
  WITH access AS MATERIALIZED (SELECT isureal_can_view($1, $2) AS allowed)
  SELECT access.allowed, r.*
  FROM access
  LEFT JOIN LATERAL (
    SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at,
           `+userColumnsAs("u", "reactor")+`
    FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
    WHERE access.allowed AND r.replaced_at IS NULL AND r.deleted_at IS NULL AND r.post_id = $2
      AND ($3::timestamptz IS NULL OR (r.created_at, r.id) < ($3, $4))
    ORDER BY r.created_at DESC, r.id DESC LIMIT 51
  ) r ON true
  ORDER BY r.created_at DESC, r.id DESC
 `, viewerID, postID, beforeCreatedAt, beforeID); err != nil {
		return false, nil, err
	}
	if len(rows) == 0 || !rows[0].Allowed {
		return false, nil, nil
	}
	reactions := make([]RealMojiWithReactor, 0, len(rows))
	for _, row := range rows {
		if row.ID == nil {
			continue
		}
		reactor, err := row.Reactor.user()
		if err != nil {
			return false, nil, err
		}
		reactions = append(reactions, RealMojiWithReactor{
			RealMoji: RealMoji{
				ID: *row.ID, PostID: *row.PostID, ReactorID: *row.ReactorID, ImageID: *row.ImageID, ReactionType: *row.ReactionType,
				CreatedAt: *row.CreatedAt, ReplacedAt: row.ReplacedAt, DeletedAt: row.DeletedAt,
			},
			Reactor: reactor,
		})
	}
	return true, reactions, nil
}

func selectCurrentRealMojis(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]RealMojiWithReactor, error) {
	var reactions []RealMojiWithReactor
	if err := sqlx.SelectContext(ctx, db, &reactions, `
  SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at,
         `+userColumnsAs("u", "reactor")+`
  FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL `+conditions, args...); err != nil {
		return nil, err
	}
	for i := range reactions {
		if err := reactions[i].Reactor.setImageKey(); err != nil {
			return nil, err
		}
	}
	return reactions, nil
}
