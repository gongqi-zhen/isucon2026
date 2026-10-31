package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

// users.search_text に保存する値。SQL 側の isureal_search_text と同じ形式にする。
func userSearchText(username, displayName string) string {
	return normalizeSearchText(username) + "\n" + normalizeSearchText(displayName)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

const searchUsersBatchSize = 200

type searchUserRow struct {
	User
	SearchText *string `db:"search_text"`
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		query := normalizeSearchText(input.Q)
		pattern := "%" + likeEscaper.Replace(query) + "%"
		// search_text による絞り込みは候補を減らすためだけに使い、一致の判定は従来どおり Go 側で行う。
		// search_text が NULL のユーザーは常に候補に含める。
		items := make([]relationshipUserResponse, 0)
		var backfillIDs, backfillUsernames, backfillDisplayNames, backfillTexts []string
		afterUsername, afterID := input.BeforeUsername, input.BeforeID
	scan:
		for {
			var users []searchUserRow
			if err := h.DB.SelectContext(ctx, &users, `
    SELECT `+strings.Join(userColumns, ", ")+`, search_text FROM users u
    WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3))
      AND (search_text LIKE $4 OR search_text IS NULL)
      AND NOT EXISTS (
        SELECT 1 FROM block_relations b
        WHERE (b.blocker_id=$1 AND b.blocked_id=u.id) OR (b.blocker_id=u.id AND b.blocked_id=$1)
      )
    ORDER BY username, id LIMIT $5
   `, viewer, afterUsername, afterID, pattern, searchUsersBatchSize); err != nil {
				return err
			}
			for _, row := range users {
				user := row.User
				if row.SearchText == nil {
					backfillIDs = append(backfillIDs, user.ID)
					backfillUsernames = append(backfillUsernames, user.Username)
					backfillDisplayNames = append(backfillDisplayNames, user.DisplayName)
					backfillTexts = append(backfillTexts, userSearchText(user.Username, user.DisplayName))
				}
				if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
					continue
				}
				if err := user.setImageKey(); err != nil {
					return err
				}
				if user.AccountType != "certified_public" {
					user.Bio = ""
				}
				items = append(items, newRelationshipUserResponse(user))
				if len(items) == 51 {
					break scan
				}
			}
			if len(users) < searchUsersBatchSize {
				break
			}
			last := users[len(users)-1]
			afterUsername, afterID = last.Username, last.ID
		}
		// SQL で正規化できなかったユーザーの search_text を Go で埋めておく（失敗しても検索結果には影響しない）。
		if len(backfillIDs) > 0 {
			if _, err := h.DB.ExecContext(ctx, `
    UPDATE users u SET search_text = v.search_text
    FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS v(id, username, display_name, search_text)
    WHERE u.id = v.id AND u.search_text IS NULL AND u.username = v.username AND u.display_name = v.display_name
   `, backfillIDs, backfillUsernames, backfillDisplayNames, backfillTexts); err != nil {
				c.Logger().Error("failed to backfill search_text", "error", err)
			}
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		user, err := getUser(ctx, h.DB, input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		blocked, err := isBlocked(ctx, h.DB, viewer, user.ID)
		if err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		friends, err := getFriendIDs(ctx, h.DB, viewer)
		if err != nil {
			return err
		}
		if user.AccountType != "certified_public" && !slices.Contains(friends, user.ID) {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
