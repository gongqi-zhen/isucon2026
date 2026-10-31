package isureal

import (
	"archive/zip"
	"context"
	"database/sql"
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

// エクスポートで1回に画像本体を読む投稿の数（最初のまとまりの件数と、倍々に増やすときの上限）と、読む関数。テストで差し替える。
var (
	exportImageChunkFirst = 8
	exportImageChunkMax   = 32
	exportLoadImages      = loadImageData
)

// exportPostsHandler は zip を組み立てながら応答に書く（#68）。
// 画像本体（1枚あたり数百KB）は、投稿のまとまりごとに別の goroutine で先に読み（prefetchExportImages）、
// 前のまとまりを送っている間に次のまとまりを DB から読む。
//
// ヘッダーを送る前（本人と投稿の一覧、最初のまとまりの画像）に失敗したら、以前と同じくエラーの応答を返す。
// ヘッダーを送った後に失敗したら、接続を切る（abortExport）。チャンク形式の終端も zip の中央ディレクトリも
// 送らないので、クライアントは途中で切れた応答として扱い、正常に終わった zip として受け取ることはない。
//
// HTTP/1.0 の要求（nginx の proxy_http_version が 1.0 のまま）では、応答の終わりを接続を閉じることで示すので、
// 途中で切っても正常に終わった応答と区別できない。その場合は、以前と同じく全部の画像を読んでから書き始める。
func exportPostsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		// 応答を終えたら（接続を切った場合も）先読みを止める。
		ctx, cancel := context.WithCancel(c.Request().Context())
		defer cancel()
		author, posts, err := loadExportPosts(ctx, h.DB, GetSession(c).UserID)
		if err != nil {
			return err
		}
		chunks := prefetchExportImages(ctx, h.DB, posts)
		if !c.Request().ProtoAtLeast(1, 1) {
			buffered, err := collectExportChunks(chunks)
			if err != nil {
				return err
			}
			chunks = buffered
		}
		chunk, ok := <-chunks
		if ok && chunk.err != nil {
			return chunk.err
		}

		response := c.Response()
		response.Header().Set(echo.HeaderContentType, "application/zip")
		response.Header().Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
		response.Header().Set(echo.HeaderCacheControl, "no-store")
		response.WriteHeader(http.StatusOK)
		writer := zip.NewWriter(response)
		items := make([]exportPostResponse, 0, len(posts))
		for ; ok; chunk, ok = <-chunks {
			if chunk.err != nil {
				abortExport(c, chunk.err)
			}
			for _, post := range chunk.posts {
				data, found := chunk.images[post.ImageID]
				if !found {
					// 以前は image_objects との内部結合だったので、画像のない投稿は含めない。
					continue
				}
				imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
				image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
				if err != nil {
					abortExport(c, err)
				}
				if _, err := image.Write(data); err != nil {
					abortExport(c, err)
				}
				items = append(items, exportPostResponse{createPostResponse: newPostResponse(post, author), ImagePath: imagePath})
			}
		}
		manifest, err := writer.Create("posts.json")
		if err != nil {
			abortExport(c, err)
		}
		if err := json.NewEncoder(manifest).Encode(items); err != nil {
			abortExport(c, err)
		}
		if err := writer.Close(); err != nil {
			abortExport(c, err)
		}
		return nil
	}
}

// collectExportChunks は、先読みのまとまりを全部受け取り、失敗がなければ同じ順に返すチャネルにする。
func collectExportChunks(chunks <-chan exportImageChunk) (<-chan exportImageChunk, error) {
	var all []exportImageChunk
	for chunk := range chunks {
		if chunk.err != nil {
			return nil, chunk.err
		}
		all = append(all, chunk)
	}
	buffered := make(chan exportImageChunk, len(all))
	for _, chunk := range all {
		buffered <- chunk
	}
	close(buffered)
	return buffered, nil
}

// abortExport は、ヘッダーを送った後の失敗で応答を打ち切る。
// エラーを返すと、net/http がチャンク形式を正常に終わらせてしまい、途中までの zip が完全な応答に見える。
// http.ErrAbortHandler は echo の Recover も投げ直すので、net/http が接続をそのまま閉じる。
func abortExport(c *echo.Context, err error) {
	c.Logger().Error("export aborted after the response started", "error", err)
	panic(http.ErrAbortHandler)
}

// exportPostsQuery は、本人の行（is_author が true の1行）と、削除されていない投稿を posted_at, id の順に返す。
// 本人の行の投稿の列と、投稿の行の本人の列には、使わない値を入れる（本人の列を投稿の数だけ繰り返さない）。
var exportPostsQuery = `
  SELECT true AS is_author, ` + userColumnsAs("u", "author") + `,
    ''::varchar AS id, ''::varchar AS author_id, ''::varchar AS posting_window_id, ''::varchar AS image_id,
    ''::text AS caption, ''::text AS visibility, ''::text AS location_kind,
    NULL::numeric AS latitude, NULL::numeric AS longitude, NULL::text AS locality,
    'epoch'::timestamptz AS posted_at, 'epoch'::timestamptz AS on_time_deadline, NULL::timestamptz AS window_ended_at
  FROM users u WHERE u.id = $1 AND u.status = 'active'
  UNION ALL
  SELECT false, ''::varchar, ''::varchar, ''::text, ''::text, ''::text, NULL::varchar, ''::text, ''::text, ''::text,
    ` + postColumns + `
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.author_id = $1 AND p.deleted_at IS NULL
  ORDER BY is_author DESC, posted_at, id
`

// loadExportPosts は本人の行と、削除されていない投稿（posted_at, id の順）を1回のクエリで読む。
// europe と america では DB への1往復が約100ms なので、以前の2往復（getUser と投稿の一覧）を1往復にした。
// 本人が active でなければ、以前の getUser と同じく sql.ErrNoRows を返す。
func loadExportPosts(ctx context.Context, db sqlx.QueryerContext, userID string) (User, []Post, error) {
	var rows []struct {
		IsAuthor bool `db:"is_author"`
		PostWithAuthor
	}
	if err := sqlx.SelectContext(ctx, db, &rows, exportPostsQuery, userID); err != nil {
		return User{}, nil, err
	}
	if len(rows) == 0 || !rows[0].IsAuthor {
		return User{}, nil, sql.ErrNoRows
	}
	author := rows[0].Author
	if err := author.setImageKey(); err != nil {
		return User{}, nil, err
	}
	posts := make([]Post, 0, len(rows)-1)
	for _, row := range rows[1:] {
		posts = append(posts, row.Post)
	}
	return author, posts, nil
}

// exportImageChunk は、投稿のまとまりと、その画像本体（imageID ごと。画像の行がないものは含まない）。
type exportImageChunk struct {
	posts  []Post
	images map[string][]byte
	err    error
}

// prefetchExportImages は、投稿を順にまとまりに分けて画像本体を読み、投稿の順に返す。
//
//   - まとまりは exportImageChunkFirst 件から始めて、exportImageChunkMax 件まで倍々に増やす。
//     最初は小さくして早く送り始め、後ろは大きくして往復の回数を減らす。
//   - 読み込みは最大2つまで同時に進める。europe と america では1回の読み込みに約100ms の往復が付くので、
//     片方の往復の間にもう片方の転送を進める。
//   - 読み終えて受け取られていないまとまりは最大2つ（同時に読んでいるものを含む）。送るのが遅くても、メモリに貯めすぎない。
//
// 失敗したまとまりを返したら、そこで終える（後ろのまとまりは返さない）。
func prefetchExportImages(ctx context.Context, db sqlx.QueryerContext, posts []Post) <-chan exportImageChunk {
	size, maxSize, load := exportImageChunkFirst, exportImageChunkMax, exportLoadImages
	chunks := make(chan exportImageChunk)
	// 読み込みを始めたまとまりの結果を、投稿の順に並べる。容量1なので、受け取る側が待っている1つと合わせて同時に2つまで読む。
	started := make(chan chan exportImageChunk, 1)
	go func() {
		defer close(started)
		for start := 0; start < len(posts); start, size = start+size, min(size*2, maxSize) {
			result := make(chan exportImageChunk, 1)
			select {
			case started <- result:
			case <-ctx.Done():
				return
			}
			go func(chunkPosts []Post) {
				images, err := load(ctx, db, chunkPosts)
				result <- exportImageChunk{posts: chunkPosts, images: images, err: err}
			}(posts[start:min(start+size, len(posts))])
		}
	}()
	go func() {
		defer close(chunks)
		for result := range started {
			var chunk exportImageChunk
			select {
			case chunk = <-result:
			case <-ctx.Done():
				return
			}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
			if chunk.err != nil {
				return
			}
		}
	}()
	return chunks
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
