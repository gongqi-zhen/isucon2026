package isureal

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func putProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Content-Type") != "image/jpeg" {
			return NewAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
		}
		data, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			c.Set("abuse", "invalid_image")
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}
		if img.Bounds().Dx() > 512 || img.Bounds().Dy() > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 本人の行のロック、画像の追加、profile_image_id の更新、前の画像の削除を、DB の関数1回の呼び出しで行う（#67）。
		// アップロードした画像の有効期限は、確定より前に取った時刻から数える。
		imageID := uuid.NewString()
		validatedAt := time.Now()
		var result string
		if err := h.DB.GetContext(ctx, &result, `SELECT isureal_put_profile_image($1, $2, $3)`, userID, imageID, data); err != nil {
			return err
		}
		if err := accountOutcome(result); err != nil {
			return err
		}
		cacheUploadedImage(imageID, data, validatedAt)
		return c.NoContent(http.StatusNoContent)
	}
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		// 本人の行のロック、profile_image_id を NULL にする更新、画像の削除を、DB の関数1回の呼び出しで行う（#67）。
		var result string
		if err := h.DB.GetContext(c.Request().Context(), &result, `SELECT isureal_delete_profile_image($1)`, GetSession(c).UserID); err != nil {
			return err
		}
		if err := accountOutcome(result); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

// accountOutcome は、プロフィール画像と退会の関数（migrate.sql）が返す結果を API のエラーに変換する。
// not_found は、変更前に行が見つからなかった場合（プロフィール画像の削除では、画像が設定されていない場合も）と同じく 404 にする。
func accountOutcome(result string) error {
	switch result {
	case "ok":
		return nil
	case "not_found":
		return NewAPIError(http.StatusNotFound, "account_not_found")
	default:
		return fmt.Errorf("unexpected account result: %q", result)
	}
}
