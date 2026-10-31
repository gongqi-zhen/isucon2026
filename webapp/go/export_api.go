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

// loadImageData は投稿の画像本体を imageID ごとに返す。
// 画像の中身は id ごとに変わらないので imageObjectCache を使い、ないものだけを1回のクエリで読んでキャッシュに入れる。
func loadImageData(ctx context.Context, db sqlx.QueryerContext, posts []Post) (map[string][]byte, error) {
	images := make(map[string][]byte, len(posts))
	missing := make([]string, 0)
	for _, post := range posts {
		if _, ok := images[post.ImageID]; ok {
			continue
		}
		if object, ok := imageObjectCache.Get(post.ImageID); ok {
			images[post.ImageID] = object.Data
			continue
		}
		missing = append(missing, post.ImageID)
	}
	if len(missing) == 0 {
		return images, nil
	}
	query, args, err := sqlx.In(`SELECT id, data, media_type FROM image_objects WHERE id IN (?)`, missing)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID        string `db:"id"`
		Data      []byte `db:"data"`
		MediaType string `db:"media_type"`
	}
	if err := sqlx.SelectContext(ctx, db, &rows, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		images[row.ID] = row.Data
		imageObjectCache.Add(row.ID, cachedImageObject{Data: row.Data, MediaType: row.MediaType}, int64(len(row.Data)), time.Time{})
	}
	return images, nil
}
