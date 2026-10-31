package isureal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		var input getImageRequest
		if err := c.Bind(&input); err != nil {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		query, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		exp, err := strconv.ParseInt(input.Exp, 10, 64)
		if err != nil || strconv.FormatInt(exp, 10) != input.Exp || exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		sig, err := base64.RawURLEncoding.Strict().DecodeString(input.Sig)
		if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != input.Sig {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		ctx := c.Request().Context()
		verifiedKey := input.ImageID + "\x00" + input.Exp + "\x00" + input.Sig
		if _, verified := verifiedSignatureCache.Get(verifiedKey); !verified {
			secretKeyHash, cached := imageOwnerKeyCache.Get(input.ImageID)
			if !cached {
				found, err := loadImageOwnerKey(ctx, h, input.ImageID, &secretKeyHash)
				if err != nil {
					return err
				}
				if !found {
					return NewAPIError(http.StatusForbidden, "image_forbidden")
				}
			}
			key, err := imageSigningKey(secretKeyHash)
			if err != nil {
				return err
			}
			if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
				// キャッシュした鍵を使った場合は、元の実装と同じく所有者が退会済みなら abuse として扱わない。
				if cached {
					found, err := loadImageOwnerKey(ctx, h, input.ImageID, &secretKeyHash)
					if err != nil {
						return err
					}
					if !found {
						return NewAPIError(http.StatusForbidden, "image_forbidden")
					}
				}
				c.Set("abuse", "invalid_signature")
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
			verifiedSignatureCache.Add(verifiedKey, struct{}{}, 1, time.Unix(exp, 0))
		}

		// 削除状態は imageValidityTTL より古い結果を使わずに確認する。5秒以内の許容範囲に収まる。
		object, cached := imageObjectCache.Get(input.ImageID)
		if cached {
			if _, valid := imageValidityCache.Get(input.ImageID); !valid {
				var exists bool
				if err := h.DB.GetContext(ctx, &exists, `SELECT EXISTS(SELECT 1 `+validImageFrom+`)`, input.ImageID); err != nil {
					return err
				}
				if !exists {
					return NewAPIError(http.StatusForbidden, "image_forbidden")
				}
				imageValidityCache.Add(input.ImageID, struct{}{}, 1, time.Now().Add(imageValidityTTL))
			}
		} else {
			validatedAt := time.Now()
			var row struct {
				Data      []byte `db:"data"`
				MediaType string `db:"media_type"`
			}
			err := h.DB.GetContext(ctx, &row, `SELECT i.data, i.media_type `+validImageFrom, input.ImageID)
			if errors.Is(err, sql.ErrNoRows) {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
			if err != nil {
				return err
			}
			object = cachedImageObject{Data: row.Data, MediaType: row.MediaType}
			imageObjectCache.Add(input.ImageID, object, int64(len(row.Data)), time.Time{})
			imageValidityCache.Add(input.ImageID, struct{}{}, 1, validatedAt.Add(imageValidityTTL))
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}

// 画像が現在も参照されているか（プロフィール画像、削除されていない投稿、差し替え・削除されていない RealMoji）。
const validImageFrom = `
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
   WHERE i.id = $1 AND (
     u.profile_image_id = i.id
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   )
`

// 所有者が active な画像について、所有者の secret_key_hash を取得してキャッシュする。
func loadImageOwnerKey(ctx context.Context, h *HandlerContext, imageID string, secretKeyHash *string) (bool, error) {
	err := h.DB.GetContext(ctx, secretKeyHash, `
   SELECT u.secret_key_hash
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id
   WHERE i.id = $1 AND u.status = 'active'
  `, imageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	imageOwnerKeyCache.Add(imageID, *secretKeyHash, 1, time.Time{})
	return true, nil
}
