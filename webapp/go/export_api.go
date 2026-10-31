package isureal

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type exportPostResponse struct {
	createPostResponse
	ImagePath string `json:"imagePath"`
}

func exportPostsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		author, err := getUser(ctx, h.DB, userID)
		if err != nil {
			return err
		}
		// 画像本体（1枚あたり数百KB）は投稿の情報と分けて取得する。
		// 画像はキャッシュにあるものを使い、ないものだけをまとめて DB から読む。
		var posts []Post
		if err := h.DB.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `, userID); err != nil {
			return err
		}
		images, err := loadImageData(ctx, h.DB, posts)
		if err != nil {
			return err
		}

		var body bytes.Buffer
		writer := zip.NewWriter(&body)
		defer writer.Close()
		items := make([]exportPostResponse, 0, len(posts))
		for _, post := range posts {
			data, ok := images[post.ImageID]
			if !ok {
				// 以前は image_objects との内部結合だったので、画像のない投稿は含めない。
				continue
			}
			imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
			image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := image.Write(data); err != nil {
				return err
			}
			items = append(items, exportPostResponse{createPostResponse: newPostResponse(post, author), ImagePath: imagePath})
		}
		manifest, err := writer.Create("posts.json")
		if err != nil {
			return err
		}
		if err := json.NewEncoder(manifest).Encode(items); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
		c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
		return c.Blob(http.StatusOK, "application/zip", body.Bytes())
	}
}

type imageObjectRow struct {
	ID        string `db:"id"`
	Data      []byte `db:"data"`
	MediaType string `db:"media_type"`
}

// loadImageData は投稿の画像本体を imageID ごとに返す。
// 画像の中身は id ごとに変わらないので imageObjectCache を使い、ないものだけを DB から読んでキャッシュに入れる。
//
// image_objects は created_at で月ごとにパーティションに分かれていて、id だけで引くと全パーティションの索引を調べる。
// 投稿の画像は created_at = posts.posted_at で保存されるので、まず (id, created_at) で引いて1つのパーティションだけを読む。
// 等式が成り立たない行（初期データなど）があっても結果が変わらないよう、見つからなかったものだけ id で引き直す。
func loadImageData(ctx context.Context, db sqlx.QueryerContext, posts []Post) (map[string][]byte, error) {
	images := make(map[string][]byte, len(posts))
	missingIDs := make([]string, 0)
	missingCreatedAts := make([]time.Time, 0)
	seen := make(map[string]bool, len(posts))
	for _, post := range posts {
		if seen[post.ImageID] {
			continue
		}
		seen[post.ImageID] = true
		if object, ok := imageObjectCache.Get(post.ImageID); ok {
			images[post.ImageID] = object.Data
			continue
		}
		missingIDs = append(missingIDs, post.ImageID)
		missingCreatedAts = append(missingCreatedAts, post.PostedAt)
	}
	if len(missingIDs) == 0 {
		return images, nil
	}
	// 普通の JOIN にすると、キーが数百件を超えたあたりで全パーティションを読む Hash Join が選ばれる。
	// LATERAL の副問い合わせに LIMIT 1 を付けて、キーごとに主キーの索引を1回引く形に固定する
	// （(id, created_at) は主キーなので LIMIT 1 で結果は変わらない）。実行時のパーティション除外で、引くのは1つのパーティションだけになる。
	var rows []imageObjectRow
	if err := sqlx.SelectContext(ctx, db, &rows, `
  SELECT i.id, i.data, i.media_type
  FROM unnest($1::text[], $2::timestamptz[]) AS k(id, created_at)
  CROSS JOIN LATERAL (
    SELECT o.id, o.data, o.media_type FROM image_objects o
    WHERE o.id = k.id AND o.created_at = k.created_at
    LIMIT 1
  ) i
 `, missingIDs, missingCreatedAts); err != nil {
		return nil, err
	}
	addRows := func(rows []imageObjectRow) {
		for _, row := range rows {
			images[row.ID] = row.Data
			imageObjectCache.Add(row.ID, cachedImageObject{Data: row.Data, MediaType: row.MediaType}, int64(len(row.Data)), time.Time{})
		}
	}
	addRows(rows)

	fallback := make([]string, 0)
	for _, id := range missingIDs {
		if _, ok := images[id]; !ok {
			fallback = append(fallback, id)
		}
	}
	if len(fallback) == 0 {
		return images, nil
	}
	query, args, err := sqlx.In(`SELECT id, data, media_type FROM image_objects WHERE id IN (?)`, fallback)
	if err != nil {
		return nil, err
	}
	var fallbackRows []imageObjectRow
	if err := sqlx.SelectContext(ctx, db, &fallbackRows, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
	}
	addRows(fallbackRows)
	return images, nil
}
