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
		startImageInvalidationPoller(h.DB)
		verifiedKey := verifiedSignatureKey(input.ImageID, input.Exp, input.Sig)
		if _, verified := verifiedSignatureCache.Get(verifiedKey); !verified {
			owner, cached := imageOwnerKeyCache.Get(input.ImageID)
			if !cached {
				found, err := loadImageOwnerKey(ctx, h, input.ImageID, &owner)
				if err != nil {
					return err
				}
				if !found {
					return NewAPIError(http.StatusForbidden, "image_forbidden")
				}
			}
			key, err := imageSigningKey(owner.SecretKeyHash)
			if err != nil {
				return err
			}
			if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
				// キャッシュした鍵を使った場合は、元の実装と同じく所有者が退会済みなら abuse として扱わない。
				if cached {
					found, err := loadImageOwnerKey(ctx, h, input.ImageID, &owner)
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

		// 削除状態は、無効化の一括検知（image_validity.go）か、imageValidityTTL より古くない確認の結果で判断する。
		// どちらも、削除の5秒後には有効とみなさない。確認の結果は、クエリを送る前の時刻から数える。
		object, cached := imageObjectCache.Get(input.ImageID)
		if cached {
			if !imageKnownValid(input.ImageID) {
				owner, _ := imageOwnerKeyCache.Get(input.ImageID)
				sentAt := time.Now()
				row, valid, err := checkImageValidity(ctx, h.DB, input.ImageID, owner.OwnerID)
				if err != nil {
					return err
				}
				if !valid {
					return NewAPIError(http.StatusForbidden, "image_forbidden")
				}
				markImageValid(input.ImageID, row.meta(), sentAt)
			}
		} else {
			sentAt := time.Now()
			row, err := loadValidImageObject(ctx, h.DB, input.ImageID)
			if errors.Is(err, sql.ErrNoRows) {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
			if err != nil {
				return err
			}
			object = cachedImageObject{Data: row.Data, MediaType: row.MediaType}
			imageObjectCache.Add(input.ImageID, object, int64(len(row.Data)), time.Time{})
			markImageValid(input.ImageID, row.meta(), sentAt)
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}

// 所有者が active な画像について、所有者の id と secret_key_hash を取得してキャッシュする。
func loadImageOwnerKey(ctx context.Context, h *HandlerContext, imageID string, owner *imageOwner) (bool, error) {
	err := h.DB.GetContext(ctx, owner, `
   SELECT u.id AS owner_id, u.secret_key_hash
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id
   WHERE i.id = $1 AND u.status = 'active'
  `, imageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	imageOwnerKeyCache.Add(imageID, *owner, 1, time.Time{})
	return true, nil
}
