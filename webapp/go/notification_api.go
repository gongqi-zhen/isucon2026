package isureal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type getNotificationsResponse struct {
	ID              string    `json:"id" db:"id"`
	PostingWindowID string    `json:"postingWindowId" db:"posting_window_id"`
	Region          string    `json:"region" db:"region"`
	CreatedAt       time.Time `json:"createdAt" db:"created_at"`
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		session := GetSession(c)
		userID := session.UserID
		// 接続した時刻を読むクエリより前に購読する。
		// 返す通知は created_at > connectedAt のものだけで、その投稿期間を開始したトランザクションは connectedAt より後に確定する。
		// ハブがそれを検知するのは確定より後、つまり購読より後なので、この接続は必ず知らされる（changed か delivered）。
		// そのため、以前は接続ごとに最初に1回引いていた通知の確認を省ける。
		hub := getNotificationHub(h)
		waiter := hub.subscribe(userID, session.Region)
		defer hub.unsubscribe(waiter)
		// 接続した時刻と、受け取る人が active であることを1回の往復で確かめる（以前は clock_timestamp と getUser の2往復）。
		var connection struct {
			ConnectedAt time.Time `db:"connected_at"`
			Region      *string   `db:"region"`
		}
		if err := h.DB.GetContext(ctx, &connection, `
   SELECT clock_timestamp() AS connected_at,
          (SELECT region FROM users WHERE id = $1 AND status = 'active') AS region
  `, userID); err != nil {
			return err
		}
		response := c.Response()
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(response)
		if err := controller.Flush(); err != nil {
			return nil
		}
		if connection.Region == nil || *connection.Region != session.Region {
			return nil
		}
		connectedAt := connection.ConnectedAt
		waiter.setConnectedAt(connectedAt)
		send := func(notification getNotificationsResponse) error {
			data, err := json.Marshal(notification)
			if err != nil {
				c.Logger().Error("failed to encode notification", "error", err)
				return nil
			}
			if _, err := fmt.Fprintf(response, "data: %s\n\n", data); err != nil {
				return nil
			}
			_ = controller.Flush()
			return nil
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-waiter.gone:
				return nil
			case notification := <-waiter.delivered:
				return send(notification)
			case <-waiter.changed:
			}
			// ハブがまとめて引けなかったとき（購読した直後、クエリの失敗）は、以前と同じくこの接続で引く。
			var notification getNotificationsResponse
			err := h.DB.GetContext(ctx, &notification, `
    SELECT n.id, n.posting_window_id, w.region, n.created_at
    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
    WHERE n.user_id = $1 AND n.created_at > $2 AND w.ended_at IS NULL
    ORDER BY n.created_at, n.id LIMIT 1
   `, userID, connectedAt)
			if err == nil {
				return send(notification)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				if ctx.Err() == nil {
					c.Logger().Error("failed to get notification", "error", err)
				}
				return nil
			}
		}
	}
}
