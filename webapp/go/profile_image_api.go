package isureal

import (
	"context"
	"fmt"
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
		// 判定は変更前と同じ（全体をデコードしてから縦横を確かめる）。同じ画像の結果は覚えておき、デコードを省く（#102）。
		switch cachedProfileImageCheck(data) {
		case jpegValid:
		case jpegDecodeFailed:
			c.Set("abuse", "invalid_image")
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 本人の行のロック、画像の追加、profile_image_id の更新、前の画像の削除を、DB の関数1回の呼び出しで行う（#67）。
		// アップロードした画像の有効期限は、確定より前に取った時刻から数える。
		imageID := uuid.NewString()
		validatedAt := time.Now()
		var result profileImageResult
		if err := h.DB.GetContext(ctx, &result, `SELECT out_result, out_region FROM isureal_put_profile_image($1, $2, $3)`, userID, imageID, data); err != nil {
			return err
		}
		if err := accountOutcome(result.Result); err != nil {
			return err
		}
		cacheUploadedImage(imageID, data, validatedAt)
		result.waitSessionCache(ctx, userID)
		return c.NoContent(http.StatusNoContent)
	}
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		// 本人の行のロック、profile_image_id を NULL にする更新、画像の削除を、DB の関数1回の呼び出しで行う（#67）。
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		var result profileImageResult
		if err := h.DB.GetContext(ctx, &result, `SELECT out_result, out_region FROM isureal_delete_profile_image($1)`, userID); err != nil {
			return err
		}
		if err := accountOutcome(result.Result); err != nil {
			return err
		}
		result.waitSessionCache(ctx, userID)
		return c.NoContent(http.StatusNoContent)
	}
}

// profileImageResult は、プロフィール画像の関数（migrate.sql）が返す行。
// Region は、関数が本人の行をロックした後に読んだリージョン（not_found のときは NULL）。
type profileImageResult struct {
	Result string  `db:"out_result"`
	Region *string `db:"out_region"`
}

// waitSessionCache は、profile_image_id の変更が、利用者のリージョンのセッションキャッシュ（#58）に反映されるまで待つ。
// プロフィール画像の API は asia1 だけが処理するので、europe と america の利用者では TTL だけ待つ。
// ロックの後に読んだリージョンを使うので、同時にリージョンが変わっても、変更をコミットした時点のリージョンで待てる。
func (r profileImageResult) waitSessionCache(ctx context.Context, userID string) {
	if r.Region != nil {
		waitSessionInvalidation(ctx, *r.Region, userID)
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
