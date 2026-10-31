package isureal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getMemoriesRequest struct {
	Month          string     `query:"month"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type memoryPostResponse struct {
	createPostResponse
	RealMojis []replaceRealMojiResponse `json:"realmojis"`
	Comments  []createCommentResponse   `json:"comments"`
}

type getMemoriesResponse struct {
	Items   []memoryPostResponse `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

type getMemoriesByDateRequest struct {
	Date           string     `param:"date"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type getMemoriesByDateResponse getMemoriesResponse

func getMemoriesHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

func getMemoriesByDateHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesByDateRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse(time.DateOnly, input.Date)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

// memoryPostRow は listMemories の1行。RealMoji とコメントは、投稿ごとに JSON の配列で受け取る。
type memoryPostRow struct {
	PostWithAuthor
	RealMojisJSON []byte `db:"realmojis_json"`
	CommentsJSON  []byte `db:"comments_json"`
}

// memoryUserJSON は、JSON で受け取る RealMoji とコメントの利用者。列は userColumns と同じ。
type memoryUserJSON struct {
	ID             string  `json:"id"`
	Username       string  `json:"username"`
	SecretKeyHash  string  `json:"secret_key_hash"`
	DisplayName    string  `json:"display_name"`
	Bio            string  `json:"bio"`
	ProfileImageID *string `json:"profile_image_id"`
	Region         string  `json:"region"`
	AccountType    string  `json:"account_type"`
	Status         string  `json:"status"`
}

func (u memoryUserJSON) user() (User, error) {
	user := User{
		ID: u.ID, Username: u.Username, SecretKeyHash: u.SecretKeyHash, DisplayName: u.DisplayName, Bio: u.Bio,
		ProfileImageID: u.ProfileImageID, Region: u.Region, AccountType: u.AccountType, Status: u.Status,
	}
	return user, user.setImageKey()
}

// memoryInteractionJSON は、JSON で受け取る RealMoji かコメントの1件。
// created_at は、pgx と同じ値（マイクロ秒、time.Local）に戻すため、エポックからのマイクロ秒で受け取る。
type memoryInteractionJSON struct {
	ID              string         `json:"id"`
	ReactionType    string         `json:"reaction_type"`
	ImageID         string         `json:"image_id"`
	Body            string         `json:"body"`
	CreatedAtMicros int64          `json:"created_at_us"`
	User            memoryUserJSON `json:"user"`
}

// memoryUserJSONObject は、別名 alias の users の列を memoryUserJSON の形の JSON にする SQL。
func memoryUserJSONObject(alias string) string {
	pairs := make([]string, len(userColumns))
	for i, column := range userColumns {
		pairs[i] = "'" + column + "', " + alias + "." + column
	}
	return "json_build_object(" + strings.Join(pairs, ", ") + ")"
}

// listMemories は自分の投稿と、それぞれの RealMoji とコメント（各最大50件）を1回のクエリで読む。
// europe と america では DB への1往復が約100ms なので、以前の3往復（投稿、RealMoji、コメント）を1往復にした。
// 並び順と件数の上限は以前と同じ：投稿は posted_at DESC, id DESC で51件読み、50件を超えたら hasMore。
// RealMoji は現在のもの（差し替え・削除されていない）を created_at DESC, id DESC、
// コメントは削除されていないものを created_at, id の順に、どちらも active な利用者のものだけを投稿ごとに先頭50件。
func listMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	var rows []memoryPostRow
	if err := sqlx.SelectContext(ctx, db, &rows, `
  WITH mp AS (
    SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
    FROM posts p
    JOIN posting_windows w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE p.deleted_at IS NULL
      AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
      AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
    ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
  )
  SELECT mp.*,
    COALESCE((
      SELECT json_agg(x.obj ORDER BY x.created_at DESC, x.id DESC)
      FROM (
        SELECT r.created_at, r.id, json_build_object(
          'id', r.id, 'reaction_type', r.reaction_type, 'image_id', r.image_id,
          'created_at_us', (extract(epoch FROM r.created_at) * 1000000)::bigint,
          'user', `+memoryUserJSONObject("ru")+`) AS obj
        FROM realmojis r JOIN users ru ON ru.id = r.reactor_id AND ru.status = 'active'
        WHERE r.post_id = mp.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
        ORDER BY r.created_at DESC, r.id DESC LIMIT 50
      ) x
    ), '[]') AS realmojis_json,
    COALESCE((
      SELECT json_agg(x.obj ORDER BY x.created_at, x.id)
      FROM (
        SELECT c.created_at, c.id, json_build_object(
          'id', c.id, 'body', c.body,
          'created_at_us', (extract(epoch FROM c.created_at) * 1000000)::bigint,
          'user', `+memoryUserJSONObject("cu")+`) AS obj
        FROM comments c JOIN users cu ON cu.id = c.author_id AND cu.status = 'active'
        WHERE c.post_id = mp.id AND c.deleted_at IS NULL
        ORDER BY c.created_at, c.id LIMIT 50
      ) x
    ), '[]') AS comments_json
  FROM mp
  ORDER BY mp.posted_at DESC, mp.id DESC
 `, userID, start, end, beforeAt, beforeID); err != nil {
		return response, err
	}
	response.HasMore = len(rows) > 50
	if response.HasMore {
		rows = rows[:50]
	}
	items := make([]memoryPostResponse, 0, len(rows))
	for _, row := range rows {
		if err := row.Author.setImageKey(); err != nil {
			return response, err
		}
		item := memoryPostResponse{
			createPostResponse: newPostResponse(row.Post, row.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		}
		var reactions, comments []memoryInteractionJSON
		if err := json.Unmarshal(row.RealMojisJSON, &reactions); err != nil {
			return response, err
		}
		if err := json.Unmarshal(row.CommentsJSON, &comments); err != nil {
			return response, err
		}
		for _, reaction := range reactions {
			reactor, err := reaction.User.user()
			if err != nil {
				return response, err
			}
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(RealMoji{
				ID: reaction.ID, PostID: row.ID, ReactorID: reactor.ID, ImageID: reaction.ImageID,
				ReactionType: reaction.ReactionType, CreatedAt: time.UnixMicro(reaction.CreatedAtMicros),
			}, reactor))
		}
		for _, comment := range comments {
			author, err := comment.User.user()
			if err != nil {
				return response, err
			}
			item.Comments = append(item.Comments, newCommentResponse(Comment{
				ID: comment.ID, PostID: row.ID, AuthorID: author.ID, Body: comment.Body,
				CreatedAt: time.UnixMicro(comment.CreatedAtMicros),
			}, author))
		}
		items = append(items, item)
	}
	response.Items = items
	return response, nil
}
