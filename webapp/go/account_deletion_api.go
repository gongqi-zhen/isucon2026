package isureal

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func deleteAccountHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 本人の行のロック、退会の状態への更新、有効なセッションの無効化を、DB の関数1回の呼び出しで行う（#67）。
		// active な利用者がいなければ、変更前と同じく 404 account_not_found にする。
		var result string
		if err := h.DB.GetContext(ctx, &result, `SELECT isureal_delete_account($1)`, userID); err != nil {
			return err
		}
		if err := accountOutcome(result); err != nil {
			return err
		}
		waitSessionInvalidation(ctx, GetSession(c).Region, userID)
		return c.NoContent(http.StatusNoContent)
	}
}
