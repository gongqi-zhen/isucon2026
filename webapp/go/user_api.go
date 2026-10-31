package isureal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type registerRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Region      string `json:"region"`
}

type registerResponse struct {
	SecretKey string              `json:"secretKey"`
	User      sessionUserResponse `json:"user"`
}

type loginRequest struct {
	Username  string `json:"username"`
	SecretKey string `json:"secretKey"`
}

type loginResponse struct {
	Token string              `json:"token"`
	User  sessionUserResponse `json:"user"`
}

type sessionUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	Status      string `json:"status"`
}

type getMeResponse struct {
	sessionUserResponse
	ProfileImageURL *string `json:"profileImageUrl"`
}

type updateMeRequest struct {
	DisplayName *string `json:"displayName"`
	Bio         *string `json:"bio"`
	Region      *string `json:"region"`
}

type updateMeResponse getMeResponse

func newSessionUserResponse(user User) sessionUserResponse {
	return sessionUserResponse{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Bio: user.Bio, Region: user.Region, AccountType: user.AccountType, Status: user.Status,
	}
}

func newGetMeResponse(user User) getMeResponse {
	response := getMeResponse{sessionUserResponse: newSessionUserResponse(user)}
	if user.ProfileImageID != nil {
		url := signedImageURL(user, *user.ProfileImageID)
		response.ProfileImageURL = &url
	}
	return response
}

func registerHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input registerRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) || !validUserText(input.DisplayName, 1, 100) || !validRegion(input.Region) {
			return NewAPIError(http.StatusBadRequest, "invalid_registration")
		}
		secretKey := uuid.NewString()
		hash := sha256.Sum256([]byte(secretKey))
		// ユーザー名は一意。すでにあるユーザー名（同時の登録も含む）は、users_username_key の一意制約違反で 409 にする。
		// 確認と追加と読み直しを、autocommit の1文で行う（#67）。
		var user User
		if err := h.DB.GetContext(c.Request().Context(), &user, `
   INSERT INTO users (id, username, secret_key_hash, display_name, region, search_text) VALUES ($1, $2, $3, $4, $5, $6)
   RETURNING `+strings.Join(userColumns, ", "),
			uuid.NewString(), input.Username, hex.EncodeToString(hash[:]), input.DisplayName, input.Region, userSearchText(input.Username, input.DisplayName)); err != nil {
			if isUniqueViolation(err) {
				return NewAPIError(http.StatusConflict, "username_taken")
			}
			return err
		}
		return c.JSON(http.StatusCreated, registerResponse{SecretKey: secretKey, User: newSessionUserResponse(user)})
	}
}

func loginHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input loginRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		ctx := c.Request().Context()
		hash := sha256.Sum256([]byte(input.SecretKey))
		token, tokenHash := newLoginSessionToken()
		// 行のロック、active と SecretKey の確認、セッションの置き換えを、DB の関数1回の呼び出しで行う（#67）。
		var row struct {
			Result string `db:"out_result"`
			funcUser
		}
		if err := h.DB.GetContext(ctx, &row, `
   SELECT out_result, `+funcUserColumns+`
   FROM isureal_login($1, $2, $3, $4)
  `, input.Username, hex.EncodeToString(hash[:]), uuid.NewString(), tokenHash); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "invalid_credentials":
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		default:
			return fmt.Errorf("unexpected login result: %q", row.Result)
		}
		user := row.user()
		// 以前のセッションを置き換えたので、ほかのプロセスのキャッシュに反映されるまで待つ。
		waitSessionInvalidation(ctx, user.Region, user.ID)
		return c.JSON(http.StatusOK, loginResponse{Token: token, User: newSessionUserResponse(user)})
	}
}

func logoutHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		if err := InvalidateLoginSession(c.Request().Context(), h.DB, session.UserID, session.TokenHash); err != nil {
			return err
		}
		waitSessionInvalidation(c.Request().Context(), session.Region, session.UserID)
		return c.NoContent(http.StatusNoContent)
	}
}

func getMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		user, err := getUser(c.Request().Context(), h.DB, session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, newGetMeResponse(user))
	}
}

func updateMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		var input updateMeRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.DisplayName == nil && input.Bio == nil && input.Region == nil) ||
			(input.DisplayName != nil && !validUserText(*input.DisplayName, 1, 100)) ||
			(input.Bio != nil && !validUserText(*input.Bio, 0, 500)) ||
			(input.Region != nil && !validRegion(*input.Region)) {
			return NewAPIError(http.StatusBadRequest, "invalid_profile")
		}
		ctx := c.Request().Context()
		var searchText *string
		if input.DisplayName != nil {
			normalized := normalizeSearchText(*input.DisplayName)
			searchText = &normalized
		}
		// 指定された列だけを変え、変えた後の行を返す。autocommit の1文で行う（#67）。
		// active でなければ0行になり、変更前と同じく 404 にする（変更前は更新してから getUser で確かめ、ロールバックしていた）。
		// 表示名を変えるとき、username は登録時の検証で正規化済みの形をしているので、そのまま使える。そうでなければ NULL にして検索時に Go で判定する。
		var user User
		err := h.DB.GetContext(ctx, &user, `
   UPDATE users SET
     display_name = COALESCE($2::text, display_name),
     search_text = CASE WHEN $2::text IS NULL THEN search_text
                        WHEN username ~ '^[a-z0-9_.-]+$' THEN username || E'\n' || $3::text END,
     bio = COALESCE($4::text, bio),
     region = COALESCE($5::text, region),
     updated_at = clock_timestamp()
   WHERE id = $1 AND status = 'active'
   RETURNING `+strings.Join(userColumns, ", "),
			session.UserID, input.DisplayName, searchText, input.Bio, input.Region)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		if input.Region != nil && *input.Region != session.Region {
			// 移る前のリージョンのプロセスにキャッシュがあれば、使われなくなるまで待つ。
			waitSessionInvalidation(ctx, session.Region, session.UserID)
		}
		return c.JSON(http.StatusOK, updateMeResponse(newGetMeResponse(user)))
	}
}
