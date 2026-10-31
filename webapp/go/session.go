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
}

func GetSession(c *echo.Context) *Session {
	sess, ok := c.Get("session").(*Session)
	if !ok {
		return nil
	}
	return sess
}

func getSessionFromHeader(c *echo.Context, h *HandlerContext) (*Session, error) {
	authHeader := c.Request().Header.Get(echo.HeaderAuthorization)
	if authHeader == "" {
		return nil, nil
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.Logger().Warn("Invalid authorization header format(not started with Bearer)")
		return nil, NewAPIError(http.StatusUnauthorized, "invalid_session")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	tokenHashBytes := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	sessions.init(h.Config.Region, dbSessionCacheSource{db: h.DB})
	if cached, ok := sessions.get(tokenHash); ok {
		return cached, nil
	}
	var session Session
	sentAt := time.Now()
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

func InvalidateLoginSession(db *sqlx.DB, userID string, tokenHash string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var lockedUserID string
	if err := tx.Get(&lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
		return fmt.Errorf("failed to lock user: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE login_sessions
		SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
		WHERE user_id = $1 AND active = true AND token_hash = $2	
	`, userID, tokenHash); err != nil {
		return fmt.Errorf("failed to invalidate sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
func CreateLoginSession(db *sqlx.DB, userID string) (*Session, error) {
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	var lockedUserID string
	if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID); err != nil {
		return nil, fmt.Errorf("failed to lock user: %w", err)
	}
	session, err := createLoginSessionTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	return session, nil
}

// createLoginSessionTx は呼び出し元が users の行を FOR NO KEY UPDATE でロックし、active であることを確かめた後に呼ぶ。
// europe と america から DB への1往復は約100ms なので、既存セッションの無効化と新しいセッションの追加を1つの文で行う。
// CTE の UPDATE は本体の INSERT と同じスナップショットで動くので、追加した行は無効化されない。
func createLoginSessionTx(ctx context.Context, tx *sqlx.Tx, userID string) (*Session, error) {
	randomToken := uuid.NewString()
	tokenHashBytes := sha256.Sum256([]byte(randomToken))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	sessionID := uuid.NewString()
	var session Session
	if err := tx.GetContext(ctx, &session, `
  WITH invalidated AS (
    UPDATE login_sessions
    SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
    WHERE user_id = $2 AND active = true
  )
  INSERT INTO login_sessions (id, user_id, token_hash)
  VALUES ($1, $2, $3)
  RETURNING user_id, token_hash, created_at
 `, sessionID, userID, tokenHash); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	session.Token = randomToken
	return &session, nil
}
