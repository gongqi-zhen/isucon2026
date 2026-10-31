package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type Session struct {
	Region    string    `db:"region" json:"-"`
	Token     string    `db:"-" json:"-"`
	UserID    string    `db:"user_id"`
	TokenHash string    `db:"token_hash"`
	CreatedAt time.Time `db:"created_at"`
	// User は、セッションと同じクエリで読んだ利用者の行（セッションをキャッシュするリージョンだけ。#58）。
	// nil のときは、使う側が DB を引く。
	User *User `db:"-" json:"-"`
	// userFingerprint は、User を読んだときの行の md5（sessionUserFingerprint）。
	userFingerprint string
}

func GetSession(c *echo.Context) *Session {
	sess, ok := c.Get("session").(*Session)
	if !ok {
		return nil
	}
	return sess
}

// sessionWithUserQuery は、セッションと利用者の行を一緒に読むクエリ（キャッシュするリージョン用、#58）。起動時に1回だけ組み立てる（#112）。
var sessionWithUserQuery = `
		SELECT s.user_id, s.token_hash, s.created_at, u.region, ` + userColumnsAs("u", "user") + `, ` + sessionUserFingerprint + ` AS fingerprint
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.active = true AND u.status = 'active'
	`

func getSessionFromHeader(c *echo.Context, h *HandlerContext) (*Session, error) {
	authHeader := c.Request().Header.Get(echo.HeaderAuthorization)
	if authHeader == "" {
		return nil, nil
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, NewAPIError(http.StatusUnauthorized, "invalid_session")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	tokenHashBytes := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	sessions.init(h.Config.Region, dbSessionCacheSource{db: h.sessionCacheDB()})
	if cached, ok := sessions.get(tokenHash); ok {
		return cached, nil
	}
	if !sessions.caching() {
		var session Session
		err := h.DB.GetContext(c.Request().Context(), &session, `
		SELECT s.user_id, s.token_hash, s.created_at, u.region
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.active = true AND u.status = 'active'
	`, tokenHash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get session: %w", err)
		}
		return &session, nil
	}
	// キャッシュするリージョンでは、利用者の行も同じクエリで読み、一緒にキャッシュに入れる（#58）。
	var row struct {
		Session
		User        User   `db:"user"`
		Fingerprint string `db:"fingerprint"`
	}
	sentAt := time.Now()
	err := h.DB.GetContext(c.Request().Context(), &row, sessionWithUserQuery, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if err := row.User.setImageKey(); err != nil {
		return nil, err
	}
	session := row.Session
	session.User, session.userFingerprint = &row.User, row.Fingerprint
	sessions.put(session, sentAt)
	return &session, nil
}

func SessionMiddleware(handlerContext *HandlerContext, authRequired bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			session, err := getSessionFromHeader(c, handlerContext)
			if err != nil {
				return err
			}
			if session == nil && authRequired {
				return NewAPIError(http.StatusUnauthorized, "invalid_session")
			}
			c.Set("session", session)
			return next(c)
		}
	}
}

// InvalidateLoginSession は、ログアウトしたトークンのセッションを無効にする。autocommit の1文で行う（#84）。
// users の行はロックしない。以前はロックしたまま往復していたので、europe と america では約200ms、
// その利用者の投稿へのコメントや RealMoji、友達関係の操作を待たせていた。
// 同じ利用者のログイン（isureal_login）や退会と同時に動いても、同じ login_sessions の行への UPDATE どうしで直列になる。
// 後の UPDATE は先のコミットを待ってから active を確かめ直すので、どちらの順でもそのトークンは無効になる（違うのは reason だけ）。
func InvalidateLoginSession(ctx context.Context, db *sqlx.DB, userID string, tokenHash string) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE login_sessions
		SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
		WHERE user_id = $1 AND active = true AND token_hash = $2
	`, userID, tokenHash); err != nil {
		return fmt.Errorf("failed to invalidate sessions: %w", err)
	}
	return nil
}

// newLoginSessionToken は、新しいセッションのトークンと、DB に保存するそのハッシュを返す。
func newLoginSessionToken() (token, tokenHash string) {
	token = uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(hash[:])
}
