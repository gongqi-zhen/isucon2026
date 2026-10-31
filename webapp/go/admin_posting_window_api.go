package isureal

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type openPostingWindowRequest struct {
	Region        string `json:"region"`
	LocalDate     string `json:"localDate"`
	OnTimeSeconds *int64 `json:"onTimeSeconds"`
}

type openPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

func openPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input openPostingWindowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		region := input.Region
		localDate, err := time.Parse(time.DateOnly, input.LocalDate)
		if !validRegion(region) || err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_posting_window")
		}
		seconds := int64(24 * 60 * 60)
		if input.OnTimeSeconds != nil {
			seconds = *input.OnTimeSeconds
		}
		ctx := c.Request().Context()
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 同じリージョンの投稿期間の開始を直列にする。users のテーブルロックは登録やプロフィール更新まで止めてしまうので使わない。
		// 同じ文で pg_notify を送り、各プロセスの通知ハブにすぐ確認させる（#60）。届くのは COMMIT の後だけで、ロールバックすれば届かない。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('posting_window:' || $1)), pg_notify($2, $1)`, region, postingWindowNotifyChannel); err != nil {
			return err
		}
		var conflict bool
		if err := tx.GetContext(ctx, &conflict, `SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2)`, region, localDate); err != nil {
			return err
		}
		if conflict {
			return NewAPIError(http.StatusConflict, "posting_window_conflict")
		}
		var startsAt time.Time
		if err := tx.GetContext(ctx, &startsAt, `SELECT clock_timestamp()`); err != nil {
			return err
		}
		// 前の投稿期間を終える UPDATE は、その行を COMMIT までロックし、そのリージョンの投稿（FOR SHARE）を待たせる。
		// そこで、その行のロックが要らない通知の一括挿入を先に済ませ、UPDATE を COMMIT の直前に置く（#85）。
		// 通知には外部キーがなく（#41）、COMMIT までは見えないので、新しい投稿期間より先に挿入してよい。
		windowID := uuid.NewString()
		// ID は先頭48ビットに開始時刻（ミリ秒）を入れた UUID v7 にする。ランダムな UUID だと、主キーのインデックスの
		// ばらばらのページに1行ずつ書き込むことになり、利用者の数だけページを読み書きする（#41）。
		// 同じ文で挿入する行は先頭が同じになり、インデックスの近いページにまとまる。
		if _, err := tx.ExecContext(ctx, `
   INSERT INTO notifications (id, user_id, posting_window_id, created_at)
   SELECT encode(set_bit(set_bit(overlay(uuid_send(gen_random_uuid())
            PLACING substring(int8send(floor(extract(epoch FROM $3::timestamptz) * 1000)::bigint) FROM 3) FROM 1 FOR 6),
            52, 1), 53, 1), 'hex')::uuid::text,
          id, $2, $3
   FROM users WHERE region = $1 AND status = 'active'
  `, region, windowID, startsAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posting_windows SET ended_at = $2 WHERE region = $1 AND ended_at IS NULL`, region, startsAt); err != nil {
			return err
		}
		// 開いている投稿期間は1つだけ（posting_windows_one_active_region）なので、前の投稿期間を終えてから挿入する。
		var window PostingWindow
		if err := tx.GetContext(ctx, &window, `
   INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
   VALUES ($1, $2, $3, $4, $5)
   RETURNING id, region, local_date, starts_at, on_time_deadline, ended_at
  `, windowID, region, localDate, startsAt, startsAt.Add(time.Duration(seconds)*time.Second)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, openPostingWindowResponse{
			ID: window.ID, Region: window.Region, LocalDate: window.LocalDate.Format(time.DateOnly),
			StartsAt: window.StartsAt, OnTimeDeadline: window.OnTimeDeadline,
		})
	}
}
