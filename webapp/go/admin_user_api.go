package isureal

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
)

type certifyUserRequest struct {
	UserID string `param:"userId" json:"-"`
}

func certifyUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input certifyUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		var region string
		err := h.DB.GetContext(ctx, &region, `UPDATE users SET account_type='certified_public', updated_at=clock_timestamp() WHERE id=$1 AND status='active' AND account_type='regular' RETURNING region`, input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusConflict, "certification_conflict")
		}
		if err != nil {
			return err
		}
		// accountType の変更が、その利用者のリージョンのセッションキャッシュ（#58）に反映されるまで待つ。
		// admin のリクエストは、どのサーバにも届きうる。
		waitSessionInvalidation(ctx, region, input.UserID)
		return c.NoContent(http.StatusNoContent)
	}
}

// 2026-08-26: 認定されたユーザーを戻すことは禁止する
func deleteUserCertificationHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		return NewAPIError(http.StatusConflict, "certification_irreversible")
	}
}
