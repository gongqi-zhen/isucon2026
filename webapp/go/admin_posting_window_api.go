package isureal

import (
	"fmt"
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
		// advisory lock、重複の確認、DB の時計での開始時刻、前の投稿期間の終了、投稿期間と通知の挿入、pg_notify（#60）を、
		// DB の関数1回の呼び出しで行う（#72）。
		var row struct {
			Result         string     `db:"out_result"`
			ID             *string    `db:"out_id"`
			Region         *string    `db:"out_region"`
			LocalDate      *time.Time `db:"out_local_date"`
			StartsAt       *time.Time `db:"out_starts_at"`
			OnTimeDeadline *time.Time `db:"out_on_time_deadline"`
		}
		if err := h.DB.GetContext(ctx, &row, `
   SELECT out_result, out_id, out_region, out_local_date, out_starts_at, out_on_time_deadline
   FROM isureal_open_posting_window($1, $2::date, $3, $4, $5)
  `, region, localDate.Format(time.DateOnly), seconds, uuid.NewString(), postingWindowNotifyChannel); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "conflict":
			return NewAPIError(http.StatusConflict, "posting_window_conflict")
		default:
			return fmt.Errorf("unexpected open posting window result: %q", row.Result)
		}
		return c.JSON(http.StatusCreated, openPostingWindowResponse{
			ID: *row.ID, Region: *row.Region, LocalDate: row.LocalDate.Format(time.DateOnly),
			StartsAt: *row.StartsAt, OnTimeDeadline: *row.OnTimeDeadline,
		})
	}
}
